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
	owner, next := pool.Join("owner", func() {}), pool.Join("next", func() {})
	if !owner.Acquire(2) {
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
	if next.Acquire(1) {
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
	owner.Release(2)
	if !next.Acquire(1) {
		t.Fatal("next project remains blocked")
	}
	next.Release(1)
}
