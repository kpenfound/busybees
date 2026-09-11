package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/review"
)

// The progress of a review, as `bees review <pr>` shows it while the
// pipeline runs. At a terminal it is a small Bubble Tea view drawn in place
// (no alternate screen): every line the runner logs, and under the line
// that names the angles one row per angle with a spinner while its session
// runs, turned into a mark when it finishes or fails, with how long it
// took. The view's last frame stays on the terminal when the run ends, so
// what is left reads as the plain log would, and triage follows under it.
//
// Without a terminal, or with --no-tui, nothing is drawn: the runner's
// lines print as they come and each angle prints one line as it starts
// and one as it ends, which is what a script or a CI log gets.

// runReviewShowingProgress runs the review and shows how it goes, one of
// the two ways above, as tuiMode decides.
func runReviewShowingProgress(ctx context.Context, runner *review.Runner, ref review.Ref, noTUI bool) (*review.Artifact, error) {
	run := func(ctx context.Context, log io.Writer, progress func(string, review.AngleEvent)) (*review.Artifact, error) {
		runner.Log, runner.Progress = log, progress
		return runner.Run(ctx, ref)
	}
	if !tuiMode(noTUI, os.Stdout) {
		return run(ctx, os.Stdout, logProgress(os.Stdout))
	}
	return drawReviewProgress(ctx, run)
}

// reviewRun is a review run the way the view watches it: told where to log
// and what to tell about the angles, returning what the run came to.
type reviewRun func(ctx context.Context, log io.Writer, progress func(angle string, event review.AngleEvent)) (*review.Artifact, error)

// logProgress is the progress hook of a review without a terminal: one line
// on w as each angle starts and one as it ends. The hook is called from
// every angle's goroutine at once, so the lines are written one at a time.
func logProgress(w io.Writer) func(angle string, event review.AngleEvent) {
	var mu sync.Mutex
	return func(angle string, event review.AngleEvent) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = fmt.Fprintf(w, "  the %s angle %s\n", angle, event)
	}
}

// drawReviewProgress runs run with the view drawn over it, and returns what
// the run came to.
func drawReviewProgress(ctx context.Context, run reviewRun) (*review.Artifact, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	p := tea.NewProgram(newReviewProgress(cancel), progressOptions()...)
	progress := func(angle string, event review.AngleEvent) {
		p.Send(angleMsg{angle: angle, event: event, at: time.Now()})
	}
	// The run ends the view, and the view can end the run: ctrl-c cancels
	// the context and the view stays up until the run has unwound, so the
	// last frame says how far it got. A view that ended on its own (a
	// signal) cancels the run and waits for it the same way.
	done := make(chan runDoneMsg, 1)
	go func() {
		a, err := run(ctx, progressLog{p}, progress)
		msg := runDoneMsg{artifact: a, err: err}
		done <- msg
		p.Send(msg)
	}()
	_, err := p.Run()
	cancel()
	res := <-done
	if err != nil {
		return nil, err
	}
	return res.artifact, res.err
}

// progressOptions are the options the view's program is started with: the
// terminal as it is, no alternate screen, so the frames scroll with the
// rest of the command's output and the last one stays. It is a variable so
// a test can drive the view without a terminal.
var progressOptions = func() []tea.ProgramOption { return nil }

// progressLog is the runner's Log at a terminal: each line it writes is
// sent to the view, which draws it where the console would have printed
// it.
type progressLog struct{ p *tea.Program }

func (l progressLog) Write(b []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		l.p.Send(logLineMsg(line))
	}
	return len(b), nil
}

// The view's messages: a line the runner logged, an angle's session
// starting or ending, a tick of the spinner, and the run ending.
type (
	logLineMsg string
	angleMsg   struct {
		angle string
		event review.AngleEvent
		at    time.Time
	}
	spinMsg    time.Time
	runDoneMsg struct {
		artifact *review.Artifact
		err      error
	}
)

// spinInterval is how often the spinner turns, and the elapsed times move.
const spinInterval = 100 * time.Millisecond

// spinnerFrames are the spinner's frames, one per tick.
var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

// Marks an ended angle's row carries instead of the spinner.
const (
	markFinished = "✓"
	markFailed   = "✗"
)

// angleRow is one angle of the fan-out as the view knows it.
type angleRow struct {
	angle   string
	started time.Time
	ended   time.Time
	event   review.AngleEvent
}

