package tui

import (
	"context"
	"errors"
	"io"
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
	go func() { done <- Run(context.Background(), Deps{Repo: "acme/widgets"}, f) }()
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
	go func() { done <- Run(ctx, Deps{Repo: "acme/widgets"}, f) }()
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
	go func() { done <- Run(context.Background(), Deps{Repo: "acme/widgets"}, f) }()
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
