package daemon

import (
	"context"
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
