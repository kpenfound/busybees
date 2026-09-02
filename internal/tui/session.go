package tui

import (
	"fmt"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/prompts"
)

// watch is the session the session view is showing: which session it is,
// the transcript read so far, where in it the reader is, and the message
// being typed, if one is.
//
// It outlives the session it is about. A session that ends while someone is
// reading it leaves the Now panel, but its transcript is on disk and the
// last thing it said is usually the thing worth reading, so the view keeps
// showing it until the reader goes back.
type watch struct {
	name  string
	role  string
	dir   string
	issue int
	pr    int

	// lines are the rendered transcript lines and off is how much of
	// transcript.jsonl they were read from; the next read starts there.
	lines []string
	off   int64
	// err is the last transcript read that failed. The lines already read
	// stay on screen: a view that blanks itself says less than a stale one.
	err string
	// ended is set when the session this is about has finished.
	ended bool

	// scroll is the first display line shown. While follow is set it is
	// recomputed on every draw instead, so the view sits on the tail as the
	// session writes to it.
	scroll int
	follow bool

	// composing is set while a message is being typed, draft is what has
	// been typed, and sent is the confirmation of the last one delivered.
	composing bool
	draft     string
	sent      string
}

// open starts watching the session selected in the Now panel.
func (m Model) open(s running) Model {
	m.watching = &watch{
		name: s.name, role: s.role, dir: s.dir, issue: s.issue, pr: s.pr,
		follow: true,
	}
	m.tailGen++
	return m
}

// ---- messages --------------------------------------------------------------

// transcriptMsg is a read of the watched session's transcript: whatever was
// appended since the last one.
type transcriptMsg struct {
	gen   int
	lines []string
	off   int64
	err   error
}

// tailMsg asks for the next read. It carries the generation of the watch it
// belongs to, so the loop of a session view that has been closed stops
// rather than racing the next one.
type tailMsg int

// sentMsg is the outcome of delivering a typed message.
type sentMsg struct {
	note string
	err  error
}

// tailInterval is how often the watched session's transcript is re-read.
// The file is appended to as claude works, and this is what makes the view
// follow it; it is faster than the once-a-second redraw because a
// transcript is read rather than glanced at.
const tailInterval = 300 * time.Millisecond

func tail(gen int) tea.Cmd {
	return tea.Tick(tailInterval, func(time.Time) tea.Msg { return tailMsg(gen) })
}

// readTail reads the watched session's transcript from where the last read
// stopped.
func (m Model) readTail() tea.Cmd {
	w, gen := m.watching, m.tailGen
	if w == nil {
		return nil
	}
	dir, off := w.dir, w.off
	return func() tea.Msg {
		lines, next, err := readTranscript(dir, off)
		return transcriptMsg{gen: gen, lines: lines, off: next, err: err}
	}
}

// deliver queues a typed message for the role the watched session is
// running as. See Deps.Send for why it is a message to the role rather than
// to the session.
func (m Model) deliver(body string) tea.Cmd {
	w, send := m.watching, m.deps.Send
	if w == nil || send == nil {
		return nil
	}
	to, issue, pr := w.role, w.issue, w.pr
	subject := "A person's message from the live view"
	note := fmt.Sprintf("queued for the next %s session%s", prompts.Title(to), on(issue, pr))
	return func() tea.Msg {
		if err := send(to, issue, pr, subject, body); err != nil {
			return sentMsg{err: err}
		}
		return sentMsg{note: note}
	}
}

// about names what a message is addressed to, for the sentence the view
// shows about it. A singleton runs on no work item at all, and the empty
// string is what lets a sentence leave the phrase out rather than read
// "on no work item".
func about(issue, pr int) string {
	switch {
	case issue > 0:
		return "issue #" + fmt.Sprint(issue)
	case pr > 0:
		return "PR #" + fmt.Sprint(pr)
	default:
		return ""
	}
}

// on renders what a session is about as the trailing phrase of a sentence,
// and renders nothing at all for a session that is about no work item.
func on(issue, pr int) string {
	if s := about(issue, pr); s != "" {
		return " on " + s
	}
	return ""
}

// ---- keys ------------------------------------------------------------------

// sessionKey handles a key while the session view is up. Ctrl-C is handled
// before this — stopping the factory is the same thing in every view — so
// everything here is about the session being read.
func (m Model) sessionKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	w := m.watching
	if w.composing {
		return m.composeKey(msg)
	}
	switch msg.String() {
	case "esc":
		m.watching = nil
		m.tailGen++
		return m, nil
	case "m":
		if m.deps.Send != nil {
			w.composing, w.sent = true, ""
		}
	case "up", "k":
		w.scrollBy(-1, m.transcriptHeight())
	case "down", "j":
		w.scrollBy(1, m.transcriptHeight())
	case "pgup":
		w.scrollBy(-m.transcriptHeight(), m.transcriptHeight())
	case "pgdown", " ":
		w.scrollBy(m.transcriptHeight(), m.transcriptHeight())
	case "home", "g":
		w.follow, w.scroll = false, 0
	case "end", "G":
		w.follow = true
	}
	return m, nil
}

