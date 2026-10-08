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
	if got := a.Acquire(1); !got {
		t.Fatalf("a.Acquire(1) on an empty pool = %v, want true", got)
	}
	if gotB, gotC := b.Acquire(1), c.Acquire(1); gotB || gotC {
		t.Fatalf("Acquire on a full pool = (b=%v, c=%v), want (false, false)", gotB, gotC)
	}
	if got := p.Waiting(); fmt.Sprint(got) != "[b c]" {
		t.Fatalf("waiting %v, want [b c]", got)
	}

	a.Release(1)
	if got := w.String(); got != "b" {
		t.Fatalf("woken %q after a's release, want %q: the head of the queue", got, "b")
	}
	if got := c.Acquire(1); got {
		t.Fatalf("c.Acquire(1) ahead of b = %v, want false", got)
	}
	if got := a.Acquire(1); got {
		t.Fatalf("a.Acquire(1) back ahead of b = %v, want false", got)
	}
	if got := b.Acquire(1); !got {
		t.Fatalf("b.Acquire(1), the head of the queue, = %v, want true", got)
	}
	if got := p.Waiting(); fmt.Sprint(got) != "[c a]" {
		t.Fatalf("waiting %v, want [c a]: a queued up behind c", got)
	}
	if got := p.InUse(); got != 1 {
		t.Fatalf("in use %d, want 1", got)
	}

	b.Release(1)
	if got := w.String(); got != "b c" {
		t.Fatalf("woken %q, want %q (b then c)", got, "b c")
	}
	if got := c.Acquire(1); !got {
		t.Fatalf("c.Acquire(1) on its turn = %v, want true", got)
	}
	c.Release(1)
	if got := w.String(); got != "b c a" {
		t.Fatalf("woken %q, want %q (b, c, a)", got, "b c a")
	}
	if got := a.Acquire(1); !got {
		t.Fatalf("a.Acquire(1) on its turn = %v, want true", got)
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
	if got := w.String(); got != "b" {
		t.Fatalf("woken %q, want %q", got, "b")
	}
	// b's wake pass finds nothing to dispatch and ends without a claim.
	b.Pass()
	if got := p.Waiting(); fmt.Sprint(got) != "[c]" {
		t.Fatalf("waiting %v after b's idle pass, want [c]", got)
	}
	if got := w.String(); got != "b c" {
		t.Fatalf("woken %q, want %q (c woken as the new head)", got, "b c")
	}
	if got := c.Acquire(1); !got {
		t.Fatalf("c.Acquire(1) on the slot b did not use = %v, want true", got)
	}
}

// The head is woken when the pool can fill what it asked for, not on every
// release: a fan-out waiting for two slots is not woken for one.
func TestSharedPoolWakesTheHeadWhenItCanBeServed(t *testing.T) {
	p := NewSharedPool(3)
	var w woken
	a, b := w.member(p, "a"), w.member(p, "b")
	if got := a.Acquire(3); !got {
		t.Fatalf("a.Acquire(3) of a fan-out the pool holds = %v, want true", got)
	}
	if got := b.Acquire(2); got {
		t.Fatalf("b.Acquire(2) on a full pool = %v, want false", got)
	}
	a.Release(1)
	if got := w.String(); got != "" {
		t.Fatalf("woken %q after one slot freed, want nobody: b asked for two", got)
	}
	a.Release(1)
	if got := w.String(); got != "b" {
		t.Fatalf("woken %q after two slots freed, want %q", got, "b")
	}
	if got := b.Acquire(2); !got {
		t.Fatalf("b.Acquire(2), the two slots it waited for, = %v, want true", got)
	}
	if got := p.InUse(); got != 3 {
		t.Fatalf("in use %d, want 3", got)
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
	gotA1, gotB1, gotB2, gotC1 := a.Acquire(1), b.Acquire(1), b.Acquire(1), c.Acquire(1)
	if !gotA1 || !gotB1 || gotB2 || gotC1 {
		t.Fatalf("setup claims = (a=%v, b1=%v, b2=%v, c=%v), want (true, true, false, false)", gotA1, gotB1, gotB2, gotC1)
	}
	b.Leave()
	if inUse, waiting := p.InUse(), fmt.Sprint(p.Waiting()); inUse != 2 || waiting != "[c]" {
		t.Fatalf("after b.Leave(): inUse=%d waiting=%s, want inUse=2 waiting=[c]", inUse, waiting)
	}
	b.Release(1)
	gotWoken, gotAcquire := w.String(), c.Acquire(1)
	if gotWoken != "c" || !gotAcquire {
		t.Fatalf("after b.Release(1): woken=%q c.Acquire(1)=%v, want woken=%q acquire=true", gotWoken, gotAcquire, "c")
	}
	c.Release(1)
	a.Release(1)
	if got := p.InUse(); got != 0 {
		t.Fatalf("in use after every release = %d, want 0", got)
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
						t.Errorf("member %d: waiting for a slot starved past its 5s deadline", i)
						return
					}
				}
				member.Pass()
				if used := p.InUse(); used < 1 || used > p.Size() {
					t.Errorf("member %d: in use = %d, want between 1 and %d", i, used, p.Size())
				}
				member.Release(1)
			}
		})
	}
	wg.Wait()
	if inUse, waiting := p.InUse(), p.Waiting(); inUse != 0 || len(waiting) != 0 {
		t.Fatalf("after every pass completed: inUse=%d waiting=%v, want 0, none", inUse, waiting)
	}
}
