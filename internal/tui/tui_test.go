package tui

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/scheduler"
)

// headless runs the Bubble Tea program with no terminal at all: no input, no
// renderer, output thrown away. CI has none, and none of what Run wires up
// needs one.
func headless(t *testing.T) {
	t.Helper()
	real := programOptions
	t.Cleanup(func() { programOptions = real })
	programOptions = func() []tea.ProgramOption {
		return []tea.ProgramOption{tea.WithInput(nil), tea.WithOutput(io.Discard), tea.WithoutRenderer()}
	}
}

// fakeFactory stands in for the scheduler: it subscribes like one and runs
// until its context is cancelled or until it is told to stop.
type fakeFactory struct {
	events chan scheduler.Event
	stop   chan struct{}
	err    error
	ctxErr error
}

func (f *fakeFactory) Subscribe() <-chan scheduler.Event { return f.events }

func (f *fakeFactory) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		f.ctxErr = ctx.Err()
	case <-f.stop:
	}
	return f.err
}

// The view lives exactly as long as the factory: when the factory stops on
// its own — `--once` finishing, a SIGTERM cancelling the command's context —
// the view comes down and Run returns what the factory returned.
func TestRunEndsWhenTheFactoryDoes(t *testing.T) {
	headless(t)
	want := errors.New("poll failed for good")
	f := &fakeFactory{events: make(chan scheduler.Event, 1), stop: make(chan struct{}), err: want}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), Deps{Repo: "acme/widgets"}, f, nil) }()
	// The factory is subscribed to before it is run, so an event published
	// as the first pass starts is never missed.
	f.events <- scheduler.Event{Kind: scheduler.EventPoll}
	close(f.stop)
	select {
	case err := <-done:
		if !errors.Is(err, want) {
			t.Errorf("Run returned %v, want the factory's %v", err, want)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after the factory stopped")
	}
}

// Cancelling the context Run was given stops the factory, whether or not
// anyone pressed anything: `bees run` under a process manager is stopped by
// a signal, and the view must not hold the process open.
func TestRunStopsTheFactoryWhenItsContextIsCancelled(t *testing.T) {
	headless(t)
	f := &fakeFactory{events: make(chan scheduler.Event, 1), stop: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, Deps{Repo: "acme/widgets"}, f, nil) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
		if f.ctxErr == nil {
			t.Error("the factory was not stopped by the cancelled context")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return after its context was cancelled")
	}
}

// Ctrl-C in the view stops the factory. Bubble Tea puts the terminal in raw
// mode, so the interrupt is a key and not a signal: this drives the real key
// through a pipe rather than a terminal, and what proves it arrived is the
// factory's context being cancelled.
func TestCtrlCInTheViewCancelsTheFactory(t *testing.T) {
	keys, keyboard := io.Pipe()
	real := programOptions
	t.Cleanup(func() { programOptions = real })
	programOptions = func() []tea.ProgramOption {
		return []tea.ProgramOption{tea.WithInput(keys), tea.WithOutput(io.Discard), tea.WithoutRenderer()}
	}

	f := &fakeFactory{events: make(chan scheduler.Event, 1), stop: make(chan struct{})}
	done := make(chan error, 1)
	go func() { done <- Run(context.Background(), Deps{Repo: "acme/widgets"}, f, nil) }()
	if _, err := keyboard.Write([]byte{3}); err != nil { // ^C
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
		if f.ctxErr == nil {
			t.Error("ctrl-c did not cancel the factory's context")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("ctrl-c did not stop the factory")
	}
	_ = keyboard.Close()
}

// The console comes back before the drain is waited out, not after it. A
// second Ctrl-C leaves the terminal while the sessions the factory started
// are still finishing and still logging their summaries, so the hand-back
// has to happen before Run waits on the factory: otherwise the person
// watches a silent terminal for as long as the drain takes.
func TestTheTerminalIsHandedBackBeforeTheDrain(t *testing.T) {
	keys, keyboard := io.Pipe()
	real := programOptions
	t.Cleanup(func() { programOptions = real })
	programOptions = func() []tea.ProgramOption {
		return []tea.ProgramOption{tea.WithInput(keys), tea.WithOutput(io.Discard), tea.WithoutRenderer()}
	}

	var visible atomic.Bool
	handedBack := make(chan struct{})
	// The drain: the factory has stopped polling and is waiting for its
	// sessions, which log as they finish. It records whether the console was
	// back by then.
	f := &drainingFactory{events: make(chan scheduler.Event, 1), drain: func() {
		select {
		case <-handedBack:
			visible.Store(true)
		case <-time.After(5 * time.Second):
		}
	}}

	done := make(chan error, 1)
	go func() {
		done <- Run(context.Background(), Deps{Repo: "acme/widgets"}, f,
			func() { close(handedBack) })
	}()
	if _, err := keyboard.Write([]byte{3, 3}); err != nil { // ^C, then ^C again
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("Run did not return")
	}
	_ = keyboard.Close()
	if !visible.Load() {
		t.Error("the factory drained with the console still silenced: the terminal is handed back only after the wait")
	}
}

// drainingFactory runs until its context is cancelled and then drains, the
// way the scheduler waits for the sessions it started.
type drainingFactory struct {
	events chan scheduler.Event
	drain  func()
}

func (f *drainingFactory) Subscribe() <-chan scheduler.Event { return f.events }

func (f *drainingFactory) Run(ctx context.Context) error {
	<-ctx.Done()
	f.drain()
	return nil
}
