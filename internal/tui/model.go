package tui

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/text"
)

// Deps are the view's inputs. Everything the model reads comes through one
// of them, so Update and View can be exercised without a terminal, without a
// scheduler and without the wall clock.
type Deps struct {
	// Events is the scheduler's event stream (Scheduler.Subscribe). A nil
	// channel means "no live events", which is what a test drives.
	Events <-chan scheduler.Event
	// Status reads status.json. It is the same file `bees status` reads and
	// the only source of the queue counts, so the two can never disagree.
	Status func() (state.Status, error)
	// Mail counts unread messages per role, as `bees status` does.
	Mail func() (map[string]int, error)
	// Now is the clock every elapsed time and every countdown is measured
	// against. Production passes time.Now; a test passes its own, so a
	// rendered view never depends on when it was rendered (#222).
	Now func() time.Time
	// Stop asks the factory to stop polling and drain, which is what Ctrl-C
	// and q do. Nil means the view cannot stop anything.
	Stop func()
	// Kill stops one running session by the name the event stream gave it
	// and hands the issue it was working on to a person
	// (scheduler.KillSession). Nil means the view cannot stop a session.
	Kill func(session string) error
	// Open shows a URL to the person watching, in whatever they read GitHub
	// in. Nil means the view cannot open anything.
	Open func(url string) error
	// Repo is the repository the factory is building, for the header and
	// for the GitHub links every row can be opened at.
	Repo string
}

// running is one session the factory is running right now, as the Now panel
// renders it. It is built from the session-started event and dropped when
// the matching session-ended arrives.
type running struct {
	name     string
	role     string
	issue    int
	pr       int
	started  time.Time
	model    string
	fallback bool
}

// spend is what the sessions of one work item have reported so far. claude
// reports turns and cost in the final event of a session's stream, so these
// are the totals of the sessions that have *finished*: the running one adds
// its own when it ends.
type spend struct {
	turns int
	cost  float64
}

// finished is one session that has ended, as the Recent panel renders it.
// Everything in it arrives on the session-ended event: nothing is looked up
// afterwards.
type finished struct {
	role    string
	issue   int
	pr      int
	at      time.Time
	outcome string
	note    string
	cost    float64
	took    time.Duration
}

// stage is the developer worker's stage for an issue, from the last stage
// event about it.
type stage struct {
	name  string
	round int
}

// Model is the Bubble Tea model behind `bees run`'s view: five panels fed by
// the scheduler's event stream and by status.json, and the keys over them.
//
// Update and View are ordinary functions of the model and its messages —
// no terminal, no goroutines, no clock of their own — which is how the whole
// view is tested (model_test.go).
type Model struct {
	deps Deps

	// sessions are the running sessions in the order they started; spent
	// and stages are keyed by the work item an event was about (see
	// spendKey) and by issue number.
	sessions []running
	spent    map[string]spend
	stages   map[int]stage
	// recent are the sessions that have finished, newest first, capped at
	// recentRows: a view of what just happened, not a log — ledger.jsonl and
	// bees.log keep everything.
	recent []finished

	status state.Status
	mail   map[string]int
	// statusErr is the last error reading status.json or the mailbox. The
	// view keeps drawing what it last read and says so.
	statusErr string

	// width and height are the terminal's, from the last WindowSizeMsg. The
	// height is what the four lists share out between them (see share), so
	// the whole view fits whatever it is drawn in.
	width  int
	height int
	ticks  int
	// stopping is set by the first Ctrl-C or q: the factory has been asked
	// to stop polling and drain, and the view stays up until it has.
	stopping bool
	// cursor is the selected row of the flat list every panel's rows join
	// (see targets), and confirmKill that the next k stops it. notice is
	// the one line the footer shows instead of the key hints: what the last
	// key did, or why it could not.
	notice      string
	cursor      int
	confirmKill bool
}

// New builds the model. Nothing is read and no goroutine is started until
// Init runs.
func New(d Deps) Model {
	if d.Now == nil {
		d.Now = time.Now
	}
	return Model{deps: d, spent: map[string]spend{}, stages: map[int]stage{}}
}

