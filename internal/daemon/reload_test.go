package daemon

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"
)

// Removal drains one loop; unchanged projects keep their identity, and a
// project re-added during drain never overlaps its previous scheduler.
func TestReloadReconcilesAndWaitsForRemovedProject(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reload := make(chan []Project)
	started := make(chan string, 10)
	draining := make(chan string, 10)
	release := make(chan struct{})
	makeProject := func(name string, hold bool) Project {
		return project(name, &fakeLoop{run: func(ctx context.Context) error {
			started <- name
			<-ctx.Done()
			draining <- name
			if hold {
				<-release
			}
			return nil
		}}, nil)
	}
	a, b, c := makeProject("a", true), makeProject("b", false), makeProject("c", false)
	log, _ := quietLogger()
	d := &Daemon{Projects: []Project{a, b}, Reload: reload, Logger: log}
	done := runAsync(ctx, d)
	receive := func(ch <-chan string) string {
		t.Helper()
		select {
		case s := <-ch:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("missing lifecycle event")
			return ""
		}
	}
	first, second := receive(started), receive(started)
	if first == second {
		t.Fatal("did not start both projects")
	}
	reload <- []Project{b, c}
	if got := receive(draining); got != "a" {
		t.Fatalf("draining %s, want a", got)
	}
	if got := receive(started); got != "c" {
		t.Fatalf("started %s, want c", got)
	}
	reload <- []Project{a, b, c}
	select {
	case got := <-started:
		t.Fatalf("restarted %s before drain finished", got)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if got := receive(started); got != "a" {
		t.Fatalf("restarted %s, want a", got)
	}
	cancel()
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	for len(draining) > 0 {
		counts[<-draining]++
	}
	if counts["a"] != 1 || counts["b"] != 1 || counts["c"] != 1 {
		t.Fatalf("final drains: %v", counts)
	}
}

func TestReloadKeepsEmptyDaemonAliveAndHardStopsDrainingLoops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reload := make(chan []Project)
	started, drain, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	loop := &fakeLoop{run: func(ctx context.Context) error { close(started); <-ctx.Done(); close(drain); <-release; return nil }}
	log, _ := quietLogger()
	d := &Daemon{Projects: []Project{project("a", loop, nil)}, Reload: reload, Logger: log}
	done := runAsync(ctx, d)
	<-started
	reload <- nil
	<-drain
	d.HardStop()
	if loop.hardStop.Load() != 1 {
		t.Fatal("hard stop missed draining project")
	}
	close(release)
	select {
	case err := <-done:
		t.Fatalf("empty daemon exited: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	cancel()
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}

func TestReloadKeepsDaemonAliveAfterEveryProjectFails(t *testing.T) {
	for _, action := range []string{"cancel", "reload", "close"} {
		t.Run(action, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			reload := make(chan []Project)
			startErr, loopErr := errors.New("cannot start"), errors.New("cannot poll")
			log, logs := quietLogger()
			d := &Daemon{Logger: log, Reload: reload, Projects: []Project{
				project("start-failure", nil, startErr),
				project("loop-failure", &fakeLoop{run: func(context.Context) error { return loopErr }}, nil),
			}}
			done := runAsync(ctx, d)
			for strings.Count(logs.String(), "project stopped") != 2 {
				select {
				case err := <-done:
					t.Fatalf("daemon returned before reload or cancellation: %v", err)
				case <-ctx.Done():
					t.Fatal("projects did not report both failures")
				case <-time.After(time.Millisecond):
				}
			}
			select {
			case err := <-done:
				t.Fatalf("daemon exited after all projects failed: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			switch action {
			case "cancel":
				cancel()
			case "close":
				close(reload)
			case "reload":
				started, release := make(chan struct{}), make(chan struct{})
				healthy := project("healthy", &fakeLoop{run: func(ctx context.Context) error {
					close(started)
					select {
					case <-release:
					case <-ctx.Done():
					}
					return nil
				}}, nil)
				select {
				case reload <- []Project{healthy}:
				case err := <-done:
					t.Fatalf("daemon returned instead of reloading: %v", err)
				case <-ctx.Done():
					t.Fatal("reload blocked")
				}
				select {
				case <-started:
				case <-ctx.Done():
					t.Fatal("replacement never started")
				}
				close(reload)
				select {
				case err := <-done:
					t.Fatalf("closed reload did not wait for active project: %v", err)
				case <-time.After(50 * time.Millisecond):
				}
				close(release)
			}
			err := wait(t, done)
			if !errors.Is(err, startErr) || !errors.Is(err, loopErr) {
				t.Fatalf("lost project errors: %v", err)
			}
			if ctx.Err() == context.DeadlineExceeded {
				t.Fatal("daemon needed timeout to exit")
			}
		})
	}
}

// Observers receive the original incarnation's drain and finish, and a
// complete reconciliation boundary after all lifecycle changes, even when a
// newer callback for the same path has arrived in a reload.
func TestReloadLifecycleObserversKeepIncarnationsAndOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reload := make(chan []Project)
	release := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	type snapshot struct {
		order  []string
		states []string
	}
	updates := make(chan snapshot, 20)
	var states []string // daemon goroutine only; copied at each boundary
	makeProject := func(path, incarnation string, hold bool) Project {
		p := project(path, &fakeLoop{run: func(ctx context.Context) error {
			<-ctx.Done()
			if hold {
				<-release
			}
			return nil
		}}, nil)
		p.Observe = func(s ProjectState) { states = append(states, fmt.Sprintf("%s:%d", incarnation, s)) }
		return p
	}
	a, b, c := makeProject("a", "a1", false), makeProject("b", "b1", true), makeProject("c", "c1", false)
	d := &Daemon{Projects: []Project{a, b}, Reload: reload, Reconciled: func(order []string) {
		updates <- snapshot{order, slices.Clone(states)}
		states = nil
	}}
	done := runAsync(ctx, d)
	receive := func() snapshot {
		t.Helper()
		select {
		case s := <-updates:
			return s
		case <-time.After(5 * time.Second):
			t.Fatal("no lifecycle boundary")
			return snapshot{}
		}
	}
	if s := receive(); !slices.Equal(s.states, []string{"a1:0", "b1:0"}) {
		t.Fatalf("initial: %+v", s)
	}
	reload <- []Project{a, c}
	if s := receive(); !slices.Equal(s.order, []string{"a", "c"}) || !slices.Equal(s.states, []string{"b1:1", "c1:0"}) {
		t.Fatalf("replace: %+v", s)
	}
	b2 := makeProject("b", "b2", false)
	reload <- []Project{c, b2, a}
	if s := receive(); !slices.Equal(s.order, []string{"c", "b", "a"}) || len(s.states) != 0 {
		t.Fatalf("reorder/readd restarted a source: %+v", s)
	}
	close(release)
	if s := receive(); !slices.Equal(s.states, []string{"b1:2", "b2:0"}) {
		t.Fatalf("old/new incarnation: %+v", s)
	}
	cancel()
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
}
