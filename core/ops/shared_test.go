package ops

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// woken records which member the pool woke, in order.
type woken struct {
	mu    sync.Mutex
	names []string
}

func (w *woken) member(p *SharedPool, name string) *Member {
	return p.Join(name, func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		w.names = append(w.names, name)
	})
}

func (w *woken) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return strings.Join(w.names, " ")
}

// A freed slot goes to the scheduler that has waited longest, not to the
// first to ask: the refused queue up, only its head may claim, and a
// scheduler that got its turn queues up again behind the others.
func TestSharedPoolServesTheQueueInOrder(t *testing.T) {
	p := NewSharedPool(1)
	var w woken
	a, b, c := w.member(p, "a"), w.member(p, "b"), w.member(p, "c")
	if !a.Acquire(1) {
		t.Fatal("an empty pool refused the first claim")
	}
	if b.Acquire(1) || c.Acquire(1) {
		t.Fatal("a full pool granted a claim")
	}
	if got := p.Waiting(); fmt.Sprint(got) != "[b c]" {
		t.Fatalf("waiting %v, want [b c]", got)
	}

	a.Release(1)
	if w.String() != "b" {
		t.Fatalf("woken %q after a's release, want b: the head of the queue", w.String())
	}
	if c.Acquire(1) {
		t.Fatal("c claimed the slot ahead of b")
	}
	if a.Acquire(1) {
		t.Fatal("a claimed the slot back ahead of b")
	}
	if !b.Acquire(1) {
		t.Fatal("b, the head of the queue, was refused a free slot")
	}
	if got := p.Waiting(); fmt.Sprint(got) != "[c a]" {
		t.Fatalf("waiting %v, want [c a]: a queued up behind c", got)
	}
	if p.InUse() != 1 {
		t.Fatalf("in use %d, want 1", p.InUse())
	}

	b.Release(1)
	if w.String() != "b c" {
		t.Fatalf("woken %q, want b then c", w.String())
	}
	if !c.Acquire(1) {
		t.Fatal("c was refused its turn")
	}
	c.Release(1)
	if w.String() != "b c a" {
		t.Fatalf("woken %q, want b, c, a", w.String())
	}
	if !a.Acquire(1) {
		t.Fatal("a was refused its turn")
	}
	if got := p.Waiting(); len(got) != 0 {
		t.Fatalf("waiting %v, want nobody", got)
	}
}

// A scheduler whose pass ends without a refusal leaves the queue, and the
// turn it held passes on: a slot is never kept for a project with nothing
// to run in it.
func TestSharedPoolAPassWithoutARefusalLeavesTheQueue(t *testing.T) {
	p := NewSharedPool(1)
	var w woken
	a, b, c := w.member(p, "a"), w.member(p, "b"), w.member(p, "c")
	a.Acquire(1)
	b.Acquire(1)
	c.Acquire(1)
	// The pass in which b was refused ends: b stays queued.
	b.Pass()
	if got := p.Waiting(); fmt.Sprint(got) != "[b c]" {
		t.Fatalf("waiting %v after the refused pass, want [b c]", got)
	}
	a.Release(1)
	if w.String() != "b" {
		t.Fatalf("woken %q, want b", w.String())
	}
	// b's wake pass finds nothing to dispatch and ends without a claim.
	b.Pass()
	if got := p.Waiting(); fmt.Sprint(got) != "[c]" {
		t.Fatalf("waiting %v after b's idle pass, want [c]", got)
	}
	if w.String() != "b c" {
		t.Fatalf("woken %q, want c woken as the new head", w.String())
	}
	if !c.Acquire(1) {
		t.Fatal("c was refused the slot b did not use")
	}
}

// The head is woken when the pool can fill what it asked for, not on every
// release: a fan-out waiting for two slots is not woken for one.
func TestSharedPoolWakesTheHeadWhenItCanBeServed(t *testing.T) {
	p := NewSharedPool(3)
	var w woken
	a, b := w.member(p, "a"), w.member(p, "b")
	if !a.Acquire(3) {
		t.Fatal("a fan-out the pool holds was refused")
	}
	if b.Acquire(2) {
		t.Fatal("a full pool granted a claim")
	}
	a.Release(1)
	if w.String() != "" {
		t.Fatalf("woken %q after one slot freed, want nobody: b asked for two", w.String())
	}
	a.Release(1)
	if w.String() != "b" {
		t.Fatalf("woken %q after two slots freed, want b", w.String())
	}
	if !b.Acquire(2) {
		t.Fatal("b was refused the two slots it waited for")
	}
	if p.InUse() != 3 {
		t.Fatalf("in use %d, want 3", p.InUse())
	}
}

// A pool of no slots would refuse everything forever.
func TestSharedPoolHasAtLeastOneSlot(t *testing.T) {
	if got := NewSharedPool(0).Size(); got != 1 {
		t.Fatalf("size %d, want 1", got)
	}
	if got := NewSharedPool(4).Size(); got != 4 {
		t.Fatalf("size %d, want 4", got)
	}
}

func TestSharedPoolLeavePreservesInFlightAndWakesNext(t *testing.T) {
	p := NewSharedPool(2)
	var w woken
	a, b, c := w.member(p, "a"), w.member(p, "b"), w.member(p, "c")
	if !a.Acquire(1) || !b.Acquire(1) || b.Acquire(1) || c.Acquire(1) {
		t.Fatal("setup claims")
	}
	b.Leave()
	if p.InUse() != 2 || fmt.Sprint(p.Waiting()) != "[c]" {
		t.Fatal("leave released active work or kept pending claim")
	}
	b.Release(1)
	if w.String() != "c" || !c.Acquire(1) {
		t.Fatal("next member not served")
	}
	c.Release(1)
	a.Release(1)
	if p.InUse() != 0 {
		t.Fatal("slots leaked")
	}
}

func TestSharedPoolConcurrentPasses(t *testing.T) {
	p := NewSharedPool(3)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			wake := NewWake()
			member := p.Join(fmt.Sprint(i), wake.Signal)
			defer member.Leave()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			for range 50 {
				for !member.Acquire(1) {
					member.Pass()
					select {
					case <-wake:
					case <-ctx.Done():
						t.Error("waiting member starved")
						return
					}
				}
				member.Pass()
				if used := p.InUse(); used < 1 || used > p.Size() {
					t.Errorf("in use=%d", used)
				}
				member.Release(1)
			}
		})
	}
	wg.Wait()
	if p.InUse() != 0 || len(p.Waiting()) != 0 {
		t.Fatal("completed passes left claims behind")
	}
}