// ---- messages --------------------------------------------------------------

// eventMsg is one scheduler event delivered to the model.
type eventMsg scheduler.Event

// statusMsg is a fresh read of status.json and the mailbox.
type statusMsg struct {
	status state.Status
	mail   map[string]int
	err    error
}

// actedMsg is what a key that did something outside the model reports back:
// the empty string when it worked, and what went wrong when it did not.
type actedMsg struct{ note string }

// tickMsg redraws the view, so elapsed times and the countdown to the next
// poll advance between events.
type tickMsg time.Time

// Stopped tells the view the factory has stopped and drained, which is the
// one thing that ends the program on its own. The wiring in Run sends it,
// and carries nothing: Run returns the factory's error itself.
type Stopped struct{}

// redrawInterval is how often the view redraws itself between events: once a
// second, because the elapsed times it shows are in seconds.
const redrawInterval = time.Second

// refreshEvery is how many redraws pass before status.json and the mailbox
// are re-read without an event asking for it. Events cover everything the
// scheduler does; this catches what another process did (a session sending
// mail, a person running `bees mail send`).
const refreshEvery = 5

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.waitForEvent(), m.refresh(), redraw())
}

// waitForEvent blocks on the event stream and delivers the next event. It is
// re-issued after every event, so exactly one read is outstanding at a time.
func (m Model) waitForEvent() tea.Cmd {
	ch := m.deps.Events
	if ch == nil {
		return nil
	}
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return nil
		}
		return eventMsg(ev)
	}
}

// refresh re-reads status.json and the mailbox — the same numbers `bees
// status` prints, computed by the scheduler and read here, never recomputed.
func (m Model) refresh() tea.Cmd {
	read, counts := m.deps.Status, m.deps.Mail
	if read == nil {
		return nil
	}
	return func() tea.Msg {
		st, err := read()
		if err != nil {
			return statusMsg{err: err}
		}
		msg := statusMsg{status: st}
		if counts != nil {
			if msg.mail, err = counts(); err != nil {
				return statusMsg{status: st, err: err}
			}
		}
		return msg
	}
}

func redraw() tea.Cmd {
	return tea.Tick(redrawInterval, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		return m.key(msg)
	case eventMsg:
		m.apply(scheduler.Event(msg))
		m.clampCursor()
		return m, tea.Batch(m.waitForEvent(), m.refresh())
	case actedMsg:
		m.notice = msg.note
	case statusMsg:
		if msg.err != nil {
			m.statusErr = msg.err.Error()
			return m, nil
		}
		m.statusErr, m.status, m.mail = "", msg.status, msg.mail
		m.clampCursor()
	case tickMsg:
		m.ticks++
		if m.ticks%refreshEvery == 0 {
			return m, tea.Batch(redraw(), m.refresh())
		}
		return m, redraw()
	case Stopped:
		// The factory is done and Run returns its error; the view has
		// nothing left to draw.
		return m, tea.Quit
	}
	return m, nil
}

// key handles the view's keys.
//
// Ctrl-C and q both ask the factory to stop polling and drain, exactly as an
// interrupt does without the view, and the view stays up while it does —
// pressing either again gives up on the drain and leaves the terminal, with
// the sessions still finishing in the background. Neither is a key that can
// be pressed by accident without being told what it did: the footer says
// what they do before, and what they are doing after.
//
// The arrows move one selection through every panel's rows in turn; o opens
// what is selected on GitHub, and k stops the selected session and hands its
// issue to a person. k asks first, the way Ctrl-C does: it is the one key
// here that throws work away. Enter is deliberately unbound: it is the key a
// person expects to open the thing they have selected *inside* the view, and
// #246 is what does that.
func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k != "k" {
		m.confirmKill = false
	}
	switch k {
	case "ctrl+c", "q":
		m.notice = ""
		if m.stopping {
			return m, tea.Quit
		}
		m.stopping = true
		if m.deps.Stop != nil {
			m.deps.Stop()
		}
	case "up", "shift+tab":
		m.notice = ""
		m.cursor = max(0, m.cursor-1)
	case "down", "tab":
		m.notice = ""
		m.cursor = min(max(0, len(m.targets())-1), m.cursor+1)
	case "o":
		return m.open()
	case "k":
		return m.kill()
	}
	return m, nil
}

