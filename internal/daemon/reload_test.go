package daemon

import (
	"context"
	"errors"
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
