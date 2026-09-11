package reviewtui

import (
	"context"
	"errors"
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/review"
	"github.com/kpenfound/busybees/internal/text"
)

// The keys the screen takes, the console's vocabulary
// (internal/review/console.go): the two front ends behave alike, and a
// person who has used one knows the other.
const (
	keySelect  = "s"
	keyEdit    = "e"
	keyDismiss = "d"
	keyDefer   = "f"
	keyAsk     = "a"
	keyNext    = "n"
	keyQuit    = "q"
	keyHelp    = "?"
)

// mode is what the screen's keys do right now.
type mode int

const (
	// modeTriage is the screen at rest: a key is an action on the finding.
	modeTriage mode = iota
	// modeInput is a text being typed: a comment, a reason, a question.
	modeInput
	// modeAsking is Queue.Ask under way. Keys are ignored until the answer
	// comes back, except the one that closes the screen.
	modeAsking
	// modeHelp is the help shown in place of the finding. Any key closes it.
	modeHelp
)

// The texts the screen asks for, each an input with a kind.
const (
	inputEdit    = "edit"
	inputDismiss = "dismiss"
	inputAsk     = "ask"
)

// input is a text being typed: what it is for, what has been typed, and
// whether return ends a line or the text. Typing goes at the end only,
// which is all a reason or a question needs; a comment that wants more
// editing than that is what the console's editor key is for.
type input struct {
	kind   string
	prompt string
	draft  string
	// multiline is set for the comment text: return starts a new line and
	// ctrl-d submits. A single-line input is submitted by return.
	multiline bool
}

// pane is one of the two panes, the one the scroll keys move.
type pane int

const (
	paneDiff pane = iota
	paneFinding
)

// askedMsg is what an ask came to: the answer, or the error Queue.Ask
// returned.
type askedMsg struct {
	id     string
	answer *review.Answer
	err    error
}

// Model is the Bubble Tea model behind the triage screen: the queue, the
// finding on screen with the diff view built for it, and the keys over
// them. Update and View are ordinary functions of the model and its
// messages, no terminal, no goroutine, which is how the screen is tested.
//
// Everything View draws is read from the model, never from the queue: what
// the queue says about the current finding is copied in by show and reload,
// so that an ask, which the queue answers on another goroutine, never has
// the screen reading what it is writing.
type Model struct {
	ctx  context.Context
	q    *review.Queue
	diff string

	width, height int
	mode          mode
	input         input
	focus         pane

	// finding is the finding on screen, pos its place among the pending
	// findings (0-based) and total how many are pending. view is the diff
	// view built for it, rows the diff pane's rows and text the finding
	// pane's, both drawn from the top the scrolls name.
	finding       review.Finding
	pos, total    int
	view          *review.DiffView
	rows          []diffRow
	text          string
	diffScroll    int
	findingScroll int

	// notice is what the last action came to, when it is something the
	// person has to read before the next key: an action the queue refused,
	// an empty comment text, what an ask added. It is cleared by the next
	// key.
	notice string
	// asking is the angle an ask is waiting on, while one is.
	asking string
	// err is the error that stopped triage, which Run returns.
	err error
}

// New is the screen over q, showing the first undecided finding beside
// diff. ctx is what an ask runs under.
func New(ctx context.Context, diff string, q *review.Queue) Model {
	m := Model{ctx: ctx, q: q, diff: diff, width: 80, height: 24}
	shown, _ := m.show(0)
	return shown.(Model)
}

// Init ends the screen at once when nothing is undecided.
func (m Model) Init() tea.Cmd {
	if m.total == 0 {
		return tea.Quit
	}
	return nil
}

// Err is the error that stopped triage, and nil when the screen closed on
// a key or with nothing left to decide.
func (m Model) Err() error { return m.err }

func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil
	case askedMsg:
		return m.answered(msg)
	case tea.KeyMsg:
		return m.key(msg)
	}
	return m, nil
}

// ---- keys ------------------------------------------------------------------

// key takes one key. Ctrl-C closes the screen in every mode; what the rest
// do depends on the mode.
func (m Model) key(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	if msg.String() == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case modeInput:
		return m.inputKey(msg)
	case modeAsking:
		return m, nil
	case modeHelp:
		m.mode = modeTriage
		if msg.String() == keyQuit {
			return m, tea.Quit
		}
		return m, nil
	}
	m.notice = ""
	switch msg.String() {
	case keySelect:
		return m.decide(m.q.Select(m.finding.ID, ""))
	case keyEdit:
		m.open(input{kind: inputEdit, prompt: "comment text", draft: m.finding.Comment(), multiline: true})
	case keyDismiss:
		m.open(input{kind: inputDismiss, prompt: "reason (recorded in your reviewer notes): "})
	case keyDefer:
		return m.decide(m.q.Defer(m.finding.ID))
	case keyAsk:
		m.open(input{kind: inputAsk, prompt: fmt.Sprintf("question for the %s angle: ", m.finding.Angle)})
	case keyNext:
		return m.show(m.pos + 1)
	case keyQuit:
		return m, tea.Quit
	case keyHelp:
		m.mode = modeHelp
	case "tab":
		m.focus = paneFinding - m.focus
	case "up", "k":
		m.scrollBy(-1)
	case "down", "j":
		m.scrollBy(1)
	case "pgup":
		m.scrollBy(-m.pageHeight())
	case "pgdown", " ":
		m.scrollBy(m.pageHeight())
	case "home", "g":
		m.scrollTo(0)
	case "end", "G":
		m.scrollTo(bottom)
	}
	return m, nil
}

