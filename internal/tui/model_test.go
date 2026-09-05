package tui

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
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

// panelHeight is the terminal drive draws in: tall enough for all five
// panels, so a test that is not about the layout does not have to say so.
// The view's own default is the classic 24 rows, which is shorter than all
// five panels — a test that is about the height sends its own
// WindowSizeMsg, and drive then leaves the size alone.
const panelHeight = 40

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
	if !slices.ContainsFunc(msgs, func(msg tea.Msg) bool {
		_, ok := msg.(tea.WindowSizeMsg)
		return ok
	}) {
		m, _ = m.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	}
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
		Issue: issue, PR: pr, Outcome: "pr-opened", Turns: turns, CostUSD: cost, CostKnown: true,
	})
}

// endedUnknownCost is a session-ended event for a session that ended with no
// result event, so its cost is not a real zero but unpriced (a signalled
// process, most often — see #359).
func endedUnknownCost(name, role string, issue, pr int, turns int) tea.Msg {
	return eventMsg(scheduler.Event{
		Kind: scheduler.EventSessionEnded, Time: fixed, Session: name, Role: role,
		Issue: issue, PR: pr, Outcome: "failed", Turns: turns,
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
		"acme/widgets", "Now", "no sessions running",
		"Recent", "no sessions have finished yet",
		"Needs human", "nothing is waiting for a person",
		"Approved PRs", "no approved pull requests are waiting to be merged",
		"Queues",
		"triage          0", "ready           0", "open PRs        0",
		"unread mail   none", "next poll     not scheduled yet",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the empty view does not say %q:\n%s", want, view)
		}
	}
}

// Ctrl-C asks the factory to stop polling and let the running sessions
// finish, and the view stays up while they do. A second press stops those
// sessions too (Deps.HardStop) and still stays up; only a third leaves the
// terminal early. Nothing but the factory stopping quits the program on its
// own. The footer names the two stops apart, with the count through
// text.Count so one session never reads "1 sessions" (#338).
func TestCtrlCStopsTheFactoryAndTheViewWaits(t *testing.T) {
	stops, hard := 0, 0
	m := New(Deps{Now: func() time.Time { return fixed }, Stop: func() { stops++ }, HardStop: func() { hard++ }})
	var view tea.Model = m
	view, _ = view.Update(started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed, "opus", false))

	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if stops != 1 || hard != 0 {
		t.Errorf("the first ctrl-c stopped %d times and hard-stopped %d, want 1 and 0", stops, hard)
	}
	if cmd != nil {
		t.Error("ctrl-c quit the view instead of waiting for the sessions")
	}
	if got := plain(view.View()); !strings.Contains(got, "stopping: waiting for 1 running session to finish — q or ctrl-c again stops them now") {
		t.Errorf("the view does not say it is cooling down:\n%s", got)
	}

	view, cmd = view.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if hard != 1 {
		t.Errorf("the second ctrl-c hard-stopped %d times, want 1", hard)
	}
	if cmd != nil {
		t.Error("the second ctrl-c quit the view instead of stopping the sessions")
	}
	if got := plain(view.View()); !strings.Contains(got, "stopping 1 running session now") {
		t.Errorf("the view does not say the sessions are being stopped:\n%s", got)
	}

	if _, cmd = view.Update(tea.KeyMsg{Type: tea.KeyCtrlC}); cmd == nil {
		t.Error("a third ctrl-c did not quit the view")
	}
	if hard != 1 {
		t.Errorf("the third ctrl-c hard-stopped again (%d times in all): it only leaves the terminal", hard)
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

// endedAs is a session-ended event carrying everything the Recent panel
// renders: how it ended, what it said about it, what it cost and how long it
// took.
func endedAs(name, role string, issue, pr int, outcome, note string, cost float64, took time.Duration) tea.Msg {
	return eventMsg(scheduler.Event{
		Kind: scheduler.EventSessionEnded, Time: fixed, Session: name, Role: role,
		Issue: issue, PR: pr, Outcome: outcome, Note: note, CostUSD: cost, CostKnown: true, Duration: took,
	})
}

// escalated and approved are what the scheduler records in status.json for
// the two panels that are about people rather than about sessions.
func escalated(n int, title, reason string, since time.Time) state.Escalated {
	return state.Escalated{Issue: n, Title: title, Reason: reason, Since: since}
}

func approvedPR(pr, issue int, title string, since time.Time) state.ApprovedPR {
	return state.ApprovedPR{PR: pr, Issue: issue, Title: title, Since: since}
}

// The Recent panel is what just happened: the sessions that have finished,
// newest first, with how each ended, what it said about it, how long it took
// and what it cost. Every one of those arrives on the session-ended event —
// the view looks nothing up.
func TestRecentPanelListsWhatHasFinished(t *testing.T) {
	view := drive(t, Deps{Repo: "acme/widgets"},
		// A note is a session's own prose and is the last column, so it is
		// what a narrow terminal cuts. Give this one room to be read whole.
		tea.WindowSizeMsg{Width: 130, Height: 40},
		endedAs("project-manager-1", config.RoleProjectManager, 12, 0, "done", "refined and moved to ready", 0.61, 3*time.Minute+2*time.Second),
		endedAs("reviewer-pr-31-r1", config.RoleReviewer, 12, 31, "changes-requested", "tests missing for the error path", 1.18, 6*time.Minute+14*time.Second),
	)
	for _, want := range []string{
		"Recent", "reviewer", "#12", "#31", "changes-requested", "6m14s", "$1.18",
		"tests missing for the error path",
		"project manager", "done", "3m2s", "$0.61", "refined and moved to ready",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the Recent panel does not mention %q:\n%s", want, view)
		}
	}
	// Newest first: the review that just ended is above the refinement.
	if i, j := strings.Index(view, "changes-requested"), strings.Index(view, "refined and moved"); i > j {
		t.Errorf("the Recent panel is oldest first:\n%s", view)
	}
	// A session that reported nothing at all says so rather than showing an
	// empty column.
	blank := drive(t, Deps{Repo: "acme/widgets"},
		endedAs("developer-issue-9-r1", config.RoleDeveloper, 9, 0, "", "", 0, time.Second))
	if !strings.Contains(blank, "no outcome") {
		t.Errorf("a session that reported no outcome renders as a blank cell:\n%s", blank)
	}
}