// composeKey handles a key while a message is being typed.
func (m Model) composeKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	w := m.watching
	switch msg.Type {
	case tea.KeyEsc:
		w.composing, w.draft = false, ""
	case tea.KeyEnter:
		body := strings.TrimSpace(w.draft)
		if body == "" {
			w.composing, w.draft = false, ""
			return m, nil
		}
		w.composing, w.draft = false, ""
		return m, m.deliver(body)
	case tea.KeyBackspace, tea.KeyDelete:
		if r := []rune(w.draft); len(r) > 0 {
			w.draft = string(r[:len(r)-1])
		}
	case tea.KeySpace:
		w.draft += " "
	case tea.KeyRunes:
		w.draft += string(msg.Runes)
	}
	return m, nil
}

// scrollBy moves the reader n lines through the transcript. Moving up stops
// following the tail; arriving back at the end resumes it, so a person who
// scrolled up to read something and came back does not have to say so.
func (w *watch) scrollBy(n, height int) {
	last := max(0, len(w.lines)-height)
	if w.follow {
		w.scroll = last
	}
	w.follow = false
	w.scroll = min(max(0, w.scroll+n), last)
	if w.scroll >= last {
		w.follow = true
	}
}

// ---- rendering -------------------------------------------------------------

// chrome is how many of the terminal's rows the session view spends on
// something other than transcript: the header, the panel's border and
// title, the composer line and the footer.
const chrome = 7

// transcriptHeight is how many lines of transcript fit on screen.
func (m Model) transcriptHeight() int {
	return max(3, m.rows()-chrome)
}

// sessionPanel renders the transcript: the lines the reader is on, followed
// by the tail as it is written unless they have scrolled away from it.
func (m Model) sessionPanel(w int) string {
	t := m.watching
	height := m.transcriptHeight()
	if len(t.lines) == 0 {
		body := "waiting for the session's first words…"
		if t.ended {
			body = "this session wrote no transcript"
		}
		return hintStyle.Render(body)
	}
	first := t.scroll
	if t.follow {
		first = max(0, len(t.lines)-height)
	}
	rows := t.lines[first:min(first+height, len(t.lines))]
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, clip(r, w))
	}
	return strings.Join(out, "\n")
}

// sessionTitle names the session being read and where in it the reader is.
func (m Model) sessionTitle() string {
	t := m.watching
	parts := []string{prompts.Title(t.role), t.name}
	if s := about(t.issue, t.pr); s != "" {
		parts = append(parts, s)
	}
	if t.pr > 0 && t.issue > 0 {
		parts = append(parts, "PR #"+fmt.Sprint(t.pr))
	}
	if t.ended {
		parts = append(parts, "ended")
	}
	where := "following"
	if !t.follow {
		where = fmt.Sprintf("line %d/%d", min(t.scroll+m.transcriptHeight(), len(t.lines)), len(t.lines))
	}
	return strings.Join(parts, " · ") + "  —  " + where
}

// sessionFooter says what a message from here does, or what the last one
// did. It never says "sent": the message is a mailbox entry for the *next*
// session on this work item, and a person must not read it as a word in the
// ear of the session they are watching.
//
// Stopping outranks all of it, composer included, and in the panels footer's
// own words (model.go): Model.key takes ctrl+c before sessionKey ever sees
// it, so the factory is stopped while a message is being typed too, and a
// footer that went on offering to queue one would hide that. Nothing is
// lost — sessionView draws the draft on its own line above this.
func (m Model) sessionFooter() string {
	t := m.watching
	switch {
	case m.stopping || m.hardStopped:
		return m.stoppingNotice()
	case t.composing:
		return fmt.Sprintf("message for the next %s session%s (enter queues it, esc cancels)",
			prompts.Title(t.role), on(t.issue, t.pr))
	case t.err != "":
		return "transcript: " + oneLine(t.err)
	case t.sent != "":
		return t.sent
	case m.deps.Send == nil:
		return "esc back · ↑/↓ scroll · end follow · q or ctrl-c stops (sessions finish)"
	default:
		return "esc back · ↑/↓ scroll · end follow · m message · q or ctrl-c stops (sessions finish)"
	}
}

// sessionView draws the whole session view: the transcript in a panel, and
// under it either the message being typed or what the footer has to say.
func (m Model) sessionView(w, inner int) string {
	t := m.watching
	var b strings.Builder
	b.WriteString(m.header(w) + "\n")
	b.WriteString(panel(clip(m.sessionTitle(), inner), m.sessionPanel(inner), inner, titleStyle) + "\n")
	if t.composing {
		b.WriteString(clip("› "+t.draft+"▏", w) + "\n")
	} else {
		b.WriteString("\n")
	}
	style := hintStyle
	if t.err != "" {
		style = warnStyle
	}
	b.WriteString(style.Render(clip(m.sessionFooter(), w)))
	return b.String()
}
