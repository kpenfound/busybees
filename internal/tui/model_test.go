package tui

import (
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/state"
)

// fixed is the clock every test renders against: the view must never read
// the wall clock, so two renders of the same messages are the same string
// (#222).
var fixed = time.Date(2026, 8, 31, 10, 3, 8, 0, time.UTC)

// drive feeds the model a sequence of messages and returns the view it
// renders afterwards, with any styling stripped. It is the whole test
// harness: Update and View are plain functions, so nothing here needs a
// terminal (Bubble Tea is never started).
func drive(t *testing.T, d Deps, msgs ...tea.Msg) string {
	t.Helper()
	if d.Now == nil {
		d.Now = func() time.Time { return fixed }
	}
	var m tea.Model = New(d)
	for _, msg := range msgs {
		m, _ = m.Update(msg)
	}
	return plain(m.View())
}

// ansi matches the escape sequences lipgloss emits on a terminal that has
// colour. A test asserts on the text, not on how it was painted.
var ansi = regexp.MustCompile(`\x1b\[[0-9;]*m`)

func plain(s string) string { return ansi.ReplaceAllString(s, "") }

// started/ended/staged are the scheduler events the view is fed, in the
// shape the scheduler publishes them (events.go).
func started(name, role string, issue, pr int, at time.Time, model string, fallback bool) tea.Msg {
	return eventMsg(scheduler.Event{
		Kind: scheduler.EventSessionStarted, Time: at, Session: name, Role: role,
		Issue: issue, PR: pr, Model: model, Fallback: fallback,
	})
}

func ended(name, role string, issue, pr int, turns int, cost float64) tea.Msg {
	return eventMsg(scheduler.Event{
		Kind: scheduler.EventSessionEnded, Time: fixed, Session: name, Role: role,
		Issue: issue, PR: pr, Outcome: "pr-opened", Turns: turns, CostUSD: cost,
	})
}

func staged(issue int, name string, round int) tea.Msg {
	return eventMsg(scheduler.Event{
		Kind: scheduler.EventStage, Time: fixed, Role: config.RoleDeveloper,
		Issue: issue, Stage: name, Round: round,
	})
}

// The Now panel lists every running session with what a person watching the
// factory needs: who is running it, what it is about, the stage its worker
// is in, how long it has been going, what the work item has spent so far and
// the model it runs on.
func TestNowPanelListsTheRunningSessions(t *testing.T) {
	view := drive(t, Deps{Repo: "acme/widgets"},
		staged(12, "developer", 2),
		started("developer-issue-12-r2", config.RoleDeveloper, 12, 31, fixed.Add(-3*time.Minute-20*time.Second), "opus", false),
		started("product-manager-1", config.RoleProductManager, 0, 0, fixed.Add(-42*time.Second), "sonnet", true),
	)
	for _, want := range []string{
		"developer", "#12", "#31", "developer r2", "3m20s", "opus",
		"product manager", "42s", "sonnet (fallback)",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the Now panel does not mention %q:\n%s", want, view)
		}
	}
}

// A session that has ended leaves the Now panel, and what it reported is
// added to what its work item has spent: the next session of the same issue
// shows the total. claude reports turns and cost only when a session ends,
// so a running session's own numbers are not in it yet.
func TestAFinishedSessionLeavesTheNowPanelAndItsSpendStays(t *testing.T) {
	deps := Deps{Repo: "acme/widgets"}
	view := drive(t, deps,
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed.Add(-time.Minute), "opus", false),
		ended("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 87, 2.41),
	)
	if strings.Contains(view, "developer-issue-12-r1") || !strings.Contains(view, "no sessions running") {
		t.Errorf("the finished session is still in the Now panel:\n%s", view)
	}

	view = drive(t, deps,
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed.Add(-time.Minute), "opus", false),
		ended("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 87, 2.41),
		staged(12, "reviewer", 1),
		started("reviewer-pr-31-r1", config.RoleReviewer, 12, 31, fixed.Add(-30*time.Second), "opus", false),
	)
	for _, want := range []string{"reviewer", "reviewer r1", "87", "$2.41"} {
		if !strings.Contains(view, want) {
			t.Errorf("the Now panel does not carry %q from the finished session:\n%s", want, view)
		}
	}
}

// The Queues panel prints the numbers the scheduler recorded in status.json
// — the same ones `bees status` prints — plus the unread mail per role and
// the countdown to the next GitHub poll.
func TestQueuesPanelShowsTheStatusNumbers(t *testing.T) {
	st := state.Status{
		NextPoll: fixed.Add(2*time.Minute + 30*time.Second),
		Queues: map[string]int{
			"triage": 2, "ready": 4, "in-progress": 1, "review": 1, "approved": 3,
			"features": 5, "feedback": 6, "open_prs": 7, "proposals": 8,
		},
	}
	view := drive(t, Deps{Repo: "acme/widgets"},
		statusMsg{status: st, mail: map[string]int{config.RoleDeveloper: 2}})
	for _, want := range []string{
		"triage          2", "ready           4", "in-progress     1", "review          1",
		"approved        3", "blocked         0", "needs-human     0",
		"features        5", "feedback        6", "open PRs        7",
		// A queue this version does not list by name is still printed.
		"proposals       8",
		"developer 2", "in 2m30s",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the Queues panel does not show %q:\n%s", want, view)
		}
	}
}