// open shows the selected row's issue or pull request on GitHub. One URL
// shape serves both: GitHub redirects an issue URL to the pull request of
// the same number, so a row that is about either is one link.
func (m Model) open() (tea.Model, tea.Cmd) {
	t, ok := m.selected()
	switch {
	case !ok:
		m.notice = "nothing to open"
		return m, nil
	case m.deps.Open == nil:
		m.notice = "this view cannot open a browser"
		return m, nil
	}
	n := t.pr
	if n == 0 {
		n = t.issue
	}
	if n == 0 || m.deps.Repo == "" {
		m.notice = "the selected row is about no issue or pull request"
		return m, nil
	}
	url := fmt.Sprintf("https://github.com/%s/issues/%d", m.deps.Repo, n)
	open := m.deps.Open
	m.notice = "opening " + url
	return m, func() tea.Msg {
		if err := open(url); err != nil {
			return actedMsg{note: "could not open " + url + ": " + oneLine(err.Error())}
		}
		return actedMsg{}
	}
}

// kill stops the selected session. The first press asks, because it throws
// away whatever the session had done and hands its issue to a person; the
// second does it, in the background, because stopping a process waits out a
// grace period and the view must keep drawing while it does.
func (m Model) kill() (tea.Model, tea.Cmd) {
	t, ok := m.selected()
	switch {
	case !ok || t.session == "":
		m.notice = "select a running session to stop it"
		m.confirmKill = false
		return m, nil
	case m.deps.Kill == nil:
		m.notice = "this view cannot stop a session"
		m.confirmKill = false
		return m, nil
	case !m.confirmKill:
		m.confirmKill = true
		m.notice = fmt.Sprintf("k again to stop %s and hand %s to a person", t.session, number(t.issue))
		return m, nil
	}
	m.confirmKill = false
	m.notice = "stopping " + t.session
	kill, name := m.deps.Kill, t.session
	return m, func() tea.Msg {
		if err := kill(name); err != nil {
			return actedMsg{note: "could not stop " + name + ": " + oneLine(err.Error())}
		}
		return actedMsg{note: "stopped " + name}
	}
}

// target is one row a person can select. session is the running session the
// row is about, empty for a row that is not one; issue and pr are what it
// links to.
type target struct {
	session string
	issue   int
	pr      int
}

// targets is every selectable row, in the order the panels draw them: the
// running sessions, then what has just finished, then what the factory is
// waiting for a person over, then what is waiting to be merged. One flat
// list is what makes a single cursor and two keys enough.
func (m Model) targets() []target {
	var out []target
	for _, s := range m.sessions {
		out = append(out, target{session: s.name, issue: s.issue, pr: s.pr})
	}
	for _, f := range m.recent {
		out = append(out, target{issue: f.issue, pr: f.pr})
	}
	for _, e := range m.status.NeedsHuman {
		out = append(out, target{issue: e.Issue})
	}
	for _, a := range m.status.Approved {
		out = append(out, target{issue: a.Issue, pr: a.PR})
	}
	return out
}

func (m Model) selected() (target, bool) {
	t := m.targets()
	if m.cursor < 0 || m.cursor >= len(t) {
		return target{}, false
	}
	return t[m.cursor], true
}

// clampCursor keeps the selection inside the list after the factory has
// moved on under it — a session ending, a queue emptying.
func (m *Model) clampCursor() {
	if n := len(m.targets()); m.cursor >= n {
		m.cursor = max(0, n-1)
	}
}

