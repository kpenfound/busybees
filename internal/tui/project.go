package tui

import (
	"fmt"
	"slices"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/state"
)

// Project is the inputs that are one project's own: its event stream, its
// status.json and mailbox, the two things the view asks its scheduler to do,
// and its repository. A single-project view has them on Deps itself; a
// daemon's view lists one Project per project it runs in Deps.Projects, and
// the view gains a selector over them (see Model.shown).
type Project struct {
	// Path is the resolved bees.toml identity. Generation distinguishes a new
	// loop after removal from the old loop still draining at the same path.
	Path       string
	Generation uint64
	Draining   bool
	// Name is what the selector and the project column call the project.
	// Empty on the one project of a single-project view, which draws
	// neither.
	Name string
	// Repo is the repository, for the header and the GitHub links every
	// row can be opened at.
	Repo string
	// Events is the project's scheduler's event stream
	// (Scheduler.Subscribe). A nil channel means "no live events", which
	// is what a test drives.
	Events <-chan scheduler.Event
	// Done cancels outstanding event reads when the source is retired.
	Done <-chan struct{}
	// Status reads the project's status.json; Mail counts its unread
	// messages per role. See Deps.Status and Deps.Mail.
	Status func() (state.Status, error)
	Mail   func() (map[string]int, error)
	// Kill and Send are Deps.Kill and Deps.Send for this project: the
	// session the name is of, and the mailbox the message goes to, are
	// this project's.
	Kill func(session string) error
	Send func(to string, issue, pr int, subject, body string) error
}

// projects is what the view shows: Deps.Projects when it lists any, else the
// one project the fields of Deps itself describe.
func (d Deps) projects() []Project {
	if len(d.Projects) > 0 {
		return d.Projects
	}
	return []Project{{
		Repo: d.Repo, Events: d.Events, Status: d.Status, Mail: d.Mail,
		Kill: d.Kill, Send: d.Send,
	}}
}

// project is one project as the model tracks it: its inputs, the last read
// of its status.json and mailbox, and what its work items have spent and
// which stage each is in. Sessions and review activities are kept on the
// model, tagged with their project: Now in start order, Recent newest first.
type project struct {
	Project
	status state.Status
	mail   map[string]int
	// statusErr is the last error reading the project's status.json or
	// mailbox. The view keeps drawing what it last read and says so.
	statusErr string
	// spent and stages are keyed by the work item an event was about (see
	// spendKey) and by issue number.
	spent  map[string]spend
	stages map[int]stage
}

// allProjects is the selector entry that shows every project's rows
// together, with the project column naming which is whose.
const allProjects = -1

// sessionRef names one running session: the project it belongs to and its
// name, which is unique within a project's state directory and not across
// projects.
type sessionRef struct {
	project int
	name    string
}

// multi says whether the view is over several projects: a daemon's view,
// where the selector and the project column exist. A single-project view
// has neither and reads exactly as it did before there were daemons.
func (m Model) multi() bool { return len(m.projects) > 1 }

// all says whether every project is in view.
func (m Model) all() bool { return m.shown == allProjects }

// inView says whether project p's rows are drawn.
func (m Model) inView(p int) bool { return m.all() || m.shown == p }

// cycle moves the selector by d entries through [all, the projects...],
// wrapping at either end. A single-project view has nothing to cycle
// through and stays where it is.
func (m *Model) cycle(d int) {
	if !m.multi() {
		return
	}
	n := len(m.projects) + 1
	at := slices.Index(m.order, m.shown) + 1
	at = ((at+d)%n + n) % n
	m.shown = allProjects
	if at > 0 {
		m.shown = m.order[at-1]
	}
}

// selector renders the entries a person cycles through, the one in view
// wrapped in brackets and painted as a title, so it can be read without
// the colour: "all [foo] bar".
func (m Model) selector() string {
	names := make([]string, 0, len(m.projects)+1)
	for _, i := range append([]int{allProjects}, m.order...) {
		name := "all"
		if i >= 0 {
			name = m.projects[i].Name
			if m.projects[i].Draining {
				name += " (draining)"
			}
		}
		if i == m.shown {
			name = titleStyle.Render("[" + name + "]")
		}
		names = append(names, name)
	}
	return strings.Join(names, " ")
}