// recentLines is nowLines for the Recent panel: locate its header by two
// labels only it carries, then the row naming mark.
func recentLines(t *testing.T, view, mark string) (header, row string) {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		switch {
		case header == "" && strings.Contains(line, "outcome") && strings.Contains(line, "took"):
			header = line
		case row == "" && strings.Contains(line, mark):
			row = line
		}
	}
	if header == "" || row == "" {
		t.Fatalf("the Recent panel has no header or no row for %q:\n%s", mark, view)
	}
	return header, row
}

// The Recent panel renders a session's cost the same way the Now panel
// does: "-" for a cost claude never reported (a signalled process, most
// often), and "$0.00" only for a session that genuinely cost nothing.
func TestRecentPanelRendersCostTheSameWayTheNowPanelDoes(t *testing.T) {
	view := drive(t, Deps{Repo: "acme/widgets"},
		endedUnknownCost("developer-issue-9-r1", config.RoleDeveloper, 9, 0, 3),
	)
	header, row := recentLines(t, view, "developer")
	if got := column(t, header, row, "cost"); got != "-" {
		t.Errorf("a session with no reported cost shows %q, want -:\n%s", got, view)
	}
	if strings.Contains(row, "$0.00") {
		t.Errorf("the row prints $0.00 for a cost nothing has reported:\n%s", row)
	}

	view = drive(t, Deps{Repo: "acme/widgets"},
		endedAs("developer-issue-9-r1", config.RoleDeveloper, 9, 0, "pr-opened", "", 0, time.Second),
	)
	header, row = recentLines(t, view, "developer")
	if got := column(t, header, row, "cost"); got != "$0.00" {
		t.Errorf("a session that really cost $0.00 shows %q:\n%s", got, view)
	}

	view = drive(t, Deps{Repo: "acme/widgets"},
		endedAs("developer-issue-9-r1", config.RoleDeveloper, 9, 0, "pr-opened", "", 2.41, time.Second),
	)
	header, row = recentLines(t, view, "developer")
	if got := column(t, header, row, "cost"); got != "$2.41" {
		t.Errorf("a session that cost $2.41 shows %q:\n%s", got, view)
	}
}

// Needs human is the panel that tells a person the factory is stuck and
// waiting for them: which issue, how long it has been waiting and why it was
// given up on. The reason is the one the scheduler recorded when it
// escalated; an issue a person labelled by hand has none, and says so rather
// than leaving a cell that reads like a rendering fault.
func TestNeedsHumanPanelSaysWhichIssueAndWhy(t *testing.T) {
	view := drive(t, Deps{Repo: "acme/widgets"}, statusMsg{status: state.Status{
		NeedsHuman: []state.Escalated{
			escalated(44, "Parser drops a token", "3 review rounds and no approval", fixed.Add(-50*time.Hour)),
			escalated(51, "Flaky worktree test", "", time.Time{}),
		},
	}})
	for _, want := range []string{
		"Needs human", "#44", "2d", "Parser drops a token", "3 review rounds and no approval",
		"#51", "Flaky worktree test", "no reason recorded",
	} {
		if !strings.Contains(view, want) {
			t.Errorf("the Needs human panel does not mention %q:\n%s", want, view)
		}
	}
}

// Approved PRs is what is waiting for a person to merge, oldest first — the
// order the scheduler put them in.
func TestApprovedPanelListsWhatIsWaitingToBeMerged(t *testing.T) {
	view := drive(t, Deps{Repo: "acme/widgets"}, statusMsg{status: state.Status{
		Approved: []state.ApprovedPR{
			approvedPR(60, 20, "Retry a session that hit the account limit", fixed.Add(-30*time.Hour)),
			approvedPR(62, 22, "Docs: the release workflow", fixed.Add(-3*time.Hour)),
		},
	}})
	for _, want := range []string{"Approved PRs", "#60", "#20", "1d", "Retry a session", "#62", "#22", "3h", "Docs: the release"} {
		if !strings.Contains(view, want) {
			t.Errorf("the Approved PRs panel does not mention %q:\n%s", want, view)
		}
	}
	if i, j := strings.Index(view, "#60"), strings.Index(view, "#62"); i > j {
		t.Errorf("the Approved PRs panel does not keep the order it was given:\n%s", view)
	}
}

// The header says whenever dispatch is paused and why, with the numbers
// behind it: a paused factory whose Now panel is empty otherwise reads
// exactly like an idle one (#367). The clock the test drives against is in
// time.Local, like schedulerLine's own tests, because the notice prints a
// wall-clock time.
func TestHeaderShowsWhyDispatchIsPaused(t *testing.T) {
	local := time.Date(2026, 8, 31, 10, 3, 0, 0, time.Local)
	deps := Deps{Repo: "acme/widgets", Now: func() time.Time { return local }}
	for _, tc := range []struct {
		name   string
		status state.Status
		want   string
	}{
		{
			name:   "claude session limit",
			status: state.Status{LimitPausedUntil: local.Add(42 * time.Minute)},
			want:   "⏸ claude session limit until 10:45 (in 42m)",
		},
		{
			name:   "daily budget",
			status: state.Status{BudgetPaused: true, DaySpendUSD: 323.8, DayBudgetUSD: 300},
			want:   "⏸ daily budget ($323.80 / $300.00)",
		},
		{
			// The session-limit pause is the harder stop and wins while it
			// is in force.
			name: "both",
			status: state.Status{LimitPausedUntil: local.Add(42 * time.Minute),
				BudgetPaused: true, DaySpendUSD: 323.8, DayBudgetUSD: 300},
			want: "⏸ claude session limit until 10:45 (in 42m)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			view := drive(t, deps, statusMsg{status: tc.status})
			if !strings.Contains(view, tc.want) {
				t.Errorf("the header does not say %q:\n%s", tc.want, view)
			}
		})
	}
}

