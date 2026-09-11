package reviewtui

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/review"
)

// sampleDiff is the diff internal/review's own tests use (output_test.go):
// two hunks in one file, a deleted file, a rename, and lines that start
// with two dashes or two pluses.
const sampleDiff = `diff --git a/gather.go b/gather.go
index 1111111..2222222 100644
--- a/gather.go
+++ b/gather.go
@@ -10,6 +10,7 @@ func Gather() {
 	a()
 	b()
-	c()
+	c(1)
+	d()
 	e()
 	f()
 	g()
@@ -40,3 +41,4 @@ func Other() {
 	x()
 	y()
 	z()
+	w()
diff --git a/old.go b/old.go
deleted file mode 100644
index 3333333..0000000
--- a/old.go
+++ /dev/null
@@ -1,2 +0,0 @@
-package old
-
diff --git a/before.go b/after.go
similarity index 90%
rename from before.go
rename to after.go
--- a/before.go
+++ b/after.go
@@ -1,2 +1,2 @@
 package widgets
-// before
+// after
\ No newline at end of file
`

// judged is a review with three findings, most severe first, as
// internal/review's triage tests build one: the first is on the diff's new
// side with a suggestion, the second names a file the diff does not have,
// the third is about the change as a whole and comes from an angle with no
// session to reopen.
func judged(t *testing.T) *review.Artifact {
	t.Helper()
	items := review.Merge([]review.Finding{
		{Angle: review.AngleTests, SessionID: "sess-tests", Category: "missing test", Severity: review.SeverityHigh, File: "gather.go", Lines: review.LineRange{Start: 12, End: 14}, Side: review.SideNew,
			Title: "Gather has no test for a source that cannot read", Body: "nothing exercises Skipped", Suggestion: "func TestASourceThatCannotRead(t *testing.T) {"},
		{Angle: review.AngleTests, SessionID: "sess-tests", Category: "docs", Severity: review.SeverityMedium, File: "README.md", Lines: review.LineRange{Start: 3, End: 3}, Side: review.SideNew,
			Title: "The README still claims a source failure stops everything", Body: "the sentence the change made false is still there"},
		{Angle: review.AngleAcceptance, Category: "scope", Severity: review.SeverityLow,
			Title: "The change renames Gather, which the issue did not ask for", Body: "every caller moves for a rename nobody wanted"},
	}, nil)
	a := &review.Artifact{
		Dir:   filepath.Join(t.TempDir(), "review"),
		Brief: &review.Brief{Ref: review.Ref{Repo: "acme/widgets", Number: 7}, Title: "widgets: gather the context", Author: "octocat", Summary: "gathers the context sources"},
		Runs: []review.AngleRun{
			{Angle: review.AngleTests, Provider: config.AgentClaude, Model: "opus", Dir: t.TempDir(), SessionID: "sess-tests", Answer: `{"findings": []}`},
		},
		Findings: &review.Findings{Items: items},
	}
	if err := a.Write(); err != nil {
		t.Fatal(err)
	}
	return a
}

// queueOf is a queue over a with reviewer notes to dismiss into and no
// agent to ask.
func queueOf(t *testing.T, a *review.Artifact) *review.Queue {
	t.Helper()
	q, err := review.NewQueue(a, nil, &review.Notes{Path: filepath.Join(t.TempDir(), "reviewer-notes.md")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

// fakeAgent answers every session with one text, and remembers what it was
// asked.
type fakeAgent struct {
	text string
	err  error

	mu   sync.Mutex
	reqs []review.AgentRequest
}

func (f *fakeAgent) Run(_ context.Context, req review.AgentRequest) (*review.AgentResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reqs = append(f.reqs, req)
	if f.err != nil {
		return nil, f.err
	}
	return &review.AgentResult{ID: req.ResumeID, Text: f.text}, nil
}

// askingQueue is queueOf with agent to reopen the angle sessions with.
func askingQueue(t *testing.T, a *review.Artifact, agent review.Agent) *review.Queue {
	t.Helper()
	q := queueOf(t, a)
	q.Angles = &review.Angles{Agent: agent, Provider: config.AgentClaude, Model: "opus"}
	return q
}

// screen is the screen over q, sized like a terminal.
func screen(t *testing.T, q *review.Queue) tea.Model {
	t.Helper()
	var m tea.Model = New(t.Context(), sampleDiff, q)
	m, _ = m.Update(tea.WindowSizeMsg{Width: 120, Height: 30})
	return m
}

// press presses keys on the screen, one message per key, and returns the
// model and the last command. A word is typed rune by rune; a name in
// tea's key vocabulary ("enter", "esc", "ctrl+d", "tab", "down") is that
// key.
func press(m tea.Model, keys ...string) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	for _, k := range keys {
		m, cmd = m.Update(keyMsg(k))
	}
	return m, cmd
}

// keyMsg is one key by name.
func keyMsg(k string) tea.KeyMsg {
	switch k {
	case "enter":
		return tea.KeyMsg{Type: tea.KeyEnter}
	case "esc":
		return tea.KeyMsg{Type: tea.KeyEsc}
	case "tab":
		return tea.KeyMsg{Type: tea.KeyTab}
	case "backspace":
		return tea.KeyMsg{Type: tea.KeyBackspace}
	case "ctrl+d":
		return tea.KeyMsg{Type: tea.KeyCtrlD}
	case "ctrl+c":
		return tea.KeyMsg{Type: tea.KeyCtrlC}
	case "up":
		return tea.KeyMsg{Type: tea.KeyUp}
	case "down":
		return tea.KeyMsg{Type: tea.KeyDown}
	case "pgdown":
		return tea.KeyMsg{Type: tea.KeyPgDown}
	case "end":
		return tea.KeyMsg{Type: tea.KeyEnd}
	case "home":
		return tea.KeyMsg{Type: tea.KeyHome}
	case " ":
		return tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}}
	}
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
}

// typed presses the runes of s one by one, a space as the space key.
func typed(m tea.Model, s string) tea.Model {
	for _, r := range s {
		if r == ' ' {
			m, _ = press(m, " ")
		} else {
			m, _ = press(m, string(r))
		}
	}
	return m
}

// quits reports whether cmd is tea.Quit.
func quits(cmd tea.Cmd) bool {
	if cmd == nil {
		return false
	}
	_, ok := cmd().(tea.QuitMsg)
	return ok
}

// decisions reads the triage state back from the artifact directory: what
// has been written, not what the queue remembers.
func decisions(t *testing.T, q *review.Queue) []review.Decision {
	t.Helper()
	if _, err := os.Stat(filepath.Join(q.Artifact.Dir, review.TriageFile)); os.IsNotExist(err) {
		return nil
	}
	tr, err := review.ReadTriage(q.Artifact.Dir)
	if err != nil {
		t.Fatal(err)
	}
	return tr.Decisions
}

// view is the screen drawn, its lines.
func view(m tea.Model) string { return m.View() }

// has fails the test unless the view holds every want, in any order.
func has(t *testing.T, v string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(v, w) {
			t.Errorf("the screen lacks %q:\n%s", w, v)
		}
	}
}

// lacks fails the test if the view holds any of the wants.
func lacks(t *testing.T, v string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if strings.Contains(v, w) {
			t.Errorf("the screen holds %q, want it gone:\n%s", w, v)
		}
	}
}
