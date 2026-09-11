package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/review"
)

// at is a clock for the view's messages.
var at = time.Date(2026, 9, 11, 14, 0, 0, 0, time.UTC)

// progressAfter drives an empty view through msgs and returns it.
func progressAfter(t *testing.T, msgs ...tea.Msg) reviewProgress {
	t.Helper()
	var m tea.Model = newReviewProgress(nil)
	for _, msg := range msgs {
		m, _ = m.Update(msg)
	}
	return m.(reviewProgress)
}

// The view draws what the runner logged before the angles started, then
// one row per angle in the catalog's order whatever order they were heard
// in, then what was logged after: the last frame reads as the plain log
// would, with the rows under the line that names the angles.
func TestReviewProgressDrawsTheLogAroundTheAngles(t *testing.T) {
	m := progressAfter(t,
		logLineMsg("gathering the context of acme/widgets#7"),
		logLineMsg("reviewing a size m change from 2 angles: general, docs"),
		angleMsg{angle: review.AngleDocs, event: review.AngleStarted, at: at},
		angleMsg{angle: review.AngleGeneral, event: review.AngleStarted, at: at},
		logLineMsg("3 findings"),
	)
	want := "gathering the context of acme/widgets#7\n" +
		"reviewing a size m change from 2 angles: general, docs\n" +
		"  " + spinnerFrames[0] + " general   0s\n" +
		"  " + spinnerFrames[0] + " docs      0s\n" +
		"3 findings\n"
	if got := m.View(); got != want {
		t.Errorf("view:\n%s\nwant:\n%s", got, want)
	}
}

// A row's spinner turns on every tick until the run ends, and the elapsed
// time moves with the tick; once the run is done the ticking stops.
func TestReviewProgressSpinsUntilTheRunEnds(t *testing.T) {
	m := progressAfter(t, angleMsg{angle: review.AngleGeneral, event: review.AngleStarted, at: at})
	next, cmd := m.Update(spinMsg(at.Add(3 * time.Second)))
	if cmd == nil {
		t.Fatal("a tick did not schedule the next one")
	}
	m = next.(reviewProgress)
	if got, want := m.View(), "  "+spinnerFrames[1]+" general   3s\n"; got != want {
		t.Errorf("after one tick:\n%s\nwant:\n%s", got, want)
	}
	next, cmd = m.Update(runDoneMsg{})
	if cmd == nil {
		t.Fatal("the run ending did not end the view")
	}
	if _, quit := cmd().(tea.QuitMsg); !quit {
		t.Errorf("the run ending gave %T, want tea.QuitMsg", cmd())
	}
	if _, cmd = next.Update(spinMsg(at.Add(4 * time.Second))); cmd != nil {
		t.Error("the spinner kept ticking after the run ended")
	}
}

// A row is marked as its session ends: a check with how long it took, or a
// cross saying it failed after how long. The mark does not turn with the
// spinner.
func TestReviewProgressMarksAnAngleAsItEnds(t *testing.T) {
	m := progressAfter(t,
		angleMsg{angle: review.AngleGeneral, event: review.AngleStarted, at: at},
		angleMsg{angle: review.AngleDocs, event: review.AngleStarted, at: at},
		angleMsg{angle: review.AngleTests, event: review.AngleStarted, at: at},
		angleMsg{angle: review.AngleGeneral, event: review.AngleFinished, at: at.Add(90 * time.Second)},
		angleMsg{angle: review.AngleDocs, event: review.AngleFailed, at: at.Add(5 * time.Second)},
		spinMsg(at.Add(2*time.Minute)),
	)
	want := "  " + markFinished + " general         1m30s\n" +
		"  " + markFailed + " docs            failed after 5s\n" +
		"  " + spinnerFrames[1] + " test_coverage   2m0s\n"
	if got := m.View(); got != want {
		t.Errorf("view:\n%s\nwant:\n%s", got, want)
	}
}

// ctrl-c cancels the run's context and the view says it is waiting, until
// the run has unwound; a second ctrl-c changes nothing.
func TestReviewProgressCtrlCStopsTheRunAndWaitsForIt(t *testing.T) {
	cancelled := 0
	var m tea.Model = newReviewProgress(func() { cancelled++ })
	m, _ = m.Update(angleMsg{angle: review.AngleGeneral, event: review.AngleStarted, at: at})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cancelled != 1 {
		t.Errorf("ctrl-c cancelled the run %d times, want once", cancelled)
	}
	if v := m.View(); !strings.HasSuffix(v, "stopping: waiting for the sessions to end\n") {
		t.Errorf("the view does not say it is stopping:\n%s", v)
	}
	m, _ = m.Update(runDoneMsg{})
	if v := m.View(); strings.Contains(v, "stopping") {
		t.Errorf("the view still says it is stopping after the run ended:\n%s", v)
	}
}

// An empty view draws nothing, and an angle that ends before the view
// heard it start still gets a row.
func TestReviewProgressSurvivesWhatItWasNotTold(t *testing.T) {
	if v := newReviewProgress(nil).View(); v != "" {
		t.Errorf("an empty view drew %q", v)
	}
	m := progressAfter(t, angleMsg{angle: review.AngleDocs, event: review.AngleFinished, at: at})
	if got, want := m.View(), "  "+markFinished+" docs   0s\n"; got != want {
		t.Errorf("view:\n%s\nwant:\n%s", got, want)
	}
}

