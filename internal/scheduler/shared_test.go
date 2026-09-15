package scheduler

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// woken records which member the pool woke, in order.
type woken struct {
	mu    sync.Mutex
	names []string
}

func (w *woken) member(p *SharedPool, name string) *sharedMember {
	return p.join(name, func() {
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
	if !a.acquire(1) {
		t.Fatal("an empty pool refused the first claim")
	}
	if b.acquire(1) || c.acquire(1) {
		t.Fatal("a full pool granted a claim")
	}
	if got := p.Waiting(); fmt.Sprint(got) != "[b c]" {
		t.Fatalf("waiting %v, want [b c]", got)
	}

	a.release(1)
	if w.String() != "b" {
		t.Fatalf("woken %q after a's release, want b: the head of the queue", w.String())
	}
	if c.acquire(1) {
		t.Fatal("c claimed the slot ahead of b")
	}
	if a.acquire(1) {
		t.Fatal("a claimed the slot back ahead of b")
	}
	if !b.acquire(1) {
		t.Fatal("b, the head of the queue, was refused a free slot")
	}
	if got := p.Waiting(); fmt.Sprint(got) != "[c a]" {
		t.Fatalf("waiting %v, want [c a]: a queued up behind c", got)
	}
	if p.InUse() != 1 {
		t.Fatalf("in use %d, want 1", p.InUse())
	}

	b.release(1)
	if w.String() != "b c" {
		t.Fatalf("woken %q, want b then c", w.String())
	}
	if !c.acquire(1) {
		t.Fatal("c was refused its turn")
	}
	c.release(1)
	if w.String() != "b c a" {
		t.Fatalf("woken %q, want b, c, a", w.String())
	}
	if !a.acquire(1) {
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
	a.acquire(1)
	b.acquire(1)
	c.acquire(1)
	// The pass in which b was refused ends: b stays queued.
	b.pass()
	if got := p.Waiting(); fmt.Sprint(got) != "[b c]" {
		t.Fatalf("waiting %v after the refused pass, want [b c]", got)
	}
	a.release(1)
	if w.String() != "b" {
		t.Fatalf("woken %q, want b", w.String())
	}
	// b's wake pass finds nothing to dispatch and ends without a claim.
	b.pass()
	if got := p.Waiting(); fmt.Sprint(got) != "[c]" {
		t.Fatalf("waiting %v after b's idle pass, want [c]", got)
	}
	if w.String() != "b c" {
		t.Fatalf("woken %q, want c woken as the new head", w.String())
	}
	if !c.acquire(1) {
		t.Fatal("c was refused the slot b did not use")
	}
}

// The head is woken when the pool can fill what it asked for, not on every
// release: a fan-out waiting for two slots is not woken for one.
func TestSharedPoolWakesTheHeadWhenItCanBeServed(t *testing.T) {
	p := NewSharedPool(3)
	var w woken
	a, b := w.member(p, "a"), w.member(p, "b")
	if !a.acquire(3) {
		t.Fatal("a fan-out the pool holds was refused")
	}
	if b.acquire(2) {
		t.Fatal("a full pool granted a claim")
	}
	a.release(1)
	if w.String() != "" {
		t.Fatalf("woken %q after one slot freed, want nobody: b asked for two", w.String())
	}
	a.release(1)
	if w.String() != "b" {
		t.Fatalf("woken %q after two slots freed, want b", w.String())
	}
	if !b.acquire(2) {
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

// A single-project scheduler shares nothing.
func TestSchedulerWithoutASharedPool(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	if h.sched.SharedPool() != nil {
		t.Fatal("a scheduler built without Deps.Shared has a shared pool")
	}
}

// timeline records the developer dispatches of several schedulers in the
// order they happened.
type timeline struct {
	mu      sync.Mutex
	entries []string
}

// record wraps the harness's fake gh so every label change to
// bees:in-progress is entered under name.
func (tl *timeline) record(name string) func(*Deps) {
	return func(d *Deps) {
		prev := d.GitHub.Exec
		d.GitHub.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
			for i, a := range args {
				if a == "--add-label" && i+1 < len(args) && args[i+1] == "bees:in-progress" {
					tl.mu.Lock()
					tl.entries = append(tl.entries, name+":"+args[2])
					tl.mu.Unlock()
				}
			}
			return prev(ctx, args...)
		}
	}
}

func (tl *timeline) String() string {
	tl.mu.Lock()
	defer tl.mu.Unlock()
	return strings.Join(tl.entries, " ")
}

// Two schedulers sharing a one-slot pool never run a developer at the same
// time, and the freed slot goes round: the second project gets it when the
// first project's worker finishes, although the first has another ready
// issue and is the one woken by its own worker. The clock is frozen and
// each polls once an hour, so every dispatch after the first is a wake.
func TestSchedulersShareThePoolRoundRobin(t *testing.T) {
	now := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	pool := NewSharedPool(1)
	var tl timeline
	share := func(d *Deps) { d.Shared = pool }
	a := newHarnessAt(t, wakeTOML+rolesOffTOML, now, share, tl.record("a"))
	b := newHarnessAt(t, strings.Replace(wakeTOML, "acme/widgets", "acme/gadgets", 1)+rolesOffTOML, now, share, tl.record("b"))
	if a.sched.SharedPool() != pool || b.sched.SharedPool() != pool {
		t.Fatal("the schedulers were not built on the shared pool")
	}
	base := now.Add(-24 * time.Hour)
	for _, h := range []*harness{a, b} {
		seedReady(h, 1, "s", base)
		seedReady(h, 2, "s", base.Add(time.Hour))
	}

	// Never more than the pool's one worker across both projects.
	stopWatch := make(chan struct{})
	overlap := make(chan string, 1)
	go func() {
		for {
			select {
			case <-stopWatch:
				return
			default:
			}
			a.sched.mu.Lock()
			na := len(a.sched.owned)
			a.sched.mu.Unlock()
			b.sched.mu.Lock()
			nb := len(b.sched.owned)
			b.sched.mu.Unlock()
			if na+nb > 1 {
				select {
				case overlap <- fmt.Sprintf("a owns %d and b owns %d workers at once", na, nb):
				default:
				}
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer close(stopWatch)

	// a goes first, so b's first pass is the one refused.
	stopA := runLoop(t, a)
	defer stopA()
	waitFor(t, 30*time.Second, "a's first worker", func() bool { return !idle(a) })
	stopB := runLoop(t, b)
	defer stopB()

	// A worker gives up its issue before it returns its slot, and the
	// wake that follows the last release runs one more pass over the cached
	// ready list: settled means the pool is empty and nobody is queued.
	waitFor(t, 2*time.Minute, "all four issues to be worked and the pool to settle", func() bool {
		return len(a.sessions(config.RoleDeveloper)) == 2 && len(b.sessions(config.RoleDeveloper)) == 2 &&
			idle(a) && idle(b) && pool.InUse() == 0 && len(pool.Waiting()) == 0
	})
	select {
	case msg := <-overlap:
		t.Fatal(msg)
	default:
	}
	if got, want := tl.String(), "a:1 b:1 a:2 b:2"; got != want {
		t.Fatalf("dispatched %q, want %q: the freed slot goes to the project waiting for it", got, want)
	}
	if polls(a) != 1 || polls(b) != 1 {
		t.Fatalf("polls a %d, b %d, want 1 each: the later dispatches must come on wakes", polls(a), polls(b))
	}
}

// A fan-out is clamped to the shared pool as it is to max_developers: three
// attempts on a project pool of three, sharing a pool of two, run as two.
func TestFanOutIsClampedToTheSharedPool(t *testing.T) {
	pool := NewSharedPool(2)
	h := newHarnessAt(t, bestOfNTOML, time.Time{}, func(d *Deps) { d.Shared = pool })
	seedSized(h, 1, "l")
	runPass(t, h)
	h.sched.wg.Wait()

	got := h.sessionNames()
	slices.Sort(got)
	want := []string{"developer-issue-1-assemble", "developer-issue-1-attempt-1", "developer-issue-1-attempt-2"}
	if !slices.Equal(got, want) {
		t.Errorf("sessions: got %v want %v", got, want)
	}
	if !strings.Contains(h.logs.String(), "best-of-N clamped to max_developers") || !strings.Contains(h.logs.String(), "pool=2") {
		t.Errorf("the clamp is not logged with the pool's size:\n%s", h.logs.String())
	}
	if got := freeSlots(h); got != 3 {
		t.Errorf("free slots after the fan-out: got %d want 3", got)
	}
	if got := pool.InUse(); got != 0 {
		t.Errorf("shared slots in use after the fan-out: got %d want 0", got)
	}
}

// A scheduler removed from a machine must relinquish its queued turn while
// preserving slots still held by work in flight.
func TestStoppingSchedulerLeavesSharedQueue(t *testing.T) {
	pool := NewSharedPool(2)
	var w woken
	owner, next := w.member(pool, "owner"), w.member(pool, "next")
	if !owner.acquire(2) {
		t.Fatal("initial acquire")
	}
	h := newHarnessAt(t, wakeTOML+rolesOffTOML, time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC), func(d *Deps) { d.Shared = pool })
	h.sched.Once = false
	seedReady(h, 1, "s", time.Now())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.sched.Run(ctx) }()
	deadline := time.Now().Add(5 * time.Second)
	for len(pool.Waiting()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("scheduler never queued")
		}
		time.Sleep(time.Millisecond)
	}
	if next.acquire(1) {
		t.Fatal("full pool granted claim")
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not stop")
	}
	if got := fmt.Sprint(pool.Waiting()); got != "[next]" {
		t.Fatalf("stopped scheduler kept its turn: %s", got)
	}
	if pool.InUse() != 2 {
		t.Fatal("stop released another worker's slots")
	}
	owner.release(2)
	if !next.acquire(1) {
		t.Fatal("next project remains blocked")
	}
	next.release(1)
}