// apply folds one scheduler event into the model.
func (m *Model) apply(ev scheduler.Event) {
	switch ev.Kind {
	case scheduler.EventSessionStarted:
		m.sessions = append(m.sessions, running{
			name: ev.Session, role: ev.Role, issue: ev.Issue, pr: ev.PR,
			started: ev.Time, model: ev.Model, fallback: ev.Fallback,
		})
	case scheduler.EventSessionEnded:
		m.drop(ev.Session)
		key := spendKey(ev.Issue, ev.Role)
		s := m.spent[key]
		s.turns += ev.Turns
		s.cost += ev.CostUSD
		m.spent[key] = s
		m.recent = append([]finished{{
			role: ev.Role, issue: ev.Issue, pr: ev.PR, at: ev.Time,
			outcome: ev.Outcome, note: ev.Note, cost: ev.CostUSD, took: ev.Duration,
		}}, m.recent...)
		if len(m.recent) > recentRows {
			m.recent = m.recent[:recentRows]
		}
	case scheduler.EventStage:
		if ev.Issue > 0 {
			m.stages[ev.Issue] = stage{name: ev.Stage, round: ev.Round}
		}
	}
}

// drop removes a finished session from the running list.
func (m *Model) drop(name string) {
	for i, s := range m.sessions {
		if s.name == name {
			m.sessions = append(m.sessions[:i:i], m.sessions[i+1:]...)
			return
		}
	}
}

// spendKey is what turns and cost are accumulated under: the issue when the
// event is about one, so every session of a work item — developer rounds,
// reviews, check fixes — adds to the same total. A singleton owns no work
// item, so its runs accumulate under its role instead, for the life of the
// process rather than of an issue.
func spendKey(issue int, role string) string {
	if issue > 0 {
		return "issue-" + strconv.Itoa(issue)
	}
	return "role-" + role
}

// ---- rendering -------------------------------------------------------------

// defaultWidth and defaultHeight are what the view draws at until the
// terminal has told it its own, and what every test renders at.
const (
	defaultWidth  = 100
	defaultHeight = 40
)

var (
	titleStyle  = lipgloss.NewStyle().Bold(true)
	headerStyle = lipgloss.NewStyle().Faint(true)
	hintStyle   = lipgloss.NewStyle().Faint(true)
	warnStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	panelStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)

func (m Model) View() string {
	w := m.width
	if w <= 0 {
		w = defaultWidth
	}
	// A panel's border takes two columns and its padding two more, so the
	// text inside one is four columns narrower than the terminal.
	inner := w - 4
	if inner < 20 {
		inner = 20
	}
	h := m.height
	if h <= 0 {
		h = defaultHeight
	}
	// Queues is as tall as its own contents; the four lists divide what is
	// left of the terminal between them, so the whole view fits in it.
	queues := panel("Queues", m.queuesPanel(inner), inner)
	want := []int{len(m.sessions), len(m.recent), len(m.status.NeedsHuman), len(m.status.Approved)}
	// The header, the footer, the Queues panel, and each list panel's two
	// border lines and title — plus, for a list with anything in it, its
	// column header.
	avail := h - 2 - strings.Count(queues, "\n") - 1 - 3*len(want)
	for _, n := range want {
		if n > 0 {
			avail--
		}
	}
	rows := share(want, avail)
	// Where each panel's rows start in the one flat list the cursor moves
	// through (see targets), so every panel marks the right row.
	at := []int{0, want[0], want[0] + want[1], want[0] + want[1] + want[2]}

	var b strings.Builder
	b.WriteString(m.header(w) + "\n")
	b.WriteString(panel("Now", m.nowPanel(inner, rows[0], at[0]), inner) + "\n")
	b.WriteString(panel("Recent", m.recentPanel(inner, rows[1], at[1]), inner) + "\n")
	b.WriteString(panel("Needs human", m.needsHumanPanel(inner, rows[2], at[2]), inner) + "\n")
	b.WriteString(panel("Approved PRs", m.approvedPanel(inner, rows[3], at[3]), inner) + "\n")
	b.WriteString(queues + "\n")
	b.WriteString(hintStyle.Render(m.footer()))
	return b.String()
}

