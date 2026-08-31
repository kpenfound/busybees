// Package tui draws the factory's live view: the panels `bees run` shows
// while it works, fed by the scheduler's event stream in the same process.
//
// Now lists every running session — role, issue or pull request, the stage
// its developer worker is in, how long it has been going, what the work item
// has spent and the model it runs on — and Recent the ones that have
// finished, with how each ended. Needs human is every issue the factory has
// given up on and why; Approved PRs is what is waiting for a person to
// merge. Queues lists the numbers `bees status` prints, read from
// status.json rather than computed a second way, together with the unread
// mail per role and the time to the next GitHub poll.
//
// The view is very nearly a subscriber and nothing more: everything it draws
// comes from the event stream and from status.json, it polls no GitHub of
// its own, and the scheduler never waits for it (an event it has no room for
// is dropped). The one thing it asks the factory to *do* is stop a session,
// on the k key, through Deps.Kill. It is drawn only when `bees run` owns a
// terminal — `bees run --no-tui`, a redirected stdout and `bees tick` log
// instead, and their output is exactly what it was before there was a view.
package tui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/scheduler"
)

// programOptions are the options the Bubble Tea program is started with. It
// is a variable so a test can drive Run without a terminal, the way
// cmd/bees' isTerminal is one.
var programOptions = func() []tea.ProgramOption {
	return []tea.ProgramOption{tea.WithAltScreen()}
}

// Factory is the half of the scheduler the view drives: it subscribes to the
// event stream and runs until its context is cancelled.
type Factory interface {
	Subscribe() <-chan scheduler.Event
	Run(ctx context.Context) error
}

// Run draws the view while f runs, and returns what f returned.
//
// Ctrl-C is handled by the view rather than by a signal, because Bubble Tea
// puts the terminal in raw mode: the first press cancels the factory's
// context — which is what stops polling and drains, exactly as an interrupt
// does without the view — and the view stays up until the drain is over. A
// second press leaves the terminal early; the sessions it started are still
// finishing, so the caller waits for them with the console back.
//
// down is called the moment the view has come down and before the drain is
// waited out — `bees run` gives the console its logging back there, so the
// person who pressed Ctrl-C twice watches the sessions finish instead of a
// silent terminal. It may be nil.
func Run(ctx context.Context, d Deps, f Factory, down func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.Events = f.Subscribe()
	d.Stop = cancel

	p := tea.NewProgram(New(d), programOptions()...)
	done := make(chan error, 1)
	go func() {
		err := f.Run(ctx)
		done <- err
		// Whatever stopped the factory — a drain the person asked for, a
		// SIGTERM, --once finishing — the view has nothing left to show.
		p.Send(Stopped{})
	}()
	_, err := p.Run()
	// The view is down: hand the terminal back before waiting on anything,
	// so whatever the caller prints during the drain is seen.
	if down != nil {
		down()
	}
	// Stop the factory if nothing has yet — the person gave up on the
	// drain, or the view itself failed — and wait for it either way: the
	// sessions it started are still finishing, and the console has its
	// logging back to say so.
	cancel()
	if runErr := <-done; err == nil {
		return runErr
	}
	return err
}