// Without a terminal the progress hook prints one line per event, the lines
// whole however many angles print at once.
func TestLogProgressPrintsOneLinePerEvent(t *testing.T) {
	var buf bytes.Buffer
	hook := logProgress(&buf)
	var wg sync.WaitGroup
	for _, angle := range review.BuiltinAngles {
		wg.Add(1)
		go func() {
			defer wg.Done()
			hook(angle, review.AngleStarted)
			hook(angle, review.AngleFinished)
		}()
	}
	wg.Wait()
	hook(review.AngleDocs, review.AngleFailed)
	lines := strings.Split(strings.TrimSuffix(buf.String(), "\n"), "\n")
	if want := 2*len(review.BuiltinAngles) + 1; len(lines) != want {
		t.Fatalf("%d lines, want %d:\n%s", len(lines), want, buf.String())
	}
	for _, line := range lines {
		if !strings.HasPrefix(line, "  the ") || !(strings.HasSuffix(line, " angle started") || strings.HasSuffix(line, " angle finished") || strings.HasSuffix(line, " angle failed")) {
			t.Errorf("line %q is not one event", line)
		}
	}
	if lines[len(lines)-1] != "  the docs angle failed" {
		t.Errorf("last line %q", lines[len(lines)-1])
	}
}

// syncBuffer is a buffer the program's renderer can write from its own
// goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// withProgressOutput drives the view's program without a terminal: keys
// from input, frames into the returned buffer.
func withProgressOutput(t *testing.T, input io.Reader) *syncBuffer {
	t.Helper()
	out := &syncBuffer{}
	real := progressOptions
	t.Cleanup(func() { progressOptions = real })
	progressOptions = func() []tea.ProgramOption {
		return []tea.ProgramOption{tea.WithInput(input), tea.WithOutput(out)}
	}
	return out
}

// Drawn over a run, the view is told every line the run logs and every
// angle event, and the run's own result comes back through it once the
// view has closed.
func TestDrawReviewProgressReturnsWhatTheRunCameTo(t *testing.T) {
	out := withProgressOutput(t, nil)
	artifact := &review.Artifact{Dir: t.TempDir()}
	got, err := drawReviewProgress(context.Background(), func(ctx context.Context, log io.Writer, progress func(string, review.AngleEvent)) (*review.Artifact, error) {
		_, _ = io.WriteString(log, "reviewing a size m change from 2 angles: general, docs\n")
		progress(review.AngleGeneral, review.AngleStarted)
		progress(review.AngleDocs, review.AngleStarted)
		progress(review.AngleGeneral, review.AngleFinished)
		progress(review.AngleDocs, review.AngleFailed)
		_, _ = io.WriteString(log, "  the docs angle failed: exit status 1\n1 finding\n")
		return artifact, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != artifact {
		t.Errorf("artifact %p, want the run's %p", got, artifact)
	}
	for _, want := range []string{
		"reviewing a size m change from 2 angles: general, docs",
		markFinished + " general",
		markFailed + " docs",
		"  the docs angle failed: exit status 1",
		"1 finding",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("the frames lack %q:\n%s", want, out.String())
		}
	}
	// A run that failed is the view's error too.
	_, err = drawReviewProgress(context.Background(), func(context.Context, io.Writer, func(string, review.AngleEvent)) (*review.Artifact, error) {
		return nil, errors.New("every angle failed")
	})
	if err == nil || err.Error() != "every angle failed" {
		t.Errorf("err = %v, want the run's", err)
	}
}

// ctrl-c at the view cancels the run, and the view stays up until the run
// has returned: the error that comes back is the run's, not the view's.
func TestDrawReviewProgressCtrlCCancelsTheRun(t *testing.T) {
	keys, typed := io.Pipe()
	withProgressOutput(t, keys)
	started := make(chan struct{})
	go func() {
		<-started
		_, _ = typed.Write([]byte{3}) // ctrl-c
	}()
	_, err := drawReviewProgress(context.Background(), func(ctx context.Context, log io.Writer, progress func(string, review.AngleEvent)) (*review.Artifact, error) {
		progress(review.AngleGeneral, review.AngleStarted)
		close(started)
		<-ctx.Done()
		progress(review.AngleGeneral, review.AngleFailed)
		return nil, ctx.Err()
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want the cancelled run's", err)
	}
	_ = typed.Close()
}

// At a terminal the review's lines go to the view and not to stdout;
// without one, or with --no-tui, they print as before.
func TestReviewDrawsItsProgressOnlyAtATerminal(t *testing.T) {
	reviewHome(t)
	// No gh on the PATH: the review fails at the gather, after its first
	// line, which is enough to see where the line went.
	t.Setenv("PATH", t.TempDir())
	withTerminal(t, true)
	out := withProgressOutput(t, nil)
	stdout, _, err := runReview(t, "acme/widgets#7")
	if err == nil || !strings.Contains(err.Error(), "read acme/widgets#7") {
		t.Errorf("err = %v, want the gather that could not read the pull request", err)
	}
	if strings.Contains(stdout, "gathering the context") {
		t.Errorf("at a terminal the review logged to stdout:\n%s", stdout)
	}
	if !strings.Contains(out.String(), "gathering the context of acme/widgets#7") {
		t.Errorf("the view was not told the review's first line:\n%s", out.String())
	}
	out = withProgressOutput(t, nil)
	stdout, _, _ = runReview(t, "acme/widgets#7", "--no-tui")
	if !strings.Contains(stdout, "gathering the context of acme/widgets#7\n") || out.String() != "" {
		t.Errorf("--no-tui at a terminal did not print the plain lines:\nstdout:\n%s\nview:\n%s", stdout, out.String())
	}
	withTerminal(t, false)
	out = withProgressOutput(t, nil)
	stdout, _, _ = runReview(t, "acme/widgets#7")
	if !strings.Contains(stdout, "gathering the context of acme/widgets#7\n") || out.String() != "" {
		t.Errorf("without a terminal the review did not print the plain lines:\nstdout:\n%s\nview:\n%s", stdout, out.String())
	}
}