// header names the repository and the clock the view is drawing at.
func (m Model) header(w int) string {
	left := titleStyle.Render("busybees") + "  " + m.deps.Repo
	right := m.deps.Now().Format("15:04:05")
	if m.statusErr != "" {
		right = warnStyle.Render("status: "+oneLine(m.statusErr)) + "   " + right
	}
	gap := w - lipgloss.Width(left) - lipgloss.Width(right)
	if gap < 1 {
		gap = 1
	}
	return left + strings.Repeat(" ", gap) + right
}

// footer says what the keys do — or, when a key has just done something,
// what it did and how it went. A notice outranks the hints: a person who
// has just pressed one of them is owed the answer, and the hints come back
// as soon as they press anything else.
func (m Model) footer() string {
	switch {
	case m.stopping && len(m.sessions) > 0:
		return fmt.Sprintf("stopping: waiting for %s to finish — q or ctrl-c again to leave them running",
			text.Count(len(m.sessions), "session"))
	case m.stopping:
		return "stopping: draining"
	case m.notice != "":
		return m.notice
	default:
		return "↑↓ select · o open on GitHub · k stop the selected session · q or ctrl-c stops polling and drains"
	}
}

// panel draws one titled box around w columns of text. lipgloss counts the
// padding in the width it is given and the border outside it, so the box
// itself ends up w+4 columns wide.
func panel(title, body string, w int) string {
	return panelStyle.Width(w + 2).Render(titleStyle.Render(title) + "\n" + body)
}

// nowPanel renders every running session: who is running it, what it is
// about, the stage its developer worker is in, how long it has been going,
// what the work item has spent so far and the model it runs on.
func (m Model) nowPanel(w, rows, from int) string {
	if len(m.sessions) == 0 {
		return hintStyle.Render("no sessions running")
	}
	out := []string{headerStyle.Render(clip(nowRow("  ", "role", "issue", "pr", "stage", "elapsed", "turns", "cost", "model"), w))}
	out = append(out, listRows(len(m.sessions), rows, func(i int) string {
		s := m.sessions[i]
		spent := m.spent[spendKey(s.issue, s.role)]
		return clip(nowRow(
			mark(m.cursor, from+i),
			prompts.Title(s.role),
			number(s.issue),
			number(s.pr),
			clip(m.stageOf(s), stageWidth),
			dur(m.deps.Now().Sub(s.started)),
			strconv.Itoa(spent.turns),
			fmt.Sprintf("$%.2f", spent.cost),
			modelCell(s, w),
		), w)
	})...)
	return strings.Join(out, "\n")
}

// stageWidth is the width of the stage column, which every cell is cut to:
// the stages the scheduler publishes run to "pre-review checks (reported)"
// and a wider one would push the model column — and with it the (fallback)
// marker — off the end of the row.
const stageWidth = 20

// nowRow lays the Now panel's columns out. The header and every row go
// through it, so they cannot drift apart.
func nowRow(sel, role, issue, pr, stage, elapsed, turns, cost, model string) string {
	return fmt.Sprintf("%s%-16s %-5s %-5s %-*s %8s %6s %8s  %s", sel, role, issue, pr, stageWidth, stage, elapsed, turns, cost, model)
}

// modelCell renders the last column: the model the session runs on, and
// whether it is the role's fallback. The model *name* is what gets shortened
// when the row does not fit, never the marker — a session running on the
// fallback model is the thing a person watching wants to see, and a name
// long enough to crowd it out is the least surprising part of the row.
func modelCell(s running, w int) string {
	name, marker := s.model, ""
	if name == "" {
		name = "-"
	}
	if s.fallback {
		marker = " (fallback)"
	}
	budget := w - lipgloss.Width(nowRow("  ", "", "", "", "", "", "", "", "")) - lipgloss.Width(marker)
	if budget < 1 {
		// Not even room for the marker: give what room there is to it and
		// let the row's own clip decide the rest. A cut "(fallback" still
		// says more than a cut model name.
		return strings.TrimSpace(marker)
	}
	return clip(name, budget) + marker
}