// title is what the header names beside "busybees": the repository of a
// single-project view; in a daemon's view the selector, followed by the
// repository of the project in view, and by nothing when every project is.
func (m Model) title() string {
	if w := m.watching; w != nil && m.projects[w.project] == nil {
		return w.repo + " (removed)"
	}
	if len(m.order) == 0 {
		return "all"
	}
	if !m.multi() {
		p := m.projects[m.order[0]]
		if p.Draining {
			return p.Repo + " (draining)"
		}
		return p.Repo
	}
	if m.all() {
		return m.selector()
	}
	return m.selector() + "  " + m.projects[m.shown].Repo
}

// notices is what the header says, right to left, about the projects in
// view: for each, the pause notice while its dispatch is paused, or its
// daily budget reading while it is not, and a status error. With every
// project in view each notice names its project, so a paused project is
// told apart from the ones still working.
func (m Model) notices() []string {
	var out []string
	for _, i := range m.order {
		p := m.projects[i]
		if !m.inView(i) {
			continue
		}
		prefix := ""
		if m.all() {
			prefix = p.Name + ": "
		}
		switch notice := p.status.PauseNotice(m.deps.Now()); {
		case notice != "":
			out = append(out, warnStyle.Render("⏸ "+prefix+notice))
		case p.status.BudgetNotice() != "":
			out = append(out, headerStyle.Render(prefix+p.status.BudgetNotice()))
		}
		if p.statusErr != "" {
			out = append(out, warnStyle.Render("status: "+prefix+oneLine(p.statusErr)))
		}
	}
	// The last reload of the configuration is the factory's, not a
	// project's: one notice, whatever is in view. A refused one is a
	// standing condition — the file on disk is not what is running.
	switch r := m.reload; {
	case r.at.IsZero():
	case r.err != "":
		out = append(out, warnStyle.Render("config reload refused "+r.at.Format("15:04:05")))
	default:
		out = append(out, headerStyle.Render("config reloaded "+r.at.Format("15:04:05")))
	}
	return out
}

// projectColumn is the width of the project column: wide enough for the
// widest name and for its own header, and zero — the column left out
// altogether — unless every project is in view, which is the one case a
// row cannot say which project it is about any other way.
func (m Model) projectColumn() int {
	if !m.multi() || !m.all() {
		return 0
	}
	w := lipgloss.Width(projectHeader)
	for _, p := range m.projects {
		w = max(w, lipgloss.Width(p.Name))
	}
	return w
}

// projectHeader is the project column's title.
const projectHeader = "project"

// lead is the columns every row starts with: the selection mark and, when
// the project column is drawn, the row's project. Every panel's rows and
// headers go through it, so the column is laid out once.
func (m Model) lead(row, p int) string {
	return mark(m.cursor, row) + m.projectCell(m.projects[p].Name)
}

// leadHeader is lead for a panel's column header.
func (m Model) leadHeader() string {
	return "  " + m.projectCell(projectHeader)
}

func (m Model) projectCell(name string) string {
	w := m.projectColumn()
	if w == 0 {
		return ""
	}
	return fmt.Sprintf("%-*s ", w, name)
}

// shownSessions is the Now rows of the projects in view, in the
// order they started.
func (m Model) shownSessions() []running {
	var out []running
	for _, s := range m.sessions {
		if m.inView(s.project) {
			out = append(out, s)
		}
	}
	return out
}

// shownRecent is the finished sessions of the projects in view, newest
// first.
func (m Model) shownRecent() []finished {
	var out []finished
	for _, f := range m.recent {
		if m.inView(f.project) {
			out = append(out, f)
		}
	}
	return out
}

// escalatedIn is one Needs human entry with the project it belongs to.
type escalatedIn struct {
	project int
	state.Escalated
}

// shownNeedsHuman is the Needs human entries of the projects in view, each
// project's in the order its status.json lists them, project by project.
func (m Model) shownNeedsHuman() []escalatedIn {
	var out []escalatedIn
	for _, i := range m.order {
		p := m.projects[i]
		if !m.inView(i) {
			continue
		}
		for _, e := range p.status.NeedsHuman {
			out = append(out, escalatedIn{i, e})
		}
	}
	return out
}

