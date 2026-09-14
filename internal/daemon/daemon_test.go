package daemon

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeLoop runs until ctx is cancelled, or does what run says.
type fakeLoop struct {
	run      func(ctx context.Context) error
	hardStop atomic.Int32
}

func (f *fakeLoop) Run(ctx context.Context) error {
	if f.run != nil {
		return f.run(ctx)
	}
	<-ctx.Done()
	return nil
}

func (f *fakeLoop) HardStop() { f.hardStop.Add(1) }

func project(name string, loop Loop, startErr error) Project {
	return Project{Name: name, Start: func(context.Context) (Loop, error) { return loop, startErr }}
}

// lockedBuffer is a log destination a test can read while projects write.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func quietLogger() (*slog.Logger, *lockedBuffer) {
	buf := &lockedBuffer{}
	return slog.New(slog.NewTextHandler(buf, nil)), buf
}

// runAsync runs d in the background and returns its result channel.
func runAsync(ctx context.Context, d *Daemon) <-chan error {
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	return done
}

func wait(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatal("the daemon did not return")
		return nil
	}
}

// Every project's loop runs at the same time: each one only returns once all
// of them have started.
func TestRunsEveryProjectConcurrently(t *testing.T) {
	const n = 3
	var started sync.WaitGroup
	started.Add(n)
	all := make(chan struct{})
	go func() { started.Wait(); close(all) }()
	var projects []Project
	for _, name := range []string{"a", "b", "c"} {
		loop := &fakeLoop{run: func(ctx context.Context) error {
			started.Done()
			select {
			case <-all:
				return nil
			case <-ctx.Done():
				return errors.New("not all projects were running at once")
			}
		}}
		projects = append(projects, project(name, loop, nil))
	}
	log, _ := quietLogger()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := wait(t, runAsync(ctx, &Daemon{Projects: projects, Logger: log})); err != nil {
		t.Fatal(err)
	}
}

// A project that fails to start, returns an error or panics is reported for
// that project alone; the healthy project keeps running until ctx ends.
func TestOneProjectFailingLeavesTheOthersRunning(t *testing.T) {
	for name, bad := range map[string]Project{
		"start error": project("bad", nil, errors.New("not a git clone")),
		"loop error":  project("bad", &fakeLoop{run: func(context.Context) error { return errors.New("poll broke") }}, nil),
		"loop panic":  project("bad", &fakeLoop{run: func(context.Context) error { panic("boom") }}, nil),
		"start panic": {Name: "bad", Start: func(context.Context) (Loop, error) { panic("boom") }},
	} {
		t.Run(name, func(t *testing.T) {
			healthyStopped := make(chan struct{})
			healthy := &fakeLoop{run: func(ctx context.Context) error {
				<-ctx.Done()
				close(healthyStopped)
				return nil
			}}
			log, logs := quietLogger()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := runAsync(ctx, &Daemon{Projects: []Project{bad, project("good", healthy, nil)}, Logger: log})

			// The bad project has failed and the good one is still running.
			deadline := time.Now().Add(5 * time.Second)
			for !strings.Contains(logs.String(), "project=bad") {
				if time.Now().After(deadline) {
					t.Fatalf("no failure logged for the bad project: %q", logs.String())
				}
				time.Sleep(10 * time.Millisecond)
			}
			select {
			case <-healthyStopped:
				t.Fatal("the healthy project stopped when the bad one failed")
			case err := <-done:
				t.Fatalf("the daemon returned while a project was still running: %v", err)
			case <-time.After(50 * time.Millisecond):
			}

			cancel()
			err := wait(t, done)
			var pe *ProjectError
			if !errors.As(err, &pe) || pe.Project != "bad" {
				t.Fatalf("error %v, want a ProjectError for bad", err)
			}
			if strings.Contains(err.Error(), "good") {
				t.Errorf("the healthy project was reported as failed: %v", err)
			}
		})
	}
}

func TestNoFailureIsNil(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	log, _ := quietLogger()
	d := &Daemon{Projects: []Project{project("a", &fakeLoop{}, nil), project("b", &fakeLoop{}, nil)}, Logger: log}
	if err := d.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

// HardStop reaches the loop of every project that has started, and a project
// whose start finishes after it never runs.
func TestHardStopReachesEveryStartedLoop(t *testing.T) {
	a, b := &fakeLoop{}, &fakeLoop{}
	lateStarted := make(chan struct{})
	release := make(chan struct{})
	late := &fakeLoop{run: func(context.Context) error { t.Error("a loop started after HardStop ran"); return nil }}
	lateProject := Project{Name: "late", Start: func(context.Context) (Loop, error) {
		close(lateStarted)
		<-release
		return late, nil
	}}
	log, _ := quietLogger()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	d := &Daemon{Projects: []Project{project("a", a, nil), project("b", b, nil), lateProject}, Logger: log}
	done := runAsync(ctx, d)

	<-lateStarted
	deadline := time.Now().Add(5 * time.Second)
	for {
		d.mu.Lock()
		n := len(d.loops)
		d.mu.Unlock()
		if n == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the two loops never started")
		}
		time.Sleep(10 * time.Millisecond)
	}
	d.HardStop()
	close(release)
	cancel()
	if err := wait(t, done); err != nil {
		t.Fatal(err)
	}
	if a.hardStop.Load() != 1 || b.hardStop.Load() != 1 {
		t.Errorf("hard stops: a %d, b %d, want 1 each", a.hardStop.Load(), b.hardStop.Load())
	}
	if late.hardStop.Load() != 0 {
		t.Errorf("the loop that never ran was hard-stopped")
	}
}