// With no daily budget configured the header carries no trace of a reading
// or a pause: an unset budget is not "$0.00 / $0.00".
func TestHeaderIsUnchangedWithNoBudget(t *testing.T) {
	deps := Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }}
	bare := drive(t, deps)
	firstLine := func(v string) string { return strings.SplitN(v, "\n", 2)[0] }
	if strings.Contains(firstLine(bare), "⏸") {
		t.Errorf("the header shows a pause the factory is not observing:\n%s", firstLine(bare))
	}
	if strings.Contains(firstLine(bare), "daily budget") {
		t.Errorf("the header shows a budget reading with no budget configured:\n%s", firstLine(bare))
	}
}

// With a daily budget configured and dispatch running (not paused) the
// header carries the reading, so a person watching the live view sees the
// spend against the budget all the time, not only once it stops the
// factory.
func TestHeaderShowsTheDailyBudgetWhenNotPaused(t *testing.T) {
	deps := Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }}
	view := drive(t, deps, statusMsg{status: state.Status{DaySpendUSD: 0.12, DayBudgetUSD: 5}})
	firstLine := strings.SplitN(view, "\n", 2)[0]
	if !strings.Contains(firstLine, "daily budget: $0.12 / $5.00") {
		t.Errorf("the header does not show the daily budget reading:\n%s", firstLine)
	}
	if strings.Contains(firstLine, "⏸") {
		t.Errorf("the header shows a pause the factory is not observing:\n%s", firstLine)
	}
}

// The pause notice outranks the reading: a paused factory must not show a
// calm reading next to the pause, and the pause notice already carries the
// same two numbers.
func TestHeaderShowsThePauseNotTheReadingWhilePaused(t *testing.T) {
	deps := Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }}
	view := drive(t, deps, statusMsg{status: state.Status{BudgetPaused: true, DaySpendUSD: 323.8, DayBudgetUSD: 300}})
	firstLine := strings.SplitN(view, "\n", 2)[0]
	if !strings.Contains(firstLine, "⏸ daily budget ($323.80 / $300.00)") {
		t.Errorf("the header does not show the pause notice:\n%s", firstLine)
	}
	if strings.Contains(firstLine, "daily budget:") {
		t.Errorf("the header shows the reading alongside the pause notice:\n%s", firstLine)
	}
}

// The pause notice's ⏸ is a wide rune, and the header's gap already clamps
// at a minimum of one column, so a narrow terminal degrades instead of
// panicking.
func TestHeaderDegradesInANarrowTerminal(t *testing.T) {
	deps := Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }}
	view := drive(t, deps,
		tea.WindowSizeMsg{Width: 20, Height: panelHeight},
		statusMsg{status: state.Status{BudgetPaused: true, DaySpendUSD: 323.8, DayBudgetUSD: 300}})
	if !strings.Contains(view, "⏸") {
		t.Errorf("a narrow terminal drops the pause notice instead of degrading:\n%s", view)
	}
}

// runCmd runs the command a key returned and gives back the message it
// produced. The keys that do something outside the model — stopping a
// session, opening a browser — do it in a command so the view keeps drawing
// while they take their time.
func runCmd(t *testing.T, cmd tea.Cmd) tea.Msg {
	t.Helper()
	if cmd == nil {
		t.Fatal("the key returned no command")
	}
	return cmd()
}

// q stops the factory exactly as Ctrl-C does: the first press asks it to
// stop polling and let the sessions finish and the view stays up, the
// second stops the sessions too. It is a key a person can find without
// reading anything, so the footer says what it does.
func TestQStopsTheFactoryLikeCtrlC(t *testing.T) {
	stops, hard := 0, 0
	var view tea.Model = New(Deps{Now: func() time.Time { return fixed }, Stop: func() { stops++ }, HardStop: func() { hard++ }})
	view, _ = view.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	view, _ = view.Update(started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed, "opus", false))
	if got := plain(view.View()); !strings.Contains(got, "q or ctrl-c stops (sessions finish)") {
		t.Errorf("the footer does not say what q does:\n%s", got)
	}

	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if stops != 1 {
		t.Errorf("q asked the factory to stop %d times, want 1", stops)
	}
	if cmd != nil {
		t.Error("q quit the view instead of waiting for the sessions")
	}
	if got := plain(view.View()); !strings.Contains(got, "stopping: waiting for 1 running session") {
		t.Errorf("the view does not say it is stopping:\n%s", got)
	}
	if _, cmd = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}); cmd != nil || hard != 1 {
		t.Errorf("a second q must stop the sessions and stay up: hard stops %d, cmd %v", hard, cmd)
	}
}