// approvedIn is one Approved PRs entry with the project it belongs to.
type approvedIn struct {
	project int
	state.ApprovedPR
}

// shownApproved is the approved pull requests of the projects in view,
// each project's in the order its status.json lists them, project by
// project.
func (m Model) shownApproved() []approvedIn {
	var out []approvedIn
	for _, i := range m.order {
		p := m.projects[i]
		if !m.inView(i) {
			continue
		}
		for _, a := range p.status.Approved {
			out = append(out, approvedIn{i, a})
		}
	}
	return out
}

// summary is what the Queues panel is drawn from: the queue counts and the
// unread mail per role of the projects in view, and when the next of them
// polls GitHub.
type summary struct {
	queues   map[string]int
	mail     map[string]int
	nextPoll time.Time
}

// summarise adds up the projects in view. One project's numbers are its
// own, as `bees status` prints them; with every project in view the counts
// are summed queue by queue and role by role, and the next poll is the
// earliest any project has scheduled — the whole machine's queues, which is
// what "all" is for, and cycling to a project reads that project's own.
func (m Model) summarise() summary {
	s := summary{queues: map[string]int{}, mail: map[string]int{}}
	for _, i := range m.order {
		p := m.projects[i]
		if !m.inView(i) {
			continue
		}
		for k, n := range p.status.Queues {
			s.queues[k] += n
		}
		for k, n := range p.mail {
			s.mail[k] += n
		}
		if next := p.status.NextPoll; !next.IsZero() && (s.nextPoll.IsZero() || next.Before(s.nextPoll)) {
			s.nextPoll = next
		}
	}
	return s
}

// projectsMsg is a full snapshot, so coalescing cannot lose a retirement.
type projectsMsg []Project

func (m Model) waitForProjects() tea.Cmd {
	if m.deps.ProjectUpdates == nil {
		return nil
	}
	return func() tea.Msg {
		projects, ok := <-m.deps.ProjectUpdates
		if !ok {
			return nil
		}
		return projectsMsg(projects)
	}
}

func (m *Model) addProject(p Project) int {
	id := m.nextProject
	m.nextProject++
	m.projects[id] = &project{Project: p, spent: map[string]spend{}, stages: map[int]stage{}}
	m.order = append(m.order, id)
	return id
}

// updateProjects retains sources and state by path and incarnation. Handles
// never move or get reused, including after a project leaves the selector.
func (m Model) updateProjects(next []Project) (tea.Model, tea.Cmd) {
	old := m.order
	m.order = nil
	keep := map[int]bool{}
	cmds := []tea.Cmd{m.waitForProjects()}
	for _, p := range next {
		id := -1
		for _, candidate := range old {
			prev := m.projects[candidate]
			if prev.Path == p.Path && prev.Generation == p.Generation {
				id = candidate
				break
			}
		}
		if id < 0 {
			id = m.addProject(p)
			cmds = append(cmds, m.waitForEvent(id), m.refresh(id))
		} else {
			// The source callbacks belong to the incarnation, not the snapshot.
			m.projects[id].Name, m.projects[id].Draining = p.Name, p.Draining
			m.order = append(m.order, id)
		}
		keep[id] = true
	}
	for _, id := range old {
		if !keep[id] {
			delete(m.projects, id)
		}
	}
	m.sessions = slices.DeleteFunc(m.sessions, func(s running) bool { return !keep[s.project] })
	m.recent = slices.DeleteFunc(m.recent, func(s finished) bool { return !keep[s.project] })
	if !keep[m.shown] {
		m.shown = allProjects
	}
	if len(m.order) == 1 {
		m.shown = m.order[0]
	}
	if w := m.watching; w != nil {
		p := m.projects[w.project]
		if p == nil || p.Draining {
			w.composing = false
		}
		if p == nil {
			w.ended = true
		}
	}
	m.confirmKill = sessionRef{}
	m.notice = ""
	m.clampCursor()
	return m, tea.Batch(cmds...)
}
