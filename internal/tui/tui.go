// Package tui draws the factory's live view: what `bees run` shows while it
// works, fed by the scheduler's event stream in the same process.
//
// It has two screens. The first is the panels. Now lists every running
// session — role, issue or pull request, the stage its developer worker is
// in, how long it has been going, what the work item has spent and the model
// it runs on — and Recent the ones that have finished, with how each ended.
// Needs human is every issue the factory has given up on and why; Approved
// PRs is what is waiting for a person to merge. Queues lists the numbers
// `bees status` prints, read from status.json rather than computed a second
// way, together with the unread mail per role and the time to the next
// GitHub poll. A terminal too short for all five panels draws fewer, from
// the bottom of that order (Model.layout). The second screen is the session
// view (session.go): the transcript of one running session, followed as it
// is written, and a line to type a message for the next session on that
// work item.
//
// The view polls no GitHub of its own, and the scheduler never waits for it
// (an event it has no room for is dropped). Everything it shows it reads
// from the event stream or from disk: status.json, the mailbox and the
// per-session transcript.jsonl. Beyond the two stop keys (q and Ctrl-C,
// which cancel the factory's context and, pressed again, call HardStop), it
// asks the factory to *do* only two things: stop a session, on the k key,
// through Deps.Kill; and queue a message for the next one, on m, through
// Deps.Send. It is drawn only when `bees run`
// owns a terminal — `bees run --no-tui`, a redirected stdout and `bees tick`
// log instead, and their output is exactly what it was before there was a
// view.
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
// event stream, runs until its context is cancelled — which starts nothing
// new and lets the work in flight finish — and stops the running sessions
// too when HardStop is called.
type Factory interface {
	Subscribe() <-chan scheduler.Event
	Run(ctx context.Context) error
	HardStop()
}

// Run draws the view while f runs, and returns what f returned.
//
// Ctrl-C is handled by the view rather than by a signal, because Bubble Tea
// puts the terminal in raw mode: the first press cancels the factory's
// context — which stops polling, starts nothing new and lets the work in
// flight finish, exactly as an interrupt does without the view — and the
// view stays up until it has. A second press stops the running sessions too
// (Factory.HardStop), and a third leaves the terminal early; whatever is
// still coming down, the caller waits for it with the console back.
//
// down is called the moment the view has come down and before the factory
// is waited out — `bees run` gives the console its logging back there, so a
// person who left the view early watches the stop finish instead of a
// silent terminal. It may be nil.
func Run(ctx context.Context, d Deps, f Factory, down func()) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.Events = f.Subscribe()
	d.Stop = cancel
	d.HardStop = f.HardStop

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
	// Stop the factory if nothing has yet — the person left the view, or
	// it failed on its own — and wait for it either way: whatever is still
	// coming down logs with the console back.
	cancel()
	if runErr := <-done; err == nil {
		return runErr
	}
	return err
}