// k stops the selected session and hands its issue to a person, through the
// scheduler's own kill path. It asks first: it is the one key here that
// throws work away, so a single press says what it is about to do and the
// second does it.
func TestKillStopsTheSelectedSessionAfterAsking(t *testing.T) {
	var killed []string
	deps := Deps{Repo: "acme/widgets", Kill: func(name string) error {
		killed = append(killed, name)
		return nil
	}}
	var view tea.Model = New(deps)
	view, _ = view.Update(started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed, "opus", false))

	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if cmd != nil || len(killed) != 0 {
		t.Fatalf("the first k stopped %v without asking", killed)
	}
	if got := plain(view.View()); !strings.Contains(got, "k again to stop developer-issue-12-r1") {
		t.Errorf("the first k does not say what the second will do:\n%s", got)
	}

	view, cmd = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if msg := runCmd(t, cmd); msg != (actedMsg{note: "stopped developer-issue-12-r1"}) {
		t.Errorf("the kill reported %+v", msg)
	}
	if len(killed) != 1 || killed[0] != "developer-issue-12-r1" {
		t.Fatalf("k stopped %v, want the selected session", killed)
	}
	// What the kill reported is what the footer says.
	view, _ = view.Update(actedMsg{note: "stopped developer-issue-12-r1"})
	if got := plain(view.View()); !strings.Contains(got, "stopped developer-issue-12-r1") {
		t.Errorf("the footer does not report what the kill did:\n%s", got)
	}
	// And a failure is reported rather than swallowed.
	fails := New(Deps{Repo: "acme/widgets", Kill: func(string) error { return errors.New("no such process") }})
	var f tea.Model = fails
	f, _ = f.Update(started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed, "opus", false))
	f, _ = f.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	_, cmd = f.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if msg, ok := runCmd(t, cmd).(actedMsg); !ok || !strings.Contains(msg.note, "no such process") {
		t.Errorf("a failed kill reported %+v", msg)
	}
}

// Only a running session can be stopped: k on any other row says so instead
// of stopping something else, and asks nothing.
func TestKillOnARowThatIsNotASessionStopsNothing(t *testing.T) {
	killed := 0
	view := drive(t, Deps{Repo: "acme/widgets", Kill: func(string) error { killed++; return nil }},
		statusMsg{status: state.Status{NeedsHuman: []state.Escalated{escalated(44, "Parser", "gave up", fixed)}}},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}},
		tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if killed != 0 {
		t.Errorf("k stopped %d sessions from a row that is not one", killed)
	}
	if !strings.Contains(view, "select a running session") {
		t.Errorf("k does not say why it stopped nothing:\n%s", view)
	}
}

// o opens the selected row on GitHub. One URL shape serves an issue and a
// pull request alike, because GitHub redirects between them — so the row
// decides the number and nothing has to know which kind it is.
func TestOpenShowsTheSelectedIssueOrPullRequest(t *testing.T) {
	for _, tc := range []struct {
		name string
		down int
		want string
	}{
		{"a running session opens its pull request", 0, "https://github.com/acme/widgets/issues/31"},
		{"an escalated issue opens the issue", 1, "https://github.com/acme/widgets/issues/44"},
		{"an approved pull request opens the pull request", 2, "https://github.com/acme/widgets/issues/60"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opened []string
			var view tea.Model = New(Deps{Repo: "acme/widgets", Open: func(u string) error {
				opened = append(opened, u)
				return nil
			}})
			view, _ = view.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
			view, _ = view.Update(started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed, "opus", false))
			view, _ = view.Update(statusMsg{status: state.Status{
				NeedsHuman: []state.Escalated{escalated(44, "Parser", "gave up", fixed)},
				Approved:   []state.ApprovedPR{approvedPR(60, 20, "Retry", fixed)},
			}})
			for range tc.down {
				view, _ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
			}
			_, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'o'}})
			runCmd(t, cmd)
			if len(opened) != 1 || opened[0] != tc.want {
				t.Errorf("o opened %v, want %q", opened, tc.want)
			}
		})
	}
}

// The selection is one cursor over every panel's rows in turn, so a person
// reaches everything with two keys. It is drawn where it is, and it never
// runs off the end of a list that has shrunk under it.
func TestTheSelectionMovesThroughEveryPanelAndStaysInside(t *testing.T) {
	deps := Deps{Repo: "acme/widgets"}
	msgs := []tea.Msg{
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed, "opus", false),
		endedAs("reviewer-pr-31-r1", config.RoleReviewer, 12, 31, "approved", "looks good", 0.5, time.Minute),
		statusMsg{status: state.Status{
			NeedsHuman: []state.Escalated{escalated(44, "Parser", "gave up", fixed)},
			Approved:   []state.ApprovedPR{approvedPR(60, 20, "Retry", fixed)},
		}},
	}
	// The four rows, in the order the panels draw them.
	for i, want := range []string{"developer-issue-12", "approved", "#44", "#60"} {
		down := make([]tea.Msg, i)
		for j := range down {
			down[j] = tea.KeyMsg{Type: tea.KeyDown}
		}
		view := drive(t, deps, append(append([]tea.Msg{}, msgs...), down...)...)
		var marked string
		for _, line := range strings.Split(view, "\n") {
			if strings.Contains(line, "▸ ") {
				marked = line
			}
		}
		switch {
		case marked == "":
			t.Fatalf("%d rows down, nothing is marked as selected:\n%s", i, view)
		case want == "developer-issue-12" && !strings.Contains(marked, "developer"):
			t.Errorf("%d rows down, the marked row is %q", i, marked)
		case want != "developer-issue-12" && !strings.Contains(marked, want):
			t.Errorf("%d rows down, the marked row is %q, want one about %q", i, marked, want)
		}
	}
	// The cursor is on the last row; the queues emptying under it must not
	// leave it pointing past the end.
	var view tea.Model = New(deps)
	for _, msg := range msgs {
		view, _ = view.Update(msg)
	}
	for range 3 {
		view, _ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	view, _ = view.Update(statusMsg{status: state.Status{}})
	m, ok := view.(Model)
	if !ok {
		t.Fatal("the model is not a Model")
	}
	if n := len(m.targets()); m.cursor >= n {
		t.Errorf("the cursor is on row %d of %d rows", m.cursor, n)
	}
}