// open starts typing a text.
func (m *Model) open(in input) {
	m.mode, m.input = modeInput, in
}

// inputKey takes one key while a text is being typed.
func (m Model) inputKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	in := &m.input
	switch msg.Type {
	case tea.KeyEsc:
		m.mode, m.input = modeTriage, input{}
	case tea.KeyEnter:
		if !in.multiline {
			return m.submit()
		}
		in.draft += "\n"
	case tea.KeyCtrlD:
		if in.multiline {
			return m.submit()
		}
	case tea.KeyBackspace, tea.KeyDelete:
		if r := []rune(in.draft); len(r) > 0 {
			in.draft = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		in.draft += " "
	case tea.KeyRunes:
		in.draft += string(msg.Runes)
	}
	return m, nil
}

// submit takes the text typed for the action it was typed for.
func (m Model) submit() (tea.Model, tea.Cmd) {
	in := m.input
	m.mode, m.input = modeTriage, input{}
	switch in.kind {
	case inputEdit:
		if strings.TrimSpace(in.draft) == "" {
			m.notice = "the comment text is empty, so the finding stays undecided"
			return m, nil
		}
		return m.decide(m.q.Select(m.finding.ID, in.draft))
	case inputDismiss:
		return m.decide(m.q.Dismiss(m.finding.ID, in.draft))
	case inputAsk:
		m.mode, m.asking = modeAsking, m.finding.Angle
		return m, m.ask(in.draft)
	}
	return m, nil
}

// ask runs Queue.Ask off the screen's goroutine: the angle's session takes
// as long as it takes, and the screen stays drawn meanwhile.
func (m Model) ask(question string) tea.Cmd {
	ctx, q, id := m.ctx, m.q, m.finding.ID
	return func() tea.Msg {
		answer, err := q.Ask(ctx, id, question)
		return askedMsg{id: id, answer: answer, err: err}
	}
}

// answered takes what an ask came to: the finding is shown again with the
// question and the answer under it (the queue recorded them), scrolled to
// the answer, and the notice says what the answer added to the queue.
func (m Model) answered(msg askedMsg) (tea.Model, tea.Cmd) {
	m.mode, m.asking = modeTriage, ""
	if msg.err != nil {
		return m.decide(fmt.Errorf("ask failed: %w", msg.err))
	}
	m = m.reload()
	m.notice = "answered"
	if n := len(msg.answer.Added); n > 0 {
		m.notice = fmt.Sprintf("answered; the answer added %s to the queue", text.Count(n, "finding"))
	}
	m.focus = paneFinding
	m.scrollTo(bottom)
	return m, nil
}

// decide takes what an action came to. Nothing wrong shows the next
// finding; a refusal is shown and the finding stays; anything else could
// not be written, and closes the screen with it.
func (m Model) decide(err error) (tea.Model, tea.Cmd) {
	var refusal *review.Refusal
	switch {
	case err == nil:
		return m.show(m.pos)
	case errors.As(err, &refusal):
		m.notice = err.Error()
		return m, nil
	default:
		m.err = err
		return m, tea.Quit
	}
}

// ---- the finding on screen -------------------------------------------------

// show puts the i-th pending finding on screen, counting round from the
// first past the last, with the diff view built for it and the diff
// scrolled to its lines. It closes the screen when nothing is pending.
func (m Model) show(i int) (tea.Model, tea.Cmd) {
	pending := m.q.Pending()
	n := len(pending)
	if n == 0 {
		m.total = 0
		return m, tea.Quit
	}
	i = ((i % n) + n) % n
	m.finding, m.pos, m.total = pending[i], i, n
	m.view = review.NewDiffView(m.diff, &m.finding)
	m.rows, m.diffScroll = diffRows(m.view)
	m.text = m.describe()
	m.findingScroll = 0
	m.focus = paneDiff
	return m, nil
}

// reload re-reads what the queue says about the finding on screen, after an
// ask has added to it, keeping where the person had scrolled to.
func (m Model) reload() Model {
	for i, f := range m.q.Pending() {
		if f.ID == m.finding.ID {
			m.pos = i
		}
	}
	m.total = len(m.q.Pending())
	m.text = m.describe()
	return m
}

// describe is the finding as the console prints it (Queue.Describe), with
// one line the console does not need: whether its lines are in the diff.
// A finding the diff does not have is posted in the review's summary
// rather than on a line, and the person deciding on it should know.
func (m Model) describe() string {
	text := m.q.Describe(m.finding)
	if m.finding.Anchored() && !m.view.InDiff {
		where, rest, _ := strings.Cut(text, "\n")
		text = where + "\n(not in the diff: posted in the review's summary, not on a line)\n" + rest
	}
	return text
}

// ---- scrolling -------------------------------------------------------------

// bottom is a scroll position past any pane's end, which the draw clamps to
// the last page.
const bottom = 1 << 30

// scrollBy moves the focused pane n lines.
func (m *Model) scrollBy(n int) {
	if m.focus == paneDiff {
		m.diffScroll = max(0, m.diffScroll+n)
	} else {
		m.findingScroll = max(0, m.findingScroll+n)
	}
}

// scrollTo puts the focused pane at a line.
func (m *Model) scrollTo(line int) {
	if m.focus == paneDiff {
		m.diffScroll = line
	} else {
		m.findingScroll = line
	}
}