// reviewProgress is the view: what the runner logged before the angles
// started and after, the angles that have started with where each one is,
// the spinner's frame and the time the elapsed times are measured against,
// which is the latest time a tick or an angle event carried.
type reviewProgress struct {
	before   []string
	after    []string
	angles   []angleRow
	frame    int
	now      time.Time
	cancel   context.CancelFunc
	stopping bool
	done     bool
}

// newReviewProgress is an empty view. cancel is what ctrl-c calls: the
// run's context, which ends the run, which ends the view. The view's clock
// is the time on the messages it gets, so it starts at zero.
func newReviewProgress(cancel context.CancelFunc) reviewProgress {
	return reviewProgress{cancel: cancel}
}

func (m reviewProgress) Init() tea.Cmd { return spin() }

func spin() tea.Cmd {
	return tea.Tick(spinInterval, func(t time.Time) tea.Msg { return spinMsg(t) })
}

func (m reviewProgress) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case logLineMsg:
		if len(m.angles) == 0 {
			m.before = append(m.before, string(msg))
		} else {
			m.after = append(m.after, string(msg))
		}
	case angleMsg:
		m.apply(msg)
	case spinMsg:
		if m.done {
			return m, nil
		}
		m.frame = (m.frame + 1) % len(spinnerFrames)
		m.now = time.Time(msg)
		return m, spin()
	case runDoneMsg:
		m.done = true
		return m, tea.Quit
	case tea.KeyMsg:
		if msg.Type == tea.KeyCtrlC && !m.stopping {
			m.stopping = true
			if m.cancel != nil {
				m.cancel()
			}
		}
	}
	return m, nil
}

// apply records an angle's session starting or ending. The angles start
// at once and are told in no particular order, so a row is placed by its
// angle's place in the catalog, not by when it was heard of.
func (m *reviewProgress) apply(msg angleMsg) {
	if msg.at.After(m.now) {
		m.now = msg.at
	}
	i := slices.IndexFunc(m.angles, func(r angleRow) bool { return r.angle == msg.angle })
	if msg.event == review.AngleStarted {
		if i >= 0 {
			return
		}
		m.angles = append(m.angles, angleRow{angle: msg.angle, started: msg.at, event: msg.event})
		slices.SortStableFunc(m.angles, func(a, b angleRow) int {
			return slices.Index(review.BuiltinAngles, a.angle) - slices.Index(review.BuiltinAngles, b.angle)
		})
		return
	}
	if i < 0 {
		m.angles = append(m.angles, angleRow{angle: msg.angle, started: msg.at})
		i = len(m.angles) - 1
	}
	m.angles[i].ended, m.angles[i].event = msg.at, msg.event
}

// View is the lines logged before the angles started, one row per angle
// heard of, the lines logged since, then a line while the run is being
// stopped. It ends with a newline so the program's exit, which clears the
// line the cursor is on, clears nothing of the last frame.
func (m reviewProgress) View() string {
	var out strings.Builder
	for _, line := range m.before {
		out.WriteString(line)
		out.WriteString("\n")
	}
	width := 0
	for _, r := range m.angles {
		width = max(width, len(r.angle))
	}
	for _, r := range m.angles {
		out.WriteString("  ")
		out.WriteString(m.mark(r))
		out.WriteString(" ")
		out.WriteString(r.angle)
		out.WriteString(strings.Repeat(" ", width-len(r.angle)+3))
		out.WriteString(m.elapsed(r))
		out.WriteString("\n")
	}
	for _, line := range m.after {
		out.WriteString(line)
		out.WriteString("\n")
	}
	if m.stopping && !m.done {
		out.WriteString("stopping: waiting for the sessions to end\n")
	}
	return out.String()
}

// mark is what a row starts with: the spinner while its session runs, a
// mark once it has ended.
func (m reviewProgress) mark(r angleRow) string {
	switch r.event {
	case review.AngleFinished:
		return styleFinished.Render(markFinished)
	case review.AngleFailed:
		return styleFailed.Render(markFailed)
	}
	return styleSpinner.Render(spinnerFrames[m.frame])
}

// elapsed is how long a row's session has run, or took: to the second,
// against the last tick while it runs.
func (m reviewProgress) elapsed(r angleRow) string {
	end := m.now
	if !r.ended.IsZero() {
		end = r.ended
	}
	if end.Before(r.started) {
		end = r.started
	}
	d := end.Sub(r.started).Round(time.Second)
	if r.event == review.AngleFailed {
		return "failed after " + d.String()
	}
	return d.String()
}