// share gives a row to every list that has something in it — a list with
// nothing says so in a line of its own, and takes no rows for it — and hands
// what is left round the lists a row at a time, so a long list cannot starve
// a short one. Layout makes room for the floor rows before it calls share;
// asking for less than that is what makes a panel go entirely.
func TestShareGivesEveryListARowAndSpreadsTheRest(t *testing.T) {
	for _, tc := range []struct {
		name  string
		want  []int
		avail int
		got   []int
	}{
		{"everything fits", []int{2, 1, 0, 3}, 20, []int{2, 1, 0, 3}},
		{"an empty list takes no rows", []int{0, 0, 0, 0}, 6, []int{0, 0, 0, 0}},
		{"nothing fits", []int{4, 4, 4, 4}, 0, []int{0, 0, 0, 0}},
		{"a long list does not starve a short one", []int{20, 1, 1, 2}, 8, []int{4, 1, 1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := share(tc.want, tc.avail)
			if len(got) != len(tc.got) {
				t.Fatalf("share(%v, %d) = %v", tc.want, tc.avail, got)
			}
			for i := range got {
				if got[i] != tc.got[i] {
					t.Fatalf("share(%v, %d) = %v, want %v", tc.want, tc.avail, got, tc.got)
				}
			}
		})
	}
}

// A list with more entries than rows says how many did not fit, so what is
// on screen and what is not add up. With a single row there is no space for
// that line and the row goes to an entry: one of the things waiting for a
// person says more than the news that some are.
func TestAListTooLongForItsPanelAccountsForTheRest(t *testing.T) {
	row := func(i int) string { return fmt.Sprintf("row%d", i) }
	got := strings.Join(listRows(5, 3, row), "|")
	if want := "row0|row1|  … 3 more"; got != want {
		t.Errorf("listRows(5, 3) = %q, want %q", got, want)
	}
	if got := strings.Join(listRows(5, 1, row), "|"); got != "row0" {
		t.Errorf("listRows(5, 1) = %q, want one entry and no room for anything else", got)
	}
	if got := strings.Join(listRows(2, 3, row), "|"); got != "row0|row1" {
		t.Errorf("listRows(2, 3) = %q, want both entries and no accounting line", got)
	}
}

// The view never draws more lines than the terminal has. Bubble Tea keeps
// the LAST height lines of an over-long view, so one line too many costs the
// header and one panel too many costs the Now panel: the person watching
// loses what is running, which is the thing they are watching for.
func TestTheViewFitsTheTerminalItIsDrawnIn(t *testing.T) {
	busy := []tea.Msg{
		started("developer-issue-1-r1", config.RoleDeveloper, 1, 2, fixed, "opus", false),
		eventMsg(scheduler.Event{Kind: scheduler.EventSessionEnded, Time: fixed, Session: "x",
			Role: config.RoleReviewer, Issue: 3, Outcome: "approved"}),
		statusMsg{status: state.Status{
			NeedsHuman: []state.Escalated{{Issue: 7, Title: "t", Since: fixed.Add(-time.Hour)}},
			Approved:   []state.ApprovedPR{{PR: 9, Issue: 8, Title: "t", Since: fixed.Add(-time.Hour)}},
		}},
	}
	// A list with more entries than rows spends one of its rows saying how
	// many did not fit, which has to come out of the budget rather than on
	// top of it: four panels each one line over is the header and the Now
	// panel gone.
	crowded := []tea.Msg{}
	for i := range 4 {
		crowded = append(crowded, started(
			fmt.Sprintf("developer-issue-%d-r1", 10+i), config.RoleDeveloper, 10+i, 20+i, fixed, "opus", false))
		crowded = append(crowded, eventMsg(scheduler.Event{Kind: scheduler.EventSessionEnded, Time: fixed,
			Session: fmt.Sprintf("s%d", i), Role: config.RoleReviewer, Issue: 30 + i, Outcome: "approved"}))
	}
	st := state.Status{}
	for i := range 4 {
		st.NeedsHuman = append(st.NeedsHuman, state.Escalated{Issue: 70 + i, Title: "t", Since: fixed.Add(-time.Hour)})
		st.Approved = append(st.Approved, state.ApprovedPR{PR: 90 + i, Issue: 80 + i, Title: "t", Since: fixed.Add(-time.Hour)})
	}
	crowded = append(crowded, statusMsg{status: st})

	for _, tc := range []struct {
		name string
		msgs []tea.Msg
	}{
		{"an idle factory", nil},
		{"something in every panel", busy},
		{"more in every panel than fits", crowded},
	} {
		for _, h := range []int{20, 24, 30, 40} {
			view := drive(t, Deps{Repo: "acme/widgets"},
				append([]tea.Msg{tea.WindowSizeMsg{Width: 100, Height: h}}, tc.msgs...)...)
			if got := strings.Count(view, "\n") + 1; got > h {
				t.Errorf("%s in a %d-row terminal draws %d lines: the top %d are cut off, header first",
					tc.name, h, got, got-h)
			}
		}
	}
}

// The header, the Now panel, the Queues panel and the footer are the last
// things a short terminal loses: the lists below Now go from the bottom, and
// Queues goes on counting what they would have listed.
func TestAShortTerminalDropsTheListsFromTheBottom(t *testing.T) {
	for _, tc := range []struct {
		height int
		gone   []string
	}{
		{40, nil},
		{24, []string{"Approved PRs"}},
		{20, []string{"Approved PRs", "Needs human"}},
	} {
		view := drive(t, Deps{Repo: "acme/widgets"}, tea.WindowSizeMsg{Width: 100, Height: tc.height})
		for _, want := range []string{"busybees", "Now", "Queues", "needs-human     0", "q or ctrl-c"} {
			if !strings.Contains(view, want) {
				t.Errorf("a %d-row terminal lost %q, which it should keep last of all:\n%s", tc.height, want, view)
			}
		}
		for _, gone := range tc.gone {
			if strings.Contains(view, gone) {
				t.Errorf("a %d-row terminal still draws the %s panel:\n%s", tc.height, gone, view)
			}
		}
	}
}

