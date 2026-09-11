package reviewtui

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/kpenfound/busybees/internal/review"
)

func TestTheScreenShowsTheFirstUndecidedFindingBesideTheDiff(t *testing.T) {
	a := judged(t)
	m := screen(t, queueOf(t, a))
	f := a.Findings.Items[0]
	v := view(m)
	has(t, v,
		"[1 of 3 findings undecided] "+f.ID+" · high · test_coverage · missing test",
		"acme/widgets#7",
		// The diff pane, named after the finding's file, with the finding's
		// lines marked in the gutter and its suggestion under the last of
		// them.
		"▸ Diff · gather.go",
		"▌     12 +    c(1)",
		"▌     13 +    d()",
		"▌ 13  14      e()",
		"suggestion:",
		"│ func TestASourceThatCannotRead(t *testing.T) {",
		"─ before.go → after.go",
		// The finding pane: what the console prints for it.
		"gather.go:12-14",
		"Gather has no test for a source that cannot",
		"nothing exercises Skipped",
		"Suggestion:",
		"s select · e edit · d dismiss · f defer · a ask · n next",
	)
	// A line the finding is not about is not marked, and the removed line
	// inside its range on the other side is not either.
	has(t, v, "  11  11      b()", "  12     -    c()")
	lacks(t, v, "▌  11", "▌  12     -", "not in the diff")
}

func TestTheDiffOpensOnTheFindingsLines(t *testing.T) {
	m := screen(t, queueOf(t, judged(t)))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 12})
	v := view(m)
	// Seven rows of diff: the hunk's header and the marked lines, not the
	// file name above them.
	has(t, v, "@@ -10,6 +10,7 @@ func Gather() {", "▌     12 +    c(1)", "▌ 13  14      e()")
	lacks(t, v, "─ gather.go")
}

func TestAFindingTheDiffDoesNotHaveMarksNothingAndSaysSo(t *testing.T) {
	a := judged(t)
	m := screen(t, queueOf(t, a))
	m, _ = press(m, "n")
	v := view(m)
	has(t, v, "[2 of 3 findings undecided] "+a.Findings.Items[1].ID+" · medium",
		"README.md:3", "(not in the diff: posted in the review's", "summary, not on a line)", "▸ Diff · 1-")
	lacks(t, v, "▌", "suggestion:", "Diff · README")

	// A finding about the change as a whole has no place to be in the diff,
	// and the screen does not say it is missing from it.
	m, _ = press(m, "n")
	v = view(m)
	has(t, v, "[3 of 3 findings undecided]", "every caller moves for a rename nobody")
	lacks(t, v, "▌", "not in the diff")
}

func TestEachDecisionIsWrittenBeforeTheNextFindingIsShown(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	m := screen(t, q)
	f := a.Findings.Items

	m, cmd := press(m, "s")
	if got := decisions(t, q); len(got) != 1 || got[0].Finding != f[0].ID || got[0].Action != review.ActionSelect || got[0].Comment != "" {
		t.Errorf("after s the artifact holds %+v, want the first finding selected as written", got)
	}
	if quits(cmd) {
		t.Error("the screen closed with findings still undecided")
	}
	has(t, view(m), "[1 of 2 findings undecided] "+f[1].ID)

	m, _ = press(m, "d")
	has(t, view(m), "reason (recorded in your reviewer notes): ▏")
	m = typed(m, "the README is rewritten in #12")
	has(t, view(m), "reason (recorded in your reviewer notes): the README is rewritten in #12▏")
	m, _ = press(m, "enter")
	if got := decisions(t, q); len(got) != 2 || got[1].Finding != f[1].ID || got[1].Action != review.ActionDismiss || got[1].Reason != "the README is rewritten in #12" {
		t.Errorf("after d the artifact holds %+v, want the second finding dismissed with the reason typed", got)
	}
	notes, err := os.ReadFile(q.Notes.Path)
	if err != nil || !strings.Contains(string(notes), "the README is rewritten in #12") {
		t.Errorf("the reviewer notes hold %q (%v), want the reason", notes, err)
	}
	has(t, view(m), "[1 of 1 finding undecided] "+f[2].ID)

	m, cmd = press(m, "f")
	if got := decisions(t, q); len(got) != 3 || got[2].Finding != f[2].ID || got[2].Action != review.ActionDefer {
		t.Errorf("after f the artifact holds %+v, want the third finding deferred", got)
	}
	if !quits(cmd) {
		t.Error("nothing is left undecided and the screen stays open")
	}
	if err := m.(Model).Err(); err != nil {
		t.Errorf("triage ended with %v, want no error", err)
	}
}

func TestEditTakesTheCommentTextOnScreen(t *testing.T) {
	// Each case gets a review of its own: the artifact is written to.
	t.Run("typed at the end and selected with ctrl-d", func(t *testing.T) {
		a := judged(t)
		f := a.Findings.Items[0]
		q := queueOf(t, a)
		m := screen(t, q)
		m, _ = press(m, "e")
		has(t, view(m), "Finding · comment text", "nothing exercises Skipped▏", "ctrl-d selects with this text")
		m = typed(m, " and here is why")
		m, _ = press(m, "ctrl+d")
		got := decisions(t, q)
		if len(got) != 1 || got[0].Action != review.ActionSelect || got[0].Comment != f.Comment()+" and here is why" {
			t.Errorf("the artifact holds %+v, want the finding selected with the text as edited", got)
		}
		has(t, view(m), "[1 of 2 findings undecided]")
	})
	t.Run("return starts a new line", func(t *testing.T) {
		a := judged(t)
		f := a.Findings.Items[0]
		q := queueOf(t, a)
		m := screen(t, q)
		m, _ = press(m, "e", "enter")
		m = typed(m, "more")
		m, _ = press(m, "ctrl+d")
		if got := decisions(t, q); len(got) != 1 || got[0].Comment != f.Comment()+"\nmore" {
			t.Errorf("the artifact holds %+v, want a comment with a new line", got)
		}
		has(t, view(m), "[1 of 2 findings undecided]")
	})
	t.Run("esc cancels", func(t *testing.T) {
		a := judged(t)
		f := a.Findings.Items[0]
		q := queueOf(t, a)
		m := screen(t, q)
		m, _ = press(m, "e")
		m = typed(m, "x")
		has(t, view(m), f.Comment()[:20])
		m, _ = press(m, "esc")
		if got := decisions(t, q); len(got) != 0 {
			t.Errorf("the artifact holds %+v, want nothing", got)
		}
		has(t, view(m), "[1 of 3 findings undecided]", "s select · e edit")
		lacks(t, view(m), "▏")
	})
	t.Run("an empty text selects nothing", func(t *testing.T) {
		a := judged(t)
		f := a.Findings.Items[0]
		q := queueOf(t, a)
		m := screen(t, q)
		m, _ = press(m, "e")
		for range []rune(f.Comment()) {
			m, _ = press(m, "backspace")
		}
		m, _ = press(m, "ctrl+d")
		if got := decisions(t, q); len(got) != 0 {
			t.Errorf("the artifact holds %+v, want nothing", got)
		}
		has(t, view(m), "the comment text is empty, so the finding stays undecided", "[1 of 3 findings undecided]")
	})
}

func TestNextMovesOnAndComesRoundAgain(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	m := screen(t, q)
	for i, want := range []int{1, 2, 0} {
		m, _ = press(m, "n")
		has(t, view(m), "] "+a.Findings.Items[want].ID+" ·")
		if got := decisions(t, q); len(got) != 0 {
			t.Errorf("after %d n the artifact holds %+v, want nothing", i+1, got)
		}
	}
}

func TestQuitKeepsWhatWasDecided(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	m := screen(t, q)
	m, _ = press(m, "s")
	m, cmd := press(m, "q")
	if !quits(cmd) {
		t.Error("q did not close the screen")
	}
	if got := decisions(t, q); len(got) != 1 {
		t.Errorf("the artifact holds %+v, want the one decision", got)
	}
	if err := m.(Model).Err(); err != nil {
		t.Errorf("quitting reported %v", err)
	}
	// Ctrl-C closes the screen from wherever it is: typing a reason too.
	m, _ = press(screen(t, q), "d")
	if _, cmd := press(m, "ctrl+c"); !quits(cmd) {
		t.Error("ctrl-c while typing did not close the screen")
	}
}

func TestAskReopensTheAngleAndShowsTheAnswer(t *testing.T) {
	a := judged(t)
	agent := &fakeAgent{text: "The test exists: gather_test.go covers Skipped."}
	q := askingQueue(t, a, agent)
	m := screen(t, q)
	m, _ = press(m, "a")
	has(t, view(m), "question for the test_coverage angle: ▏")
	m = typed(m, "is there a test already?")
	m, cmd := press(m, "enter")
	if cmd == nil {
		t.Fatal("the question was not asked")
	}
	has(t, view(m), "asking the test_coverage angle…")
	// Nothing is decided while the angle is answering.
	m, _ = press(m, "s")
	if got := decisions(t, q); len(got) != 0 {
		t.Errorf("a key during an ask wrote %+v", got)
	}
	has(t, view(m), "asking the test_coverage angle…")

	m, _ = m.Update(cmd())
	v := view(m)
	has(t, v,
		"Q: is there a test already?",
		"A: The test exists: gather_test.go",
		"answered",
		"[1 of 3 findings undecided] "+a.Findings.Items[0].ID,
		"▸ Finding",
	)
	if len(agent.reqs) != 1 || agent.reqs[0].ResumeID != "sess-tests" || !strings.Contains(agent.reqs[0].Prompt, "is there a test already?") {
		t.Errorf("the angle was asked %+v, want its session resumed with the question", agent.reqs)
	}
	got := decisions(t, q)
	if len(got) != 1 || got[0].Action != review.ActionAsk || got[0].Answer != agent.text {
		t.Errorf("the artifact holds %+v, want the ask recorded", got)
	}
}

func TestAnAnswerThatAddsFindingsSaysSo(t *testing.T) {
	a := judged(t)
	agent := &fakeAgent{text: "Looked again.\n```json\n{\"findings\": [{\"category\": \"missing test\", \"severity\": \"low\", \"file\": \"gather.go\", \"lines\": [41, 44], \"side\": \"new\", \"title\": \"Other has no test either\", \"body\": \"w() is never called\"}]}\n```"}
	q := askingQueue(t, a, agent)
	m := screen(t, q)
	m, _ = press(m, "a")
	m = typed(m, "anything else?")
	m, cmd := press(m, "enter")
	m, _ = m.Update(cmd())
	has(t, view(m), "answered; the answer added 1 finding to the queue", "[1 of 4 findings undecided]", "A: Looked again.")
	lacks(t, view(m), "```")
	// The finding it added is in the queue, on its lines.
	for range 3 {
		m, _ = press(m, "n")
		if strings.Contains(view(m), "Other has no test either") {
			has(t, view(m), "of 4 findings undecided]", "▌     44 +    w()")
			return
		}
	}
	t.Errorf("the finding the answer added is not in the queue:\n%s", view(m))
}

func TestARefusalIsShownAndTheFindingStays(t *testing.T) {
	t.Run("a dismissal with no reason", func(t *testing.T) {
		a := judged(t)
		q := queueOf(t, a)
		m := screen(t, q)
		m, _ = press(m, "d", "enter")
		has(t, view(m), "a dismissal needs a reason", "[1 of 3 findings undecided] "+a.Findings.Items[0].ID)
		if got := decisions(t, q); len(got) != 0 {
			t.Errorf("the artifact holds %+v, want nothing", got)
		}
		// The next key clears it.
		m, _ = press(m, "n")
		lacks(t, view(m), "a dismissal needs a reason")
	})
	t.Run("an ask with no session to reopen", func(t *testing.T) {
		q := askingQueue(t, judged(t), &fakeAgent{text: "never asked"})
		m := screen(t, q)
		m, _ = press(m, "n", "n", "a")
		m = typed(m, "why?")
		m, cmd := press(m, "enter")
		m, _ = m.Update(cmd())
		has(t, view(m), "ask failed: the acceptance_criteria angle has no session in this review", "[3 of 3 findings undecided]")
		if got := decisions(t, q); len(got) != 0 {
			t.Errorf("the artifact holds %+v, want nothing", got)
		}
	})
	t.Run("a session that fails", func(t *testing.T) {
		q := askingQueue(t, judged(t), &fakeAgent{err: errors.New("claude: exit status 1")})
		m := screen(t, q)
		m, _ = press(m, "a")
		m = typed(m, "why?")
		m, cmd := press(m, "enter")
		m, _ = m.Update(cmd())
		has(t, view(m), "ask failed: claude: exit status 1", "[1 of 3 findings undecided]")
		if err := m.(Model).Err(); err != nil {
			t.Errorf("a failed session ended triage with %v", err)
		}
	})
}

func TestADecisionThatCannotBeWrittenClosesTheScreen(t *testing.T) {
	q := queueOf(t, judged(t))
	q.Artifact.Dir = filepath.Join(q.Artifact.Dir, review.BriefFile, "review")
	m := screen(t, q)
	m, cmd := press(m, "s")
	if !quits(cmd) {
		t.Error("the screen stayed open after a decision that was not written")
	}
	if err := m.(Model).Err(); err == nil {
		t.Error("Err is nil, want the write error")
	}
}

func TestNothingUndecidedClosesTheScreenAtOnce(t *testing.T) {
	q := queueOf(t, judged(t))
	for _, f := range q.Findings() {
		if err := q.Defer(f.ID); err != nil {
			t.Fatal(err)
		}
	}
	m := New(t.Context(), sampleDiff, q)
	if !quits(m.Init()) {
		t.Error("the screen opened on a queue with nothing to decide")
	}
	m2 := New(t.Context(), sampleDiff, queueOf(t, judged(t)))
	if quits(m2.Init()) {
		t.Error("the screen closed on a queue with findings to decide")
	}
}

func TestHelpListsEveryKeyAndAnyKeyClosesIt(t *testing.T) {
	q := queueOf(t, judged(t))
	m := screen(t, q)
	m, _ = press(m, "?")
	v := view(m)
	has(t, v, "Keys", "any key closes the help")
	for _, key := range []string{"s  select", "e  edit", "d  dismiss", "f  defer", "a  ask", "n  next", "q  quit", "tab", "?  this help"} {
		has(t, v, key)
	}
	m, _ = press(m, "s")
	has(t, view(m), "Finding", "s select · e edit")
	lacks(t, view(m), "any key closes the help")
	if got := decisions(t, q); len(got) != 0 {
		t.Errorf("the key that closed the help decided %+v", got)
	}
	m, _ = press(m, "?")
	if _, cmd := press(m, "q"); !quits(cmd) {
		t.Error("q on the help did not close the screen")
	}
}

func TestTheScrollKeysMoveTheFocusedPane(t *testing.T) {
	m := screen(t, queueOf(t, judged(t)))
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 12})
	has(t, view(m), "▸ Diff · gather.go · 2-8 of 28", "Finding · 1-7 of")
	m, _ = press(m, "down")
	has(t, view(m), "▸ Diff · gather.go · 3-9 of 28", "Finding · 1-7 of")
	m, _ = press(m, "end")
	has(t, view(m), "▸ Diff · gather.go · 22-28 of 28")
	m, _ = press(m, "home")
	has(t, view(m), "▸ Diff · gather.go · 1-7 of 28")
	m, _ = press(m, "tab", "down")
	has(t, view(m), "Diff · gather.go · 1-7 of 28", "▸ Finding · 2-8 of")
	m, _ = press(m, "up", "up")
	has(t, view(m), "▸ Finding · 1-7 of")
	m, _ = press(m, "pgdown")
	lacks(t, view(m), "▸ Finding · 1-7 of")
	// A new finding starts at the top of both panes, the diff on the
	// finding's lines.
	m, _ = press(m, "n", "n", "n")
	has(t, view(m), "▸ Diff · gather.go · 2-8 of 28")
}

func TestTheScreenFitsTheTerminalItIsDrawnIn(t *testing.T) {
	q := queueOf(t, judged(t))
	for _, size := range [][2]int{{120, 30}, {100, 20}, {80, 24}, {60, 16}, {40, 12}} {
		m := screen(t, q)
		m, _ = m.Update(tea.WindowSizeMsg{Width: size[0], Height: size[1]})
		for _, state := range []string{"", "e", "?"} {
			if state != "" {
				m, _ = press(m, state)
			}
			lines := strings.Split(view(m), "\n")
			if len(lines) != size[1] {
				t.Errorf("%dx%d after %q: %d lines drawn", size[0], size[1], state, len(lines))
			}
			for i, l := range lines {
				if w := lipgloss.Width(l); w > size[0] {
					t.Errorf("%dx%d after %q: line %d is %d wide: %q", size[0], size[1], state, i, w, l)
				}
			}
			if state != "" {
				m, _ = press(m, "esc")
			}
		}
	}
	// A narrow terminal stacks the panes; a wide one puts them side by
	// side.
	m := screen(t, q)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	if v := view(m); strings.Contains(v, "╮╭") {
		t.Errorf("an 80-column terminal drew the panes side by side:\n%s", v)
	}
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 24})
	if v := view(m); !strings.Contains(v, "╮╭") {
		t.Errorf("a 120-column terminal stacked the panes:\n%s", v)
	}
}

func TestAnEmptyDiffSaysSo(t *testing.T) {
	q := queueOf(t, judged(t))
	var m tea.Model = New(t.Context(), "", q)
	has(t, view(m), "the diff is empty", "gather.go:12-14", "(not in the diff: posted in the review's summary, not on a line)")
}

func TestWrapBreaksAtSpacesAndKeepsTheIndent(t *testing.T) {
	for _, tc := range []struct {
		in   string
		w    int
		want []string
	}{
		{"short", 10, []string{"short"}},
		{"one two three four", 9, []string{"one two", "three", "four"}},
		{"  func a() { return b }", 12, []string{"  func a() {", "  return b }"}},
		{"abcdefghijkl", 5, []string{"abcde", "fghij", "kl"}},
		{"a\n\nb", 5, []string{"a", "", "b"}},
		{"tab\there", 20, []string{"tab    here"}},
	} {
		if got := wrap(tc.in, tc.w); strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("wrap(%q, %d) = %q, want %q", tc.in, tc.w, got, tc.want)
		}
	}
}

// ansi matches an escape sequence.
var ansi = regexp.MustCompile("\x1b\\[[0-9;]*m")

// plain strips the escape sequences from s.
func plain(s string) string { return ansi.ReplaceAllString(s, "") }