// An idle factory reads as an idle factory: no running sessions and no
// queued work are said in words and in zeros, never as an empty box.
func TestEmptyStateReadsAsEmpty(t *testing.T) {
	view := drive(t, Deps{Repo: "acme/widgets"})
	for _, want := range []string{
		"acme/widgets", "Now", "no sessions running", "Queues",
		"triage          0", "ready           0", "open PRs        0",
		"unread mail   none", "next poll     not scheduled yet",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the empty view does not say %q:\n%s", want, view)
		}
	}
}

// Ctrl-C asks the factory to stop polling and drain, and the view stays up
// while it does — a second press gives up on the drain. Nothing but the
// factory stopping quits the program on its own.
func TestCtrlCStopsTheFactoryAndTheViewWaits(t *testing.T) {
	stops := 0
	m := New(Deps{Now: func() time.Time { return fixed }, Stop: func() { stops++ }})
	var view tea.Model = m
	view, _ = view.Update(started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed, "opus", false))

	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if stops != 1 {
		t.Errorf("ctrl-c asked the factory to stop %d times, want 1", stops)
	}
	if cmd != nil {
		t.Error("ctrl-c quit the view instead of waiting for the drain")
	}
	if got := plain(view.View()); !strings.Contains(got, "stopping: waiting for 1 session") {
		t.Errorf("the view does not say it is stopping:\n%s", got)
	}

	if _, cmd = view.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Error("a second ctrl-c did not quit the view")
	}
	if _, cmd = view.Update(Stopped{}); cmd == nil {
		t.Error("the factory stopping did not quit the view")
	}
}

// A status.json that cannot be read never blanks the view: it keeps drawing
// what it last read and says what went wrong.
func TestAFailedStatusReadKeepsTheLastView(t *testing.T) {
	view := drive(t, Deps{Repo: "acme/widgets"},
		statusMsg{status: state.Status{Queues: map[string]int{"ready": 4}}},
		statusMsg{err: errors.New("unexpected end of JSON input")})
	if !strings.Contains(view, "ready           4") {
		t.Errorf("the failed read blanked the queues:\n%s", view)
	}
	if !strings.Contains(view, "unexpected end of JSON input") {
		t.Errorf("the failed read is not reported:\n%s", view)
	}
}

// Every elapsed time and countdown comes from the injected clock, so the
// same messages render the same view however long a test takes (#222).
func TestTheViewIsRenderedFromTheInjectedClock(t *testing.T) {
	msgs := []tea.Msg{
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed.Add(-90*time.Second), "opus", false),
		statusMsg{status: state.Status{NextPoll: fixed.Add(time.Minute)}},
	}
	first := drive(t, Deps{Repo: "acme/widgets"}, msgs...)
	time.Sleep(1100 * time.Millisecond)
	if second := drive(t, Deps{Repo: "acme/widgets"}, msgs...); second != first {
		t.Errorf("the view moved with the wall clock:\ngot\n%s\nwant\n%s", second, first)
	}
	if !strings.Contains(first, "1m30s") {
		t.Errorf("the elapsed time is not measured from the injected clock:\n%s", first)
	}
}

// A row too wide for the terminal is shortened, and what it gives up is the
// model's name: a session running on the fallback model is what a person
// watching wants to see, so the marker survives a model name long enough to
// crowd it out.
func TestALongModelNameIsShortenedAndTheFallbackMarkerIsNot(t *testing.T) {
	view := drive(t, Deps{Repo: "acme/widgets"},
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed, "claude-a-very-long-model-name-5", true))
	if !strings.Contains(view, "(fallback)") {
		t.Errorf("the view dropped the fallback marker to fit the model name:\n%s", view)
	}
	if strings.Contains(view, "claude-a-very-long-model-name-5") {
		t.Errorf("the long model name was not shortened to make room:\n%s", view)
	}
	for _, line := range strings.Split(view, "\n") {
		if w := len([]rune(line)); w > defaultWidth {
			t.Errorf("a %d-column line in a %d-column view: %q", w, defaultWidth, line)
		}
	}
}

// The stage column is cut to its own width, so a long stage name cannot push
// the model column — and with it the (fallback) marker — off the end of the
// row. "pre-review checks (reported)" is what the scheduler publishes for a
// worker sitting in the pre-review gate (developer.go's setChecksGate).
func TestALongStageNameDoesNotPushTheModelColumnOff(t *testing.T) {
	view := drive(t, Deps{Repo: "acme/widgets"},
		staged(12, "pre-review checks (reported)", 1),
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed.Add(-time.Minute), "sonnet", true))
	for _, want := range []string{"pre-review checks (…", "sonnet", "(fallback)"} {
		if !strings.Contains(view, want) {
			t.Errorf("the long stage name crowded %q out of the row:\n%s", want, view)
		}
	}
	for _, line := range strings.Split(view, "\n") {
		if w := len([]rune(line)); w > defaultWidth {
			t.Errorf("a %d-column line in a %d-column view: %q", w, defaultWidth, line)
		}
	}
}
