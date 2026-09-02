package tui

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/session"
)

// sent is one message the view asked to have delivered.
type sent struct {
	to            string
	issue, pr     int
	subject, body string
}

// watcher builds a model already watching the session written in dir, the
// way a person gets there: the session starts, they move the cursor onto it
// and press Enter. It returns the model, the messages it asked to send and
// the commands Enter produced, so a test can drive the transcript loop by
// hand rather than waiting on a Bubble Tea program.
func watcher(t *testing.T, dir string, extra ...tea.Msg) (tea.Model, *[]sent, tea.Cmd) {
	t.Helper()
	box := &[]sent{}
	d := Deps{
		Repo: "acme/widgets",
		Now:  func() time.Time { return fixed },
		Send: func(to string, issue, pr int, subject, body string) error {
			*box = append(*box, sent{to, issue, pr, subject, body})
			return nil
		},
	}
	var m tea.Model = New(d)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m, _ = m.Update(watched("developer-issue-12-r1", config.RoleDeveloper, 12, 31, dir))
	for _, msg := range extra {
		m, _ = m.Update(msg)
	}
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	return m, box, cmd
}

// watched is a session-started event carrying the session's own directory,
// which is what the view follows the transcript in.
func watched(name, role string, issue, pr int, dir string) tea.Msg {
	ev := scheduler.Event{
		Kind: scheduler.EventSessionStarted, Time: fixed.Add(-time.Minute), Session: name,
		Role: role, Dir: dir, Issue: issue, PR: pr, Model: "opus",
	}
	return eventMsg(ev)
}

// run drives one command to its message and folds it back into the model,
// the way a Bubble Tea program does. A batch is run in order.
func run(t *testing.T, m tea.Model, cmd tea.Cmd) (tea.Model, tea.Cmd) {
	t.Helper()
	if cmd == nil {
		return m, nil
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		var next tea.Cmd
		for _, c := range batch {
			m, next = run(t, m, c)
		}
		return m, next
	}
	return m.Update(msg)
}

// writeTranscript writes a session's transcript.jsonl, one stream-json line
// per assistant text given.
func writeTranscript(t *testing.T, dir string, says ...string) {
	t.Helper()
	var b strings.Builder
	for _, s := range says {
		fmt.Fprintf(&b, `{"type":"assistant","message":{"content":[{"type":"text","text":%q}]}}`+"\n", s)
	}
	if err := os.WriteFile(filepath.Join(dir, session.TranscriptFile), []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

// Selecting a running session opens its transcript, and the view keeps
// reading the file as the session writes to it: what a session says while
// someone is watching appears without them asking for it again.
func TestSelectingASessionStreamsItsTranscript(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue")
	m, _, cmd := watcher(t, dir)
	m, cmd = run(t, m, cmd)

	view := plain(m.View())
	for _, want := range []string{"developer-issue-12-r1", "issue #12", "PR #31", "● reading the issue"} {
		if !strings.Contains(view, want) {
			t.Errorf("the session view does not show %q:\n%s", want, view)
		}
	}
	if strings.Contains(view, "Queues") {
		t.Errorf("the session view is still drawing the panels:\n%s", view)
	}

	// The session carries on working; the next read of the file picks up
	// only what it added.
	writeTranscript(t, dir, "reading the issue", "writing the test")
	m, _ = run(t, m, cmd)
	view = plain(m.View())
	if !strings.Contains(view, "● writing the test") {
		t.Errorf("the view did not follow the transcript as it was written:\n%s", view)
	}
	if strings.Count(view, "reading the issue") != 1 {
		t.Errorf("the view rendered a line it had already read:\n%s", view)
	}
}

// The view follows the tail by default: a session writing more than fits on
// screen shows its latest words, not its first. Scrolling up stops it
// following, and scrolling back to the end resumes it — so a person who
// looked back at something does not have to say when they are done.
func TestTheSessionViewFollowsTheTailAndScrolls(t *testing.T) {
	dir := t.TempDir()
	var says []string
	for i := 1; i <= 40; i++ {
		says = append(says, fmt.Sprintf("line %d", i))
	}
	writeTranscript(t, dir, says...)
	m, _, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)

	view := plain(m.View())
	if !strings.Contains(view, "line 40") || strings.Contains(view, "line 1 ") {
		t.Errorf("the view is not on the tail of the transcript:\n%s", view)
	}
	if !strings.Contains(view, "following") {
		t.Errorf("the view does not say it is following:\n%s", view)
	}

	// One line up is enough to stop it following: a reader who moved is
	// reading, and the tail arriving under them would take it away again.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	view = plain(m.View())
	if strings.Contains(view, "line 40") || strings.Contains(view, "following") {
		t.Errorf("scrolling up did not stop the view following the tail:\n%s", view)
	}
	// And one line back down is enough to resume it, because that is the
	// end: nobody should have to say they are done reading.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	view = plain(m.View())
	if !strings.Contains(view, "line 40") || !strings.Contains(view, "following") {
		t.Errorf("scrolling back to the end did not resume following:\n%s", view)
	}

	// Scrolling to the top stops it following and says where the reader is.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	view = plain(m.View())
	if !strings.Contains(view, "● line 1") || strings.Contains(view, "line 40") {
		t.Errorf("home did not scroll to the start of the transcript:\n%s", view)
	}
	if strings.Contains(view, "following") || !strings.Contains(view, "/40") {
		t.Errorf("the view does not say where in the transcript the reader is:\n%s", view)
	}

	// A line that arrives while the reader is scrolled away does not drag
	// them back to the tail.
	writeTranscript(t, dir, append(says, "line 41")...)
	m, _ = run(t, m, m.(Model).readTail())
	if view = plain(m.View()); !strings.Contains(view, "● line 1") {
		t.Errorf("a new line dragged the reader back to the tail:\n%s", view)
	}

	// Scrolling back to the end resumes following.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEnd})
	if view = plain(m.View()); !strings.Contains(view, "line 41") || !strings.Contains(view, "following") {
		t.Errorf("end did not resume following the tail:\n%s", view)
	}
}

// A message typed in the session view is a mailbox entry for the role that
// session is running as, carrying the work item it is about, so it reaches
// whichever session picks that item up next.
//
// The view says exactly that and never "sent": nothing reaches the running
// session — `claude -p` cannot be told anything after it has started — and a
// person who read it as a word in that session's ear would be wrong about
// what the factory is doing.
func TestAMessageIsQueuedForTheNextSessionAndTheViewSaysSo(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue")
	m, box, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	if view := plain(m.View()); !strings.Contains(view, "message for the next developer session on issue #12") {
		t.Errorf("the composer does not say where the message goes:\n%s", view)
	}
	for _, r := range "use the fixture" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeySpace})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("x")})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyBackspace})
	if view := plain(m.View()); !strings.Contains(view, "› use the fixture ") {
		t.Errorf("the composer does not show what is being typed:\n%s", view)
	}

	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if len(*box) != 0 {
		t.Fatalf("the message was delivered from Update rather than from a command: %+v", *box)
	}
	m, _ = run(t, m, cmd)

	if len(*box) != 1 {
		t.Fatalf("the view delivered %d messages, want 1: %+v", len(*box), *box)
	}
	got := (*box)[0]
	if got.to != config.RoleDeveloper || got.issue != 12 || got.pr != 31 {
		t.Errorf("the message went to %q about issue %d / PR %d, want the watched session's developer / 12 / 31",
			got.to, got.issue, got.pr)
	}
	if got.body != "use the fixture" || got.subject == "" {
		t.Errorf("the message was delivered as subject %q body %q", got.subject, got.body)
	}

	view := plain(m.View())
	if !strings.Contains(view, "queued for the next developer session on issue #12") {
		t.Errorf("the view does not say the message was queued rather than delivered:\n%s", view)
	}
	if strings.Contains(view, "sent") || strings.Contains(view, "delivered") {
		t.Errorf("the view claims the running session was told something:\n%s", view)
	}

	// Enter with nothing typed is a person changing their mind, not an
	// empty message for the next session to puzzle over.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	if len(*box) != 1 {
		t.Errorf("an empty message was queued: %+v", *box)
	}
	if view := plain(m.View()); strings.Contains(view, "message for the next") {
		t.Errorf("enter with nothing typed left the composer open:\n%s", view)
	}
}

// A message that could not be queued is said so in the view rather than
// swallowed: a person who typed one and saw nothing would assume it landed.
func TestAMessageThatCannotBeQueuedIsReported(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue")
	m, _, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)
	m, _ = m.Update(sentMsg{err: errors.New("mail: recipient role is required")})
	if view := plain(m.View()); !strings.Contains(view, "could not be queued: mail: recipient role is required") {
		t.Errorf("a failed send is not reported:\n%s", view)
	}
}

// With no way to send, the view does not offer to: `m` types nothing and
// the footer does not mention a key that would do nothing.
func TestWithNoSenderTheViewDoesNotOfferToMessage(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue")
	var m tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }})
	m, _ = m.Update(watched("developer-issue-12-r1", config.RoleDeveloper, 12, 31, dir))
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	view := plain(m.View())
	if strings.Contains(view, "m message") || strings.Contains(view, "message for the next") {
		t.Errorf("the view offers to message with nothing to send it:\n%s", view)
	}
}

// A session that ends while it is being read stays on screen: its
// transcript is on disk and its last words are usually the ones worth
// reading. Escape goes back to the panels, which it has left.
func TestASessionThatEndsWhileItIsReadStaysOnScreen(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "opening the pull request")
	m, _, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)
	m, _ = m.Update(ended("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 87, 2.41))

	view := plain(m.View())
	for _, want := range []string{"● opening the pull request", "ended"} {
		if !strings.Contains(view, want) {
			t.Errorf("the finished session left the session view (%q missing):\n%s", want, view)
		}
	}
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if view = plain(m.View()); !strings.Contains(view, "Queues") || !strings.Contains(view, "no sessions running") {
		t.Errorf("escape did not go back to the panels:\n%s", view)
	}
}

// Closing a session view stops its transcript loop rather than leaving it
// running against the next one: a read that belongs to a closed view is
// dropped, and its tick does not ask for another.
func TestClosingASessionViewStopsItsTranscriptLoop(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue")
	m, _, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)
	gen := m.(Model).tailGen

	// While the view is open the loop re-arms itself: every tick reads and
	// asks for the next tick, which is what makes the view follow a file
	// nothing else announces a change to.
	_, live := m.Update(tailMsg(gen))
	if !hasTail(t, live, gen) {
		t.Error("a tick of the open session view's loop did not ask for the next read")
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if _, cmd := m.Update(tailMsg(gen)); cmd != nil {
		t.Error("the closed session view's loop asked for another read")
	}
	// A read still in flight when the view closed is dropped rather than
	// filling the *next* session view with the last one's transcript. The
	// nil check above cannot catch this one: by the time the stale read
	// arrives a new view is open, so only the generation tells them apart.
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	m, _ = m.Update(transcriptMsg{gen: gen, lines: []string{"● stale"}, off: 99})
	if view := plain(m.View()); strings.Contains(view, "stale") {
		t.Errorf("a read from the closed session view landed in the next one:\n%s", view)
	}
	if got := m.(Model).watching.off; got == 99 {
		t.Error("the closed session view's read offset was carried into the next one")
	}
}

// A transcript that cannot be read never blanks the view: what has already
// been read stays on screen and the reason is said under it.
func TestAFailedTranscriptReadKeepsWhatWasRead(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue")
	m, _, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)
	m, _ = m.Update(transcriptMsg{gen: m.(Model).tailGen, err: errors.New("permission denied")})
	view := plain(m.View())
	if !strings.Contains(view, "● reading the issue") {
		t.Errorf("the failed read blanked the transcript:\n%s", view)
	}
	if !strings.Contains(view, "transcript: permission denied") {
		t.Errorf("the failed read is not reported:\n%s", view)
	}
}

// The cursor is what Enter opens, and it moves with the arrow keys. A
// session that finishes never leaves the cursor pointing past the end of
// the list.
func TestTheCursorSelectsWhichSessionIsOpened(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeTranscript(t, first, "the first session")
	writeTranscript(t, second, "the second session")
	var m tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }})
	m, _ = m.Update(watched("developer-issue-12-r1", config.RoleDeveloper, 12, 31, first))
	m, _ = m.Update(watched("reviewer-pr-33-r1", config.RoleReviewer, 14, 33, second))

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	if view := plain(m.View()); !strings.Contains(view, "the second session") {
		t.Errorf("enter opened a session other than the one the cursor was on:\n%s", view)
	}

	// The session the cursor was on ends. There is one cursor over the whole
	// view (Model.targets), so it stays on the row it was on — which is now
	// that session's row in Recent, a row enter has nothing to open. It must
	// say so rather than open a session the cursor is not on, and ↑ must get
	// back to one that is.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = m.Update(ended("reviewer-pr-33-r1", config.RoleReviewer, 14, 33, 12, 0.4))
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	view := plain(m.View())
	if strings.Contains(view, "the first session") || strings.Contains(view, "the second session") {
		t.Errorf("enter opened a session the cursor was not on:\n%s", view)
	}
	if !strings.Contains(view, "select a running session to watch it") {
		t.Errorf("enter on a row that is not a session does not say so:\n%s", view)
	}

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyUp})
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	if view := plain(m.View()); !strings.Contains(view, "the first session") {
		t.Errorf("the cursor was left pointing past the end of the list:\n%s", view)
	}
}

// With nothing running there is nothing to open, and Enter does not build a
// session view of a session that does not exist. Moving the cursor over an
// empty list and opening the session that starts afterwards is the same
// question one step later: the cursor is kept inside the list where it is
// read, so neither can index past either end of it.
func TestEnterOnAnEmptyNowPanelOpensNothing(t *testing.T) {
	var m tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }})
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd != nil {
		t.Error("enter with no sessions running started a transcript loop")
	}
	if view := plain(m.View()); !strings.Contains(view, "no sessions running") {
		t.Errorf("enter with no sessions running left the panels:\n%s", view)
	}

	dir := t.TempDir()
	writeTranscript(t, dir, "the session that started")
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, _ = m.Update(watched("developer-issue-12-r1", config.RoleDeveloper, 12, 31, dir))
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	if view := plain(m.View()); !strings.Contains(view, "the session that started") {
		t.Errorf("the cursor did not come back inside the list:\n%s", view)
	}
}

// hasTail reports whether running cmd asks for another read of the
// transcript, at the generation given.
func hasTail(t *testing.T, cmd tea.Cmd, gen int) bool {
	t.Helper()
	if cmd == nil {
		return false
	}
	switch msg := cmd().(type) {
	case tailMsg:
		return int(msg) == gen
	case tea.BatchMsg:
		for _, c := range msg {
			if hasTail(t, c, gen) {
				return true
			}
		}
	}
	return false
}

// Only the session being read is marked ended. Sessions finish while
// someone is watching another one — a factory normally has two or three
// running — and the title saying "ended" over a session that is still
// working is a claim the view has no business making.
func TestOnlyTheWatchedSessionIsMarkedEnded(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeTranscript(t, first, "the session being read")
	writeTranscript(t, second, "another session")
	m, _, cmd := watcher(t, first, watched("reviewer-pr-33-r1", config.RoleReviewer, 14, 33, second))
	m, _ = run(t, m, cmd)

	m, _ = m.Update(ended("reviewer-pr-33-r1", config.RoleReviewer, 14, 33, 12, 0.4))
	if view := plain(m.View()); strings.Contains(view, "ended") {
		t.Errorf("another session finishing marked the one being read ended:\n%s", view)
	}
	m, _ = m.Update(ended("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 87, 2.41))
	if view := plain(m.View()); !strings.Contains(view, "ended") {
		t.Errorf("the session being read finished and the view does not say so:\n%s", view)
	}
}

// A tick left over from a closed session view does not re-arm the loop of
// the one that replaced it. Without the generation test it would: the tick
// is scheduled ahead whatever the reader does, so closing a view and
// opening another inside that window leaves the new view with two loops
// reading the same file, and every such round trip adds one more.
func TestAStaleTickDoesNotRearmTheNextSessionViewsLoop(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	writeTranscript(t, first, "the first session")
	writeTranscript(t, second, "the second session")
	m, _, cmd := watcher(t, first, watched("reviewer-pr-33-r1", config.RoleReviewer, 14, 33, second))
	m, _ = run(t, m, cmd)
	stale := m.(Model).tailGen

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyEsc})
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyDown})
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	live := m.(Model).tailGen
	if live == stale {
		t.Fatalf("the second session view has the generation of the first (%d)", live)
	}
	if _, cmd := m.Update(tailMsg(stale)); hasTail(t, cmd, live) {
		t.Error("a tick of the closed session view's loop re-armed the open one's")
	}
}

// The transcript kept in memory is capped, and dropping the oldest lines
// does not move what the reader is looking at. A session can run for hours
// and nothing else bounds the slice; without the matching shift of the
// scroll position, every line falling off the front would drag the text
// under the reader's eyes forward by one.
func TestALongTranscriptIsCappedWithoutMovingTheReader(t *testing.T) {
	dir := t.TempDir()
	says := make([]string, maxTranscriptLines)
	for i := range says {
		says[i] = fmt.Sprintf("line %04d", i)
	}
	writeTranscript(t, dir, says...)
	m, _, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)
	if got := len(m.(Model).watching.lines); got != maxTranscriptLines {
		t.Fatalf("kept %d lines of a %d-line transcript", got, maxTranscriptLines)
	}

	// Scroll away from the tail, and note the line at the top of the screen.
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyHome})
	for range 12 {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyPgDown})
	}
	top := m.(Model).watching.scroll
	want := fmt.Sprintf("line %04d", top)

	// The session writes 100 more lines, so 100 fall off the front.
	for i := maxTranscriptLines; i < maxTranscriptLines+100; i++ {
		says = append(says, fmt.Sprintf("line %04d", i))
	}
	writeTranscript(t, dir, says...)
	m, _ = run(t, m, m.(Model).readTail())

	if got := len(m.(Model).watching.lines); got != maxTranscriptLines {
		t.Errorf("kept %d lines of a %d-line transcript", got, len(says))
	}
	view := plain(m.View())
	if !strings.Contains(view, "● "+want) {
		t.Errorf("the reader was looking at %q and the truncation moved it:\n%s", want, view)
	}
}

// A singleton runs on no work item at all, so the sentences about a message
// to it simply end: "queued for the next product manager session", not
// "… on no work item".
func TestASingletonSessionIsAboutNoWorkItem(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the feedback")
	box := &[]sent{}
	d := Deps{
		Repo: "acme/widgets",
		Now:  func() time.Time { return fixed },
		Send: func(to string, issue, pr int, subject, body string) error {
			*box = append(*box, sent{to, issue, pr, subject, body})
			return nil
		},
	}
	var m tea.Model = New(d)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 100, Height: 24})
	m, _ = m.Update(watched("product_manager-r1", config.RoleProductManager, 0, 0, dir))
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("m")})
	for _, r := range "look at the feedback queue" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)

	view := plain(m.View())
	if !strings.Contains(view, "queued for the next product manager session") {
		t.Errorf("the view does not say where the message went:\n%s", view)
	}
	for _, unwanted := range []string{"no work item", "product manager session on"} {
		if strings.Contains(view, unwanted) {
			t.Errorf("the view says the message is about a work item (%q):\n%s", unwanted, view)
		}
	}
	if len(*box) != 1 || (*box)[0].to != config.RoleProductManager {
		t.Fatalf("the view delivered %+v, want one message to the product manager", *box)
	}
}

// q stops the factory everywhere except in a message being typed, where it
// is a letter. Ctrl-C cannot be typed and stops the factory there too, so
// nothing is lost by the exception — and without it a person writing "queue
// a retry" loses the factory to their first keystroke.
func TestQIsALetterWhileAMessageIsBeingTyped(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "the session's first words")
	stops, sent := 0, ""
	var m tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed },
		Stop: func() { stops++ },
		Send: func(_ string, _, _ int, _, body string) error { sent = body; return nil },
	})
	m, _ = m.Update(watched("developer-issue-12-r1", config.RoleDeveloper, 12, 31, dir))
	m, cmd := m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	for _, r := range "queue a retry" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}
	if stops != 0 {
		t.Fatalf("typing a message stopped the factory %d times, want 0", stops)
	}
	if view := plain(m.View()); !strings.Contains(view, "› queue a retry") {
		t.Errorf("the q of a typed message did not reach the composer:\n%s", view)
	}
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyEnter})
	m, _ = run(t, m, cmd)
	if sent != "queue a retry" {
		t.Errorf("the queued message is %q, want %q", sent, "queue a retry")
	}

	// The message is over, so q is a stop key again.
	if _, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}); stops != 1 {
		t.Errorf("q asked the factory to stop %d times once the message was sent, want 1", stops)
	}
}

// The session view says the factory is stopping, in the panels footer's own
// words. The first q asks it to stop and the view stays up while the
// running sessions finish; a second press stops those sessions too and says
// so. Without this the screen does not change at all, and a person who
// reads the first press as a dead key presses again without being told what
// that second press now does.
func TestStoppingTheFactoryIsSaidInTheSessionViewToo(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue")
	stops, hard := 0, 0
	m, _, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)
	m = withStop(m, &stops, &hard)

	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if stops != 1 {
		t.Errorf("q asked the factory to stop %d times, want 1", stops)
	}
	if cmd != nil {
		t.Error("q quit the session view instead of waiting for the sessions")
	}
	if got := plain(m.View()); !strings.Contains(got, "stopping: waiting for 1 running session to finish — q or ctrl-c again stops them now") {
		t.Errorf("the session view does not say it is stopping:\n%s", got)
	}
	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if hard != 1 || cmd != nil {
		t.Errorf("a second q must stop the sessions and stay up: hard stops %d, cmd %v", hard, cmd)
	}
	if got := plain(m.View()); !strings.Contains(got, "stopping 1 running session now") {
		t.Errorf("the session view does not say the sessions are being stopped:\n%s", got)
	}
	if _, cmd = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}}); cmd == nil {
		t.Error("a third q did not quit the session view")
	}
}

// The count is text.Count's, the one plural helper, so two sessions read as
// two sessions and the two screens say the same thing.
func TestTheSessionViewCountsTheSessionsItIsWaitingFor(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue")
	m, _, cmd := watcher(t, dir, started("reviewer-pr-31-r1", config.RoleReviewer, 12, 31, fixed, "opus", false))
	m, _ = run(t, m, cmd)

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if got := plain(m.View()); !strings.Contains(got, "stopping: waiting for 2 running sessions to finish") {
		t.Errorf("the session view miscounts what it is waiting for:\n%s", got)
	}
}

// With nothing left running there is nothing to wait for, and the session
// view says so in the panels footer's words. A session that ended while it
// was being read is still on screen, which is how a person gets here.
func TestTheSessionViewSaysStoppingWithNoSessionsLeft(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "opening the pull request")
	m, _, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)
	m, _ = m.Update(ended("developer-issue-12-r1", config.RoleDeveloper, 12, 31, 87, 2.41))

	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if got := plain(m.View()); !strings.Contains(got, "stopping: nothing left running") {
		t.Errorf("the session view does not say nothing is left running:\n%s", got)
	}
}

// Ctrl-C stops the factory while a message is being typed too, so the
// composer's own footer must not hide it. The draft is not lost: the view
// draws it on its own line above the footer.
func TestCtrlCWhileTypingAMessageSaysTheFactoryIsStopping(t *testing.T) {
	dir := t.TempDir()
	writeTranscript(t, dir, "reading the issue")
	stops := 0
	m, _, cmd := watcher(t, dir)
	m, _ = run(t, m, cmd)
	m = withStop(m, &stops, nil)
	m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'m'}})
	for _, r := range "queue a retry" {
		m, _ = m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	}

	m, cmd = m.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if stops != 1 {
		t.Fatalf("ctrl-c asked the factory to stop %d times while a message was being typed, want 1", stops)
	}
	if cmd != nil {
		t.Error("ctrl-c quit the session view instead of waiting for the drain")
	}
	got := plain(m.View())
	if !strings.Contains(got, "stopping: waiting for 1 running session to finish") {
		t.Errorf("the composer's footer hides that the factory is stopping:\n%s", got)
	}
	if !strings.Contains(got, "› queue a retry") {
		t.Errorf("the draft is gone from the screen:\n%s", got)
	}
}

// withStop gives a model built by watcher somewhere to count the two stops.
// watcher builds its own Deps, and the counters are the one thing these
// tests need that a message box cannot carry. hard may be nil.
func withStop(m tea.Model, stops, hard *int) tea.Model {
	v := m.(Model)
	v.deps.Stop = func() { *stops++ }
	if hard != nil {
		v.deps.HardStop = func() { *hard++ }
	}
	return v
}