// The selection is always on a row that is drawn. A list squeezed by a short
// terminal draws its first rows and accounts for the rest, but the cursor
// walks every entry, so ↓ past the last drawn row leaves nothing marked:
// the key looks dead and k then names a session that is not on screen.
func TestTheSelectionIsAlwaysOnARowThatIsDrawn(t *testing.T) {
	msgs := []tea.Msg{tea.WindowSizeMsg{Width: 100, Height: 40}}
	for i := range 3 {
		msgs = append(msgs, started(
			"developer-issue-"+string(rune('1'+i))+"-r1", config.RoleDeveloper, 10+i, 30+i, fixed, "opus", false))
	}
	for i := range 8 {
		msgs = append(msgs, eventMsg(scheduler.Event{
			Kind: scheduler.EventSessionEnded, Time: fixed, Session: "s" + string(rune('a'+i)),
			Role: config.RoleReviewer, Issue: 50 + i, Outcome: "approved", Note: "fine"}))
	}
	st := state.Status{}
	for i := range 3 {
		st.NeedsHuman = append(st.NeedsHuman, state.Escalated{Issue: 70 + i, Title: "t", Reason: "why", Since: fixed.Add(-time.Hour)})
		st.Approved = append(st.Approved, state.ApprovedPR{PR: 90 + i, Issue: 80 + i, Title: "t", Since: fixed.Add(-time.Hour)})
	}
	msgs = append(msgs, statusMsg{status: st})

	// And in a terminal short enough that whole panels are dropped: their
	// entries are not on screen at all, so they are not rows to select.
	for _, h := range []int{40, 24, 20} {
		var view tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }})
		for _, msg := range append([]tea.Msg{tea.WindowSizeMsg{Width: 100, Height: h}}, msgs[1:]...) {
			view, _ = view.Update(msg)
		}
		for n := 1; n < 17; n++ {
			view, _ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
			if got := plain(view.View()); !strings.Contains(got, "▸ ") {
				t.Fatalf("%d rows down in a %d-row terminal, nothing on screen is marked as selected:\n%s", n, h, got)
			}
		}
	}
}

// The Now panel marks the row enter opens and k stops, and marks only that
// one: the cursor is the whole of what tells a person which session they are
// about to act on. It is the same ▸ every other panel draws, because there
// is one selection over the whole view.
func TestTheNowPanelMarksTheSelectedRow(t *testing.T) {
	out := drive(t, Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }},
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed, "opus", false),
		started("reviewer-pr-33-r1", config.RoleReviewer, 14, 33, fixed, "sonnet", false),
		tea.KeyMsg{Type: tea.KeyDown},
	)
	var marked []string
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "│ ▸ ") {
			marked = append(marked, strings.TrimSpace(line))
		}
	}
	if len(marked) != 1 || !strings.Contains(marked[0], "reviewer") {
		t.Errorf("the Now panel marks %q, want the one row the cursor is on:\n%s", marked, out)
	}
}

// The selection survives the terminal being resized. targets() enumerates
// only the rows the layout draws, so a shorter terminal has fewer of them:
// without a clamp the cursor is left past the end, nothing is marked, o and
// k both say there is nothing selected, and ↑ — the key a person reaches for
// — walks back through rows that are not there before anything reappears.
func TestResizingTheTerminalKeepsTheSelectionOnScreen(t *testing.T) {
	msgs := []tea.Msg{tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight}}
	for i := range 3 {
		msgs = append(msgs, started(
			fmt.Sprintf("developer-issue-%d-r1", 10+i), config.RoleDeveloper, 10+i, 30+i, fixed, "opus", false))
	}
	for i := range 8 {
		msgs = append(msgs, endedAs(
			fmt.Sprintf("reviewer-pr-%d-r1", 50+i), config.RoleReviewer, 50+i, 0, "approved", "fine", 0.1, time.Minute))
	}
	st := state.Status{}
	for i := range 3 {
		st.NeedsHuman = append(st.NeedsHuman, escalated(70+i, "stuck", "gave up", fixed.Add(-time.Hour)))
		st.Approved = append(st.Approved, approvedPR(90+i, 80+i, "waiting", fixed.Add(-time.Hour)))
	}
	msgs = append(msgs, statusMsg{status: st})

	var view tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }})
	for _, msg := range msgs {
		view, _ = view.Update(msg)
	}
	for range 12 {
		view, _ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	for _, h := range []int{24, 20} {
		view, _ = view.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: h})
		if got := plain(view.View()); !strings.Contains(got, "▸ ") {
			t.Fatalf("after the terminal shrank to %d rows, nothing on screen is marked as selected:\n%s", h, got)
		}
	}
}

// A list panel that is drawn always has a row to draw in. layout makes room
// for one row per non-empty list before it shares the rest out; without that
// floor a panel can be drawn with its column header and nothing under it,
// four lines of box saying nothing, while a panel below it still lists rows.
func TestADrawnListPanelAlwaysHasARow(t *testing.T) {
	base := []tea.Msg{
		started("developer-issue-1-r1", config.RoleDeveloper, 10, 30, fixed, "opus", false),
		started("developer-issue-2-r1", config.RoleDeveloper, 11, 31, fixed, "opus", false),
	}
	for i := range 4 {
		base = append(base, endedAs(
			fmt.Sprintf("reviewer-pr-%d-r1", 50+i), config.RoleReviewer, 50+i, 0, "approved", "fine", 0.1, time.Minute))
	}
	st := state.Status{}
	for i := range 2 {
		st.NeedsHuman = append(st.NeedsHuman, escalated(70+i, "stuck", "gave up", fixed.Add(-time.Hour)))
		st.Approved = append(st.Approved, approvedPR(90+i, 80+i, "waiting", fixed.Add(-time.Hour)))
	}
	base = append(base, statusMsg{status: st})

	// Every column header of a list panel, so an empty one is spotted
	// whatever the panel.
	headers := []string{"role             issue pr    stage", "role             issue pr    outcome",
		"issue waiting", "pr    issue open"}
	for h := 14; h <= 30; h++ {
		msgs := append([]tea.Msg{tea.WindowSizeMsg{Width: defaultWidth, Height: h}}, base...)
		lines := strings.Split(drive(t, Deps{Repo: "acme/widgets"}, msgs...), "\n")
		for i := 0; i+1 < len(lines); i++ {
			isHeader := false
			for _, want := range headers {
				isHeader = isHeader || strings.Contains(lines[i], want)
			}
			if isHeader && strings.Contains(lines[i+1], "╰") {
				t.Errorf("in a %d-row terminal a panel is drawn with a column header and no rows:\n%s",
					h, strings.Join(lines, "\n"))
			}
		}
	}
}

// The confirmation names one session and stops that one. The cursor is a
// flat index over rows the factory moves under it, so an earlier session
// finishing between the two k presses shifts every later one up a row: the
// second press must act on the name the first one showed, not on whatever
// the cursor points at by then (#308).
func TestKillStopsTheSessionTheConfirmationNamed(t *testing.T) {
	var killed []string
	var view tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed },
		Kill: func(name string) error { killed = append(killed, name); return nil }})
	view, _ = view.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	view, _ = view.Update(started("developer-A", config.RoleDeveloper, 10, 0, fixed, "sonnet", false))
	view, _ = view.Update(started("developer-B", config.RoleDeveloper, 20, 0, fixed, "sonnet", false))
	view, _ = view.Update(started("developer-C", config.RoleDeveloper, 30, 0, fixed, "sonnet", false))

	view, _ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if cmd != nil || len(killed) != 0 {
		t.Fatalf("the first k stopped %v without asking", killed)
	}
	if got := plain(view.View()); !strings.Contains(got, "k again to stop developer-B and hand #20 to a person") {
		t.Fatalf("the confirmation does not name the selected session:\n%s", got)
	}

	// An unrelated session earlier in the Now panel finishes: developer-B
	// and developer-C move up a row, and the cursor does not move with them.
	view, _ = view.Update(ended("developer-A", config.RoleDeveloper, 10, 0, 4, 0.02))
	if s, ok := view.(Model).selection(); !ok || s.name != "developer-C" {
		t.Fatalf("the fixture does not reproduce the drift: the cursor is on %+v", s)
	}

	view, cmd = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if msg := runCmd(t, cmd); msg != (actedMsg{note: "stopped developer-B"}) {
		t.Errorf("the kill reported %+v", msg)
	}
	if !slices.Equal(killed, []string{"developer-B"}) {
		t.Errorf("k stopped %v, want only the session the confirmation named", killed)
	}
	if got := plain(view.View()); !strings.Contains(got, "stopping developer-B") {
		t.Errorf("the view does not say which session is being stopped:\n%s", got)
	}
}

// A confirmation whose session has finished before the second press stops
// nothing: the person read that name, and stopping whatever is selected
// instead is the bug this asks about (#308).
func TestKillStopsNothingWhenTheNamedSessionHasFinished(t *testing.T) {
	var killed []string
	var view tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed },
		Kill: func(name string) error { killed = append(killed, name); return nil }})
	view, _ = view.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	view, _ = view.Update(started("developer-A", config.RoleDeveloper, 10, 0, fixed, "sonnet", false))
	view, _ = view.Update(started("developer-B", config.RoleDeveloper, 20, 0, fixed, "sonnet", false))
	view, _ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	view, _ = view.Update(ended("developer-A", config.RoleDeveloper, 10, 0, 4, 0.02))

	// The command a firing k returns is what stops a session, so a test
	// that only looks at the model cannot tell a kill from a refusal: run
	// whatever comes back.
	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if cmd != nil {
		if msg := runCmd(t, cmd); msg != nil {
			t.Errorf("k reported %+v after the session it named had finished", msg)
		}
	}
	if len(killed) != 0 {
		t.Errorf("k stopped %v after the session it named had finished", killed)
	}
	if got := plain(view.View()); !strings.Contains(got, "developer-A has finished; nothing was stopped") {
		t.Errorf("the view does not say why nothing was stopped:\n%s", got)
	}
}

// Moving the cursor disarms the confirmation, so k on another session asks
// about that one instead of firing at the one named before (#308).
func TestKillOnAnotherSessionAsksAgain(t *testing.T) {
	var killed []string
	var view tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed },
		Kill: func(name string) error { killed = append(killed, name); return nil }})
	view, _ = view.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	view, _ = view.Update(started("developer-A", config.RoleDeveloper, 10, 0, fixed, "sonnet", false))
	view, _ = view.Update(started("developer-B", config.RoleDeveloper, 20, 0, fixed, "sonnet", false))

	view, _ = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	view, _ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if cmd != nil || len(killed) != 0 {
		t.Fatalf("k on another session stopped %v instead of asking about it", killed)
	}
	if got := plain(view.View()); !strings.Contains(got, "k again to stop developer-B and hand #20 to a person") {
		t.Fatalf("k does not ask about the session that is selected now:\n%s", got)
	}

	_, cmd = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'k'}})
	if msg := runCmd(t, cmd); msg != (actedMsg{note: "stopped developer-B"}) {
		t.Errorf("the kill reported %+v", msg)
	}
	if !slices.Equal(killed, []string{"developer-B"}) {
		t.Errorf("k stopped %v, want the session it asked about", killed)
	}
}

// nowLines picks the Now panel's header and the row naming mark out of a
// rendered view. Both are drawn inside the same box, so they share a border
// prefix and a column of one is a column of the other.
func nowLines(t *testing.T, view, mark string) (header, row string) {
	t.Helper()
	for _, line := range strings.Split(view, "\n") {
		switch {
		case header == "" && strings.Contains(line, "elapsed") && strings.Contains(line, "turns"):
			header = line
		case row == "" && strings.Contains(line, mark):
			row = line
		}
	}
	if header == "" || row == "" {
		t.Fatalf("the Now panel has no header or no row for %q:\n%s", mark, view)
	}
	return header, row
}

// column returns the value of a Now panel row's column, found by where the
// header's own label ends: every column but the last is right-aligned and
// the header and the rows are laid out by one nowRow call, so a label's
// right edge is its cells' right edge. A cell formatted anywhere else would
// not end there, and the token this cuts out would be the wrong one.
//
// Columns are counted in runes, not bytes: the cursor's ▸ is one column
// wide and three bytes long.
func column(t *testing.T, header, row, label string) string {
	t.Helper()
	end := at(t, header, label) + len([]rune(label))
	r := []rune(row)
	if end > len(r) {
		t.Fatalf("the %q column ends past the row:\nheader %q\nrow    %q", label, header, row)
	}
	fields := strings.Fields(string(r[:end]))
	if len(fields) == 0 {
		t.Fatalf("the %q column of the row is empty:\nheader %q\nrow    %q", label, header, row)
	}
	return fields[len(fields)-1]
}

// at is the column want starts in, in runes.
func at(t *testing.T, line, want string) int {
	t.Helper()
	i := strings.Index(line, want)
	if i < 0 {
		t.Fatalf("no %q in %q", want, line)
	}
	return len([]rune(line[:i]))
}

// A running session's turns come from its own transcript. claude reports
// num_turns in the result event of its stream and nothing before it, so a
// session that has not ended has no reported turn count — and 0 in that
// column reads as "doing nothing" over a session doing real work (#313).
// The count is taken on the refresh tick, the same one that re-reads
// status.json, not on every redraw.
func TestTheNowPanelCountsARunningSessionsTurns(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue", "grepping", "editing", "running the tests", "pushing")

	var m tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }})
	m, _ = m.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	m, _ = m.Update(watched("developer-issue-12-r1", config.RoleDeveloper, 12, 31, dir))

	var cmd tea.Cmd
	for i := 0; i < refreshEvery; i++ {
		m, cmd = m.Update(tickMsg(fixed))
	}
	m, _ = run(t, m, cmd)

	view := plain(m.View())
	header, row := nowLines(t, view, "developer")
	if got := column(t, header, row, "turns"); got != "5" {
		t.Errorf("the Now panel shows %q turns for a session whose transcript holds 5 assistant messages:\n%s", got, view)
	}
	// The columns still line up: the last one starts where its header does.
	if want, got := at(t, header, "model"), at(t, row, "opus"); want != got {
		t.Errorf("the model column starts at %d in the row and %d in the header:\n%s", got, want, view)
	}
}

// The live count is only ever a stand-in. When the session ends, the number
// claude reports takes its place: the running entry is dropped in the same
// event that adds the real total, so the two are never added together.
func TestALiveTurnCountIsReplacedByTheOneTheSessionReports(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue", "grepping", "editing", "running the tests", "pushing")

	var m tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }})
	m, _ = m.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	m, _ = m.Update(watched("developer-issue-12-r1", config.RoleDeveloper, 12, 31, dir))
	var cmd tea.Cmd
	for i := 0; i < refreshEvery; i++ {
		m, cmd = m.Update(tickMsg(fixed))
	}
	m, _ = run(t, m, cmd)
	// The live count must really have been taken, or this proves nothing.
	view := plain(m.View())
	header, row := nowLines(t, view, "developer")
	if got := column(t, header, row, "turns"); got != "5" {
		t.Fatalf("the running session was not counted (%q turns):\n%s", got, view)
	}

	m, _ = m.Update(ended("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 40, 1.25))
	m, _ = m.Update(staged(12, "reviewer", 1))
	m, _ = m.Update(started("reviewer-pr-31-r1", config.RoleReviewer, 12, 31, fixed.Add(-30*time.Second), "opus", false))

	view = plain(m.View())
	header, row = nowLines(t, view, "reviewer r1")
	if got := column(t, header, row, "turns"); got != "40" {
		t.Errorf("the next session of issue #12 shows %q turns, want the 40 the finished one reported:\n%s", got, view)
	}
}

// An unknown cost is not a cost of zero. Until a session of the work item
// has ended and said what it cost, the cell says "-" the way an unknown
// issue or PR does; a session that really did cost nothing still prints
// $0.00.
func TestAnUnknownCostIsNotZero(t *testing.T) {
	deps := Deps{Repo: "acme/widgets"}
	view := drive(t, deps,
		staged(12, "developer", 1),
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed.Add(-time.Minute), "opus", false),
	)
	header, row := nowLines(t, view, "developer r1")
	if got := column(t, header, row, "cost"); got != "-" {
		t.Errorf("a work item no session has finished on shows a cost of %q, want -:\n%s", got, view)
	}
	if strings.Contains(row, "$0.00") {
		t.Errorf("the row prints $0.00 for a cost nothing has reported yet:\n%s", row)
	}

	view = drive(t, deps,
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed.Add(-time.Minute), "opus", false),
		ended("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 3, 0),
		staged(12, "reviewer", 1),
		started("reviewer-pr-31-r1", config.RoleReviewer, 12, 31, fixed.Add(-30*time.Second), "opus", false),
	)
	header, row = nowLines(t, view, "reviewer r1")
	if got := column(t, header, row, "cost"); got != "$0.00" {
		t.Errorf("a work item whose session reported $0.00 shows a cost of %q:\n%s", got, view)
	}

	// A session that ended with no result event (a signalled process, most
	// often) reported no cost at all: the work item's spend is still
	// unknown, not zero, exactly like the case before any session finished.
	view = drive(t, deps,
		started("developer-issue-12-r1", config.RoleDeveloper, 12, 31, fixed.Add(-time.Minute), "opus", false),
		endedUnknownCost("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 3),
		staged(12, "reviewer", 1),
		started("reviewer-pr-31-r1", config.RoleReviewer, 12, 31, fixed.Add(-30*time.Second), "opus", false),
	)
	header, row = nowLines(t, view, "reviewer r1")
	if got := column(t, header, row, "cost"); got != "-" {
		t.Errorf("a work item whose only finished session reported no cost shows %q, want -:\n%s", got, view)
	}
}