// stageOf names the stage a session is running in: the developer worker's
// stage for the issue, with the round it is on. A singleton role owns no
// issue and so has no stage.
func (m Model) stageOf(s running) string {
	st, ok := m.stages[s.issue]
	if !ok {
		return "-"
	}
	if st.round > 0 {
		return fmt.Sprintf("%s r%d", st.name, st.round)
	}
	return st.name
}

// queueOrder is the order the Queues panel lists the counts in: the workflow
// states in the order an issue passes through them, then the two kinds that
// live outside the state machine, then the pull requests. Every row is
// printed whether or not the scheduler recorded a count for it, so an idle
// factory reads as zeros rather than as an empty box.
var queueOrder = []string{
	"triage", "ready", "in-progress", "review", "approved", "blocked", "needs-human",
	"features", "feedback", "open_prs",
}

// queueCell is the width of one "name count" cell of the Queues grid: the
// widest queue name plus the count column, which is what %-13s %3d renders.
const queueCell = 17

// queueTitles are the queue names that read badly as their status.json key.
var queueTitles = map[string]string{"open_prs": "open PRs", "no_state": "no state"}

// queuesPanel renders the counts `bees status` prints, read from
// status.json rather than computed a second time, plus the unread mail per
// role and the time to the next GitHub poll.
func (m Model) queuesPanel(w int) string {
	var cells []string
	for _, k := range queueNames(m.status.Queues) {
		cells = append(cells, fmt.Sprintf("%-13s %3d", queueTitle(k), m.status.Queues[k]))
	}
	// As many cells per line as the panel is wide enough for: each is
	// queueCell columns and they are separated by two more.
	per := max(1, (w+2)/(queueCell+2))
	var rows []string
	for i := 0; i < len(cells); i += per {
		rows = append(rows, clip(strings.Join(cells[i:min(i+per, len(cells))], "  "), w))
	}
	rows = append(rows, clip(fmt.Sprintf("%-13s %s", "unread mail", m.mailText()), w))
	rows = append(rows, clip(fmt.Sprintf("%-13s %s", "next poll", m.nextPollText()), w))
	return strings.Join(rows, "\n")
}

// queueNames lists the queues to print: the known ones in their own order,
// then anything else status.json carries, so a queue this version does not
// know about is still shown.
func queueNames(queues map[string]int) []string {
	names := slices.Clone(queueOrder)
	var extra []string
	for k := range queues {
		if !slices.Contains(queueOrder, k) {
			extra = append(extra, k)
		}
	}
	slices.Sort(extra)
	return append(names, extra...)
}

func queueTitle(k string) string {
	if t, ok := queueTitles[k]; ok {
		return t
	}
	return k
}

// mailText lists the roles with unread mail, or says there is none.
func (m Model) mailText() string {
	var parts []string
	for _, r := range config.Roles {
		if n := m.mail[r]; n > 0 {
			parts = append(parts, fmt.Sprintf("%s %d", prompts.Title(r), n))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// nextPollText counts down to the next GitHub poll, from the time the
// scheduler recorded for it.
func (m Model) nextPollText() string {
	switch d := m.status.NextPoll.Sub(m.deps.Now()); {
	case m.status.NextPoll.IsZero():
		return "not scheduled yet"
	case d > 0:
		return "in " + dur(d)
	default:
		return "due"
	}
}

// dur renders an elapsed time or a countdown to the second.
func dur(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// number renders an issue or pull request number, or a dash when the session
// is about neither.
func number(n int) string {
	if n <= 0 {
		return "-"
	}
	return "#" + strconv.Itoa(n)
}

// clip cuts a line to the width of the panel it is drawn in, so a long model
// name or a wide terminal-less default never wraps the box.
func clip(s string, w int) string {
	if w <= 0 || lipgloss.Width(s) <= w {
		return s
	}
	r := []rune(s)
	if len(r) <= w {
		return s
	}
	return string(r[:w-1]) + "…"
}

// oneLine flattens an error for the header.
func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }
