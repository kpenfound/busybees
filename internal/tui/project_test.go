package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/state"
)

// in tags a message with the project it is from: the event or status read
// of project p's scheduler, the way waitForEvent and refresh tag them.
func in(p int, msg tea.Msg) tea.Msg {
	switch msg := msg.(type) {
	case eventMsg:
		msg.project = p
		return msg
	case statusMsg:
		msg.project = p
		return msg
	}
	return msg
}

// twoProjects is a daemon's view over foo and bar.
func twoProjects() Deps {
	return Deps{Projects: []Project{{Name: "foo", Repo: "acme/foo"}, {Name: "bar", Repo: "acme/bar"}}}
}

// busy is a foo and a bar with something in every panel: foo runs a
// developer and has a finished session and an escalated issue, bar runs QA
// and has an approved pull request, and each has queue counts of its own.
func busy() []tea.Msg {
	return []tea.Msg{
		in(0, staged(12, "developer", 2)),
		in(0, started("developer-issue-12-r2", config.RoleDeveloper, 12, 31, fixed.Add(-3*time.Minute), "opus", false)),
		in(1, started("qa-1", config.RoleQA, 0, 0, fixed.Add(-time.Minute), "sonnet", false)),
		in(0, ended("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 87, 2.41)),
		in(0, statusMsg{status: state.Status{
			Queues:     map[string]int{"ready": 2},
			NeedsHuman: []state.Escalated{escalated(44, "Parser", "gave up", fixed)},
		}, mail: map[string]int{config.RoleDeveloper: 1}}),
		in(1, statusMsg{status: state.Status{
			Queues:   map[string]int{"ready": 3},
			Approved: []state.ApprovedPR{approvedPR(60, 20, "Retry", fixed)},
			NextPoll: fixed.Add(time.Minute),
		}, mail: map[string]int{config.RoleDeveloper: 2, config.RoleQA: 1}}),
	}
}

func right() tea.Msg { return tea.KeyMsg{Type: tea.KeyRight} }
func left() tea.Msg  { return tea.KeyMsg{Type: tea.KeyLeft} }

// header is the first line of a rendered view.
func header(view string) string { return strings.SplitN(view, "\n", 2)[0] }

// A daemon's view opens on every project and ← and → cycle the selector
// through all, then each project in the daemon's order, wrapping at either
// end. The entry in view is bracketed, so it reads without colour, and the
// header names the repository of the project in view.
func TestADaemonsViewCyclesThroughTheProjects(t *testing.T) {
	for _, tc := range []struct {
		keys []tea.Msg
		want string
	}{
		{nil, "busybees  [all] foo bar"},
		{[]tea.Msg{right()}, "busybees  all [foo] bar  acme/foo"},
		{[]tea.Msg{right(), right()}, "busybees  all foo [bar]  acme/bar"},
		{[]tea.Msg{right(), right(), right()}, "busybees  [all] foo bar"},
		{[]tea.Msg{left()}, "busybees  all foo [bar]  acme/bar"},
		{[]tea.Msg{left(), left()}, "busybees  all [foo] bar  acme/foo"},
	} {
		view := drive(t, twoProjects(), tc.keys...)
		if got := header(view); !strings.HasPrefix(got, tc.want) {
			t.Errorf("after %d keys the header is %q, want it to start %q", len(tc.keys), got, tc.want)
		}
		if !strings.Contains(view, "←→ project · ↑↓ select") {
			t.Errorf("the footer does not offer the project keys:\n%s", view)
		}
	}
}

// With every project in view each panel lists every project's rows together
// and a project column says which is whose; the Queues panel adds the
// projects up. Cycling to one project filters every panel to it — the other
// project's rows gone, the column with them — and the Queues panel is that
// project's own, exactly as `bees status` prints it.
func TestAllShowsEveryProjectsRowsWithAProjectColumn(t *testing.T) {
	all := drive(t, twoProjects(), busy()...)
	for _, want := range []string{
		"project role", "project issue", "project pr",
		"foo     developer        #12   #31   developer r2",
		"bar     QA engineer",
		"foo     developer        #12   #31   pr-opened",
		"foo     #44   just now  Parser",
		"bar     #60   #20   just now   Retry",
		"ready           5", "unread mail   developer 3, QA engineer 1", "next poll     in 1m0s",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("all does not show %q:\n%s", want, all)
		}
	}

	foo := drive(t, twoProjects(), append(busy(), right())...)
	for _, want := range []string{
		"  role             issue pr    stage", "▸ developer        #12   #31   developer r2",
		"  developer        #12   #31   pr-opened", "  #44   just now  Parser",
		"no approved pull requests are waiting to be merged",
		"ready           2", "unread mail   developer 1", "next poll     not scheduled yet",
	} {
		if !strings.Contains(foo, want) {
			t.Errorf("foo does not show %q:\n%s", want, foo)
		}
	}
	for _, gone := range []string{"project role", "project issue", "project pr", "QA engineer", "#60", "Retry", "bar "} {
		if strings.Contains(strings.TrimPrefix(foo, header(foo)), gone) {
			t.Errorf("foo still shows %q, which is bar's or the project column:\n%s", gone, foo)
		}
	}

	bar := drive(t, twoProjects(), append(busy(), left())...)
	for _, want := range []string{
		"▸ QA engineer", "no sessions have finished yet", "nothing is waiting for a person",
		"  #60   #20   just now   Retry",
		"ready           3", "unread mail   developer 2, QA engineer 1", "next poll     in 1m0s",
	} {
		if !strings.Contains(bar, want) {
			t.Errorf("bar does not show %q:\n%s", want, bar)
		}
	}
	if strings.Contains(bar, "#44") || strings.Contains(bar, "developer r2") {
		t.Errorf("bar shows foo's rows:\n%s", bar)
	}
}

// A single-project view is exactly what it was before there were daemons:
// no selector, no project column, no project keys, and ← and → do nothing.
func TestASingleProjectViewHasNoSelector(t *testing.T) {
	msgs := []tea.Msg{
		started("developer-issue-12-r2", config.RoleDeveloper, 12, 31, fixed.Add(-3*time.Minute), "opus", false),
		statusMsg{status: state.Status{NeedsHuman: []state.Escalated{escalated(44, "Parser", "gave up", fixed)}}},
	}
	view := drive(t, Deps{Repo: "acme/widgets"}, msgs...)
	if !strings.HasPrefix(header(view), "busybees  acme/widgets ") {
		t.Errorf("the header is %q", header(view))
	}
	for _, absent := range []string{"[all]", "project", "←→"} { // the column, and the footer's "←→ project"
		if strings.Contains(view, absent) {
			t.Errorf("a single-project view shows %q:\n%s", absent, view)
		}
	}
	if moved := drive(t, Deps{Repo: "acme/widgets"}, append(msgs, right(), left())...); moved != view {
		t.Errorf("← and → changed a single-project view:\n%s", moved)
	}
}

// A session name is unique within a project's state directory and not
// across projects: the same name in foo and bar is two sessions, and what
// one reports — ending, its spend, its stage — is its own project's.
func TestTheSameSessionNameInTwoProjectsIsTwoSessions(t *testing.T) {
	view := drive(t, twoProjects(),
		in(0, staged(12, "developer", 2)),
		in(0, started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed, "opus", false)),
		in(1, started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed, "opus", false)),
		in(1, ended("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 87, 2.41)),
		in(1, staged(12, "reviewer", 1)),
		in(1, started("reviewer-pr-31-r1", config.RoleReviewer, 12, 31, fixed, "opus", false)),
	)
	if !strings.Contains(view, "foo     developer        #12   -     developer r2") {
		t.Errorf("foo's session left the Now panel when bar's ended, or took bar's stage or spend:\n%s", view)
	}
	if strings.Contains(view, "bar     developer        #12   -  ") {
		t.Errorf("bar's ended session is still in the Now panel:\n%s", view)
	}
	if !strings.Contains(view, "bar     reviewer         #12   #31   reviewer r1") {
		t.Errorf("bar's next session on the issue does not carry bar's stage:\n%s", view)
	}
	// The spend is bar's issue 12's, not foo's: foo's row still says it has
	// no known cost.
	if !strings.Contains(row(view, "bar     reviewer"), "$2.41") || strings.Contains(row(view, "foo     developer"), "$2.41") {
		t.Errorf("the spend is not bar's alone:\n%s", view)
	}
}

// row is the line of a rendered view that contains s, or "".
func row(view, s string) string {
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, s) {
			return line
		}
	}
	return ""
}

// k stops the selected session through its own project's Kill, o opens the
// selected row in its own project's repository, and a message typed while
// watching a session goes through its project's Send.
func TestKillOpenAndMessageGoToTheRowsProject(t *testing.T) {
	var killed, opened []string
	var sentTo []string
	d := twoProjects()
	d.Now = func() time.Time { return fixed }
	d.Open = func(url string) error { opened = append(opened, url); return nil }
	for i := range d.Projects {
		name := d.Projects[i].Name
		d.Projects[i].Kill = func(session string) error { killed = append(killed, name+":"+session); return nil }
		d.Projects[i].Send = func(to string, issue, pr int, subject, body string) error {
			sentTo = append(sentTo, name+":"+to+":"+body)
			return nil
		}
	}
	var m tea.Model = New(d)
	m, _ = m.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	for _, msg := range busy() {
		m, _ = m.Update(msg)
	}
	// Row 1 of all is bar's QA session.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("k")})
	runCmd(t, cmd)
	if want := []string{"bar:qa-1"}; strings.Join(killed, ",") != strings.Join(want, ",") {
		t.Errorf("k stopped %v, want %v", killed, want)
	}

	// Row 4 of all is bar's approved pull request.
	for range 3 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	}
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("o")})
	runCmd(t, cmd)
	if want := "https://github.com/acme/bar/issues/60"; strings.Join(opened, ",") != want {
		t.Errorf("o opened %v, want %s", opened, want)
	}

	// Back on bar's session: watch it and message its role.
	for range 3 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("retry")})
	_, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	runCmd(t, cmd)
	if want := "bar:qa:retry"; strings.Join(sentTo, ",") != want {
		t.Errorf("the message went %v, want %s", sentTo, want)
	}
}

// With every project in view, the header's notices name their project: a
// paused foo is told apart from a bar still working, and a status.json that
// could not be read is named as whose. With one project in view its notices
// read as a single-project view's.
func TestHeaderNamesTheProjectOfEachNotice(t *testing.T) {
	msgs := []tea.Msg{
		in(0, statusMsg{status: state.Status{BudgetPaused: true, DaySpendUSD: 323.8, DayBudgetUSD: 300}}),
		in(1, statusMsg{status: state.Status{DaySpendUSD: 0.12, DayBudgetUSD: 5}}),
		in(1, statusMsg{err: errors.New("unexpected end of JSON input")}),
	}
	all := header(drive(t, twoProjects(), msgs...))
	for _, want := range []string{
		"⏸ foo: daily budget ($323.80 / $300.00)",
		"bar: daily budget: $0.12 / $5.00",
		"status: bar: unexpected end of JSON input",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("all's header lacks %q: %q", want, all)
		}
	}
	// Notices in project order, left to right.
	if strings.Index(all, "foo:") > strings.Index(all, "bar:") {
		t.Errorf("the notices are not in project order: %q", all)
	}

	foo := header(drive(t, twoProjects(), append(msgs, right())...))
	if !strings.Contains(foo, "⏸ daily budget ($323.80 / $300.00)") || strings.Contains(foo, "foo:") || strings.Contains(foo, "bar:") {
		t.Errorf("foo's header is %q, want its own notice unprefixed and none of bar's", foo)
	}
}

// The cursor walks the rows in view and only those: cycling to a project
// with fewer rows than the cursor is on keeps it on a drawn row, and the
// row it marks is that project's.
func TestCyclingKeepsTheCursorOnARowInView(t *testing.T) {
	msgs := append(busy(), tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown}, tea.KeyMsg{Type: tea.KeyDown})
	// Row 3 of all is bar's approved pull request; bar alone has two rows.
	view := drive(t, twoProjects(), append(msgs, left())...)
	var marked string
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "▸ ") {
			marked = line
		}
	}
	if !strings.Contains(marked, "#60") {
		t.Errorf("in bar the cursor marks %q, want bar's last row (#60):\n%s", marked, view)
	}
	// In foo the marked row is foo's, never a row of bar's the index would
	// have named in all.
	view = drive(t, twoProjects(), append(busy(), tea.KeyMsg{Type: tea.KeyDown}, right())...)
	marked = ""
	for _, line := range strings.Split(view, "\n") {
		if strings.Contains(line, "▸ ") {
			marked = line
		}
	}
	if !strings.Contains(marked, "developer        #12   #31   pr-opened") {
		t.Errorf("in foo the cursor marks %q, want foo's second row:\n%s", marked, view)
	}
}

// Every project's event stream is read: an event from either project's
// scheduler reaches the model, tagged with its project.
func TestEveryProjectsEventStreamIsRead(t *testing.T) {
	foo, bar := make(chan scheduler.Event, 1), make(chan scheduler.Event, 1)
	d := twoProjects()
	d.Projects[0].Events, d.Projects[1].Events = foo, bar
	m := New(d)
	bar <- scheduler.Event{Kind: scheduler.EventSessionStarted, Session: "qa-1", Role: config.RoleQA}
	foo <- scheduler.Event{Kind: scheduler.EventSessionStarted, Session: "dev-1", Role: config.RoleDeveloper}
	got := map[sessionRef]bool{}
	for p := range d.Projects {
		msg, ok := m.waitForEvent(p)().(eventMsg)
		if !ok {
			t.Fatalf("project %d delivered no event", p)
		}
		got[sessionRef{msg.project, msg.Session}] = true
	}
	if !got[sessionRef{0, "dev-1"}] || !got[sessionRef{1, "qa-1"}] {
		t.Errorf("the events were tagged %v", got)
	}
}

func TestReviewActivityBelongsToItsEventSourceProject(t *testing.T) {
	d := twoProjects()
	d.Now = func() time.Time { return fixed }
	var opened string
	d.Open = func(url string) error { opened = url; return nil }
	m := New(d)
	m.width, m.height = 140, panelHeight
	ev := reviewActivity(scheduler.EventReviewStarted)
	// Identical project-local identities must coexist across projects.
	for p := range 2 {
		next, _ := m.Update(eventMsg{project: p, Event: ev})
		m = next.(Model)
	}
	if len(m.sessions) != 2 {
		t.Fatalf("project activities collided: %+v", m.sessions)
	}
	view := plain(m.View())
	for _, want := range []string{"project role", "foo     reviewer", "bar     reviewer"} {
		if !strings.Contains(view, want) {
			t.Errorf("all-project view lacks %q:\n%s", want, view)
		}
	}
	m.cycle(1) // foo
	if rows := m.shownSessions(); len(rows) != 1 || rows[0].project != 0 {
		t.Fatalf("foo rows: %+v", rows)
	}
	m.cycle(1) // bar
	if rows := m.shownSessions(); len(rows) != 1 || rows[0].project != 1 {
		t.Fatalf("bar rows: %+v", rows)
	}
	_, cmd := m.openOnGitHub()
	cmd()
	if opened != "https://github.com/acme/bar/issues/31" {
		t.Errorf("wrong project's link: %s", opened)
	}
	ev.Kind, ev.Phase, ev.Completed, ev.Total = scheduler.EventReviewProgress, "angles", 1, 3
	m.apply(0, ev)
	if m.sessions[0].activity.Completed != 1 || m.sessions[1].activity.Completed != 0 {
		t.Fatal("progress crossed projects")
	}
	m.apply(1, scheduler.Event{Kind: scheduler.EventSessionStarted, Activity: ev.Activity,
		Session: "bar-judge", Role: config.RoleReviewer, Time: fixed, Work: ghwork.New(0, 31)})
	if len(m.sessions) != 2 || m.sessions[0].activity == nil || m.sessions[1].name != "bar-judge" {
		t.Fatalf("handoff crossed projects: %+v", m.sessions)
	}
	ev.Kind, ev.Err = scheduler.EventReviewEnded, "cancelled"
	m.apply(0, ev)
	if len(m.sessions) != 1 || m.sessions[0].name != "bar-judge" || len(m.recent) != 0 {
		t.Fatal("failure removed another project's judge or created Recent")
	}
}
