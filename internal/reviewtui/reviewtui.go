// Package reviewtui draws the triage screen: what a person triaging a
// review at a terminal sees, one finding at a time, beside the pull
// request's diff.
//
// It is the second front end to the triage queue (internal/review's Queue),
// after the console (internal/review/console.go), and it drives the queue
// the same way: the same keys, the same four actions, and every decision
// written into the artifact before the next finding is shown, so a triage
// stopped mid-way picks up where it left off whichever front end runs it.
// What the console reads a line at a time this screen takes on a single
// key, no return needed, and it draws the diff (review.NewDiffView) on one
// side and the finding on the other, the finding's lines marked.
//
// The screen is given the diff as text and the queue as built: it fetches
// nothing, reads no GitHub and holds no client. Choosing how the review ends
// is not its job either: the command that opens it asks that at the
// terminal once the screen has closed (Console.Choose), as it does after
// the console.
package reviewtui

import (
	"context"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/review"
)

// programOptions are the options the Bubble Tea program is started with. It
// is a variable so a test can drive Run without a terminal.
var programOptions = func() []tea.ProgramOption {
	return []tea.ProgramOption{tea.WithAltScreen()}
}

// Run draws the screen over q until nothing is left undecided or the quit
// key is pressed, and returns the error that stopped triage, if one did: a
// decision that could not be written into the artifact ends the screen at
// once, the way it ends the console, and the exit is the person's signal
// that the last decision was not kept. An action the queue refused is shown
// on screen instead, and the finding stays. diff is the pull request's
// unified diff, as `gh pr diff` prints it; it may be empty, and the diff
// pane then says so. A cancelled ctx closes the screen.
func Run(ctx context.Context, diff string, q *review.Queue) error {
	p := tea.NewProgram(New(ctx, diff, q), append(programOptions(), tea.WithContext(ctx))...)
	final, err := p.Run()
	if err != nil {
		return err
	}
	return final.(Model).err
}
