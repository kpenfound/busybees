package review

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// drive runs the console over q with input typed at it, and returns what it
// printed.
func drive(t *testing.T, q *Queue, input string, editor func(string) (string, error)) string {
	t.Helper()
	var out bytes.Buffer
	c := &Console{In: strings.NewReader(input), Out: &out, Editor: editor}
	if err := c.Run(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	return out.String()
}

// order is the sequence in which the findings were shown, by the index each
// has in the review.
func order(out string, findings []Finding) []int {
	var shown []int
	for _, line := range strings.Split(out, "\n") {
		if !strings.HasPrefix(line, "[") {
			continue
		}
		for i, f := range findings {
			if strings.Contains(line, " "+f.ID+" ") {
				shown = append(shown, i)
			}
		}
	}
	return shown
}

func TestTheConsoleShowsEachUndecidedFindingAndTakesOneKeyForIt(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	out := drive(t, q, "s\nd\nthe README is rewritten in #12\nf\n", nil)
	// Each finding once, most severe first, whole.
	if got := order(out, a.Findings.Items); len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Errorf("shown in order %v, want each finding once, most severe first:\n%s", got, out)
	}
	f := a.Findings.Items[0]
	for _, want := range []string{
		"[1 of 3 findings undecided] " + f.ID + " · high · test_coverage · missing test\n",
		"gather.go:12-14\n",
		f.Title + "\n\n" + f.Body + "\n",
		"Suggestion:\n  func TestASourceThatCannotRead(t *testing.T) {\n",
		"[1 of 2 findings undecided] " + a.Findings.Items[1].ID,
		"[1 of 1 finding undecided] " + a.Findings.Items[2].ID,
		"reason (recorded in your reviewer notes): ",
		"1 selected, 1 dismissed, 1 deferred, 0 undecided of 3 findings\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the console did not print %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "e edit and select") {
		t.Errorf("the edit key is offered without an editor:\n%s", out)
	}
	// Every key went into the artifact and the dismissal into the notes.
	decisions := written(t, q).Decisions
	if len(decisions) != 3 || decisions[0].Action != ActionSelect || decisions[1].Action != ActionDismiss || decisions[1].Reason != "the README is rewritten in #12" || decisions[2].Action != ActionDefer {
		t.Errorf("triage.json holds %+v", decisions)
	}
	notes, err := os.ReadFile(q.Notes.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(notes), "- [acme/widgets] [test_coverage] [docs] the README is rewritten in #12\n") {
		t.Errorf("the notes do not hold the dismissal:\n%s", notes)
	}
	if len(q.Pending()) != 0 {
		t.Error("something is still pending")
	}
}

func TestTheConsoleStopsAtQuitAndWhenTheInputEnds(t *testing.T) {
	for input, shown := range map[string]int{"q\n": 1, "quit\n": 1, "": 1, "s\n": 2} {
		a := judged(t)
		q := queueOf(t, a)
		out := drive(t, q, input, nil)
		if got := order(out, a.Findings.Items); len(got) != shown {
			t.Errorf("input %q: shown %v, want %d findings shown before triage stopped:\n%s", input, got, shown, out)
		}
		want := "0 selected, 0 dismissed, 0 deferred, 3 undecided of 3 findings\n"
		if input == "s\n" {
			want = "1 selected, 0 dismissed, 0 deferred, 2 undecided of 3 findings\n"
		}
		if !strings.Contains(out, want) {
			t.Errorf("input %q: the console did not print %q:\n%s", input, want, out)
		}
	}
	// A dismissal or a question cut short by the end of the input decides
	// nothing.
	for _, input := range []string{"d\n", "a\n"} {
		a := judged(t)
		q := queueOf(t, a)
		q.Angles = &Angles{Agent: &fakeAgent{answer: "Yes."}, Provider: config.AgentClaude}
		drive(t, q, input, nil)
		if got := written(t, q).Decisions; len(got) != 0 {
			t.Errorf("input %q: triage.json holds %+v, want nothing", input, got)
		}
	}
}

func TestTheConsoleLeavesAFindingForLaterOnNext(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	out := drive(t, q, "n\ns\nn\n", nil)
	if got := order(out, a.Findings.Items); len(got) != 3 || got[0] != 0 || got[1] != 1 || got[2] != 2 {
		t.Errorf("shown in order %v, want the passed-over finding not shown again this run:\n%s", got, out)
	}
	if got := ids(q.Pending()); len(got) != 2 || got[0] != a.Findings.Items[0].ID || got[1] != a.Findings.Items[2].ID {
		t.Errorf("pending %v, want the two passed over", got)
	}
	if !strings.Contains(out, "1 selected, 0 dismissed, 0 deferred, 2 undecided of 3 findings\n") {
		t.Errorf("the summary is wrong:\n%s", out)
	}
}

func TestTheConsoleEditsTheCommentTextBeforeSelecting(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	var given string
	editor := func(text string) (string, error) {
		given = text
		return "Shorter.\n", nil
	}
	out := drive(t, q, "?\ne\nq\n", editor)
	if given != a.Findings.Items[0].Comment() {
		t.Errorf("the editor was given %q, want the finding's own text", given)
	}
	for _, want := range []string{"e edit and select", "  e  edit the comment text in your editor, then select\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the edit key is not offered with an editor, %q is missing:\n%s", want, out)
		}
	}
	if got := q.Selected(); len(got) != 1 || got[0].Comment != "Shorter." {
		t.Errorf("selected %+v, want the finding with the edited text", got)
	}
	// An editor that fails, or leaves nothing, decides nothing, and the
	// finding is shown again.
	for _, tc := range []struct {
		name   string
		editor func(string) (string, error)
		want   string
	}{
		{"fails", func(string) (string, error) { return "", errors.New("vi: not found") }, "the editor failed: vi: not found\n"},
		{"empties", func(string) (string, error) { return " \n", nil }, "the comment text is empty, so the finding stays undecided\n"},
		{"is not there", nil, "no editor: set $VISUAL or $EDITOR to edit the comment text\n"},
	} {
		a := judged(t)
		q := queueOf(t, a)
		out := drive(t, q, "e\nq\n", tc.editor)
		if !strings.Contains(out, tc.want) {
			t.Errorf("editor %s: the console did not print %q:\n%s", tc.name, tc.want, out)
		}
		if got := order(out, a.Findings.Items); len(got) != 2 || got[1] != 0 {
			t.Errorf("editor %s: shown %v, want the finding shown again:\n%s", tc.name, got, out)
		}
		if len(q.Pending()) != 3 {
			t.Errorf("editor %s: something was decided", tc.name)
		}
	}
}

func TestTheConsoleAsksTheAngleAndShowsTheAnswerWithTheFinding(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	answer := "Reading sources.go again, there is one more.\n\n```json\n" + sessionAnswer(rawFromAsk) + "\n```\n"
	agent := &fakeAgent{answer: answer, id: "sess-tests"}
	q.Angles = &Angles{Agent: agent, Provider: config.AgentClaude, Model: "opus"}
	out := drive(t, q, "a\nAnything else in sources.go?\ns\nq\n", nil)
	if !strings.Contains(agent.req.Prompt, "Anything else in sources.go?") || agent.req.ResumeID != "sess-tests" {
		t.Errorf("the angle was asked %+v", agent.req)
	}
	added := q.Findings()[1]
	for _, want := range []string{
		"question for the test_coverage angle: ",
		"\nReading sources.go again, there is one more.\n",
		"the answer added 1 finding to the queue:\n  " + added.ID + "  high  Collect swallows the read error\n",
		// The finding shown again, with the exchange under it, then
		// selected; the added finding is next.
		"\nQ: Anything else in sources.go?\nA: Reading sources.go again, there is one more.\n",
		"[1 of 3 findings undecided] " + added.ID + " · high · test_coverage · error handling\nsources.go:40-41\n",
		"1 selected, 0 dismissed, 0 deferred, 3 undecided of 4 findings\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the console did not print %q:\n%s", want, out)
		}
	}
	if got := order(out, a.Findings.Items); len(got) != 3 || got[0] != 0 || got[1] != 0 || got[2] != 1 {
		t.Errorf("shown in order %v, want the asked finding again and then the one the answer added:\n%s", got, out)
	}
	// An ask alone decides nothing, and the summary says so.
	a = judged(t)
	q = queueOf(t, a)
	q.Angles = &Angles{Agent: &fakeAgent{answer: "Yes.", id: "sess-tests"}, Provider: config.AgentClaude}
	out = drive(t, q, "a\nSure?\nq\n", nil)
	if !strings.Contains(out, "0 selected, 0 dismissed, 0 deferred, 3 undecided of 3 findings\n") {
		t.Errorf("the summary after an ask alone is wrong:\n%s", out)
	}
	// An ask that fails is said, and the finding is shown again.
	a = judged(t)
	q = queueOf(t, a)
	q.Angles = &Angles{Agent: &fakeAgent{err: errors.New("no capacity")}, Provider: config.AgentClaude}
	out = drive(t, q, "a\nSure?\nq\n", nil)
	if !strings.Contains(out, "ask failed: no capacity\n") {
		t.Errorf("the console did not say what the ask failed with:\n%s", out)
	}
	if got := order(out, a.Findings.Items); len(got) != 2 || got[1] != 0 {
		t.Errorf("shown %v, want the finding shown again after the failed ask:\n%s", got, out)
	}
}

func TestTheConsoleRefusesADismissalWithNoReasonAndAnUnknownKey(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	out := drive(t, q, "d\n\nx\n?\nf\nq\n", nil)
	for _, want := range []string{
		"a dismissal needs a reason: it is what your reviewer notes are made of\n",
		"\"x\" is not a key\n",
		"  d  dismiss: leave it out, and record why in your reviewer notes\n",
		"0 selected, 0 dismissed, 1 deferred, 2 undecided of 3 findings\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the console did not print %q:\n%s", want, out)
		}
	}
	if got := order(out, a.Findings.Items); len(got) != 5 || got[3] != 0 || got[4] != 1 {
		t.Errorf("shown %v, want the first finding shown until a key decided it:\n%s", got, out)
	}
	if _, err := os.Stat(q.Notes.Path); !os.IsNotExist(err) {
		t.Errorf("the refused dismissal wrote the notes: %v", err)
	}
}

func TestTheConsoleShowsWhatTheFindingIsAnchoredTo(t *testing.T) {
	a := judged(t)
	a.Findings.Items[0].Side = SideOld
	a.Findings.Items[0].Evidence = "the table test's cases"
	a.Findings.Items[0].Sources = []string{"#566", "CONTRIBUTING.md"}
	a.Findings.Items[0].AlsoFrom = []string{AngleAcceptance}
	q := queueOf(t, a)
	out := drive(t, q, "n\nn\nn\n", nil)
	for _, want := range []string{
		"gather.go:12-14 (removed)\n",
		"Evidence: the table test's cases\n",
		"Sources: #566, CONTRIBUTING.md\n",
		"Also from: acceptance_criteria\n",
		"README.md:3\n",
		// A finding about the change as a whole is anchored to nothing.
		"· acceptance_criteria · scope\n\nThe change renames Gather",
		"[3 of 3 findings undecided]",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the console did not print %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "\nQ: ") {
		t.Errorf("an exchange was printed for a finding never asked about:\n%s", out)
	}
}

// brokenPipe is an output nothing can be written to.
type brokenPipe struct{}

func (brokenPipe) Write([]byte) (int, error) { return 0, errors.New("write: broken pipe") }

func TestTheConsoleStopsWhenItsOutputCannotBeWritten(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	c := &Console{In: strings.NewReader("s\ns\ns\n"), Out: brokenPipe{}}
	err := c.Run(context.Background(), q)
	if err == nil || !strings.Contains(err.Error(), "broken pipe") {
		t.Errorf("err = %v, want the write that failed", err)
	}
	// Nothing was asked of an input whose output went nowhere.
	if len(q.Pending()) != 3 {
		t.Errorf("%d findings pending, want every one: no key was taken", len(q.Pending()))
	}
}

func TestTheConsoleStopsWhenADecisionCannotBeWritten(t *testing.T) {
	for key, keys := range map[string]string{"select": "s\ns\n", "dismiss": "d\nnot now\ns\n", "defer": "f\ns\n"} {
		a := judged(t)
		q := queueOf(t, a)
		unwritable(q)
		var out bytes.Buffer
		err := (&Console{In: strings.NewReader(keys), Out: &out}).Run(context.Background(), q)
		if err == nil || !strings.Contains(err.Error(), BriefFile) {
			t.Errorf("%s: err = %v, want the write that failed", key, err)
		}
		// Triage stopped at the failure: the finding is not decided, the
		// next key was not read, and no summary claims otherwise.
		if got := order(out.String(), a.Findings.Items); len(got) != 1 {
			t.Errorf("%s: shown %v, want the one finding before the failure:\n%s", key, got, out.String())
		}
		if len(q.Pending()) != 3 || strings.Contains(out.String(), "undecided of 3 findings") {
			t.Errorf("%s: the console went on after a decision that was not written:\n%s", key, out.String())
		}
	}
	// An ask whose answer cannot be recorded stops triage the same way.
	a := judged(t)
	q := queueOf(t, a)
	q.Angles = &Angles{Agent: &fakeAgent{answer: "Yes.", id: "sess-tests"}, Provider: config.AgentClaude}
	unwritable(q)
	var out bytes.Buffer
	err := (&Console{In: strings.NewReader("a\nSure?\ns\n"), Out: &out}).Run(context.Background(), q)
	if err == nil || !strings.Contains(err.Error(), "ask failed") || !strings.Contains(err.Error(), BriefFile) {
		t.Errorf("err = %v, want the ask's write failure", err)
	}
	if len(q.Pending()) != 3 || strings.Contains(out.String(), "undecided of 3 findings") {
		t.Errorf("the console went on after an ask that was not written:\n%s", out.String())
	}
}

func TestChooseReadsTheEndOfTheReview(t *testing.T) {
	for input, want := range map[string]string{
		"a\n": OutputApprove, "c\n": OutputComment, "r\n": OutputReject, "o\n": OutputReport, "d\n": OutputDiscard,
		"approve\n": OutputApprove, "REPORT\n": OutputReport, "discard\n": OutputDiscard,
		// A key that is none of them is asked again; an input that ends
		// discards.
		"x\nr\n": OutputReject, "": OutputDiscard, "x\n": OutputDiscard,
	} {
		a := judged(t)
		q := queueOf(t, a)
		if err := q.Select(a.Findings.Items[0].ID, ""); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		c := &Console{In: strings.NewReader(input), Out: &out}
		if got := c.Choose(q); got != want {
			t.Errorf("%q chose %q, want %q", input, got, want)
		}
		if !strings.HasPrefix(out.String(), "\n1 finding selected. a approve and comment · c comment only · r reject and comment · o output the report · d discard\n> ") {
			t.Errorf("%q: the prompt was:\n%s", input, out.String())
		}
		if strings.HasPrefix(input, "x") && !strings.Contains(out.String(), `"x" is not a key. a approve`) {
			t.Errorf("an unknown key was not asked again:\n%s", out.String())
		}
		if want == OutputDiscard && input != "d\n" && input != "discard\n" && !strings.Contains(out.String(), "discarded: nothing posted\n") {
			t.Errorf("an ended input did not say it discarded:\n%s", out.String())
		}
	}
}

func TestChooseReadsFromWhereTriageStopped(t *testing.T) {
	// One reader over the input: what Run read ahead is what Choose reads.
	a := judged(t)
	q := queueOf(t, a)
	var out bytes.Buffer
	c := &Console{In: strings.NewReader("s\nq\no\n"), Out: &out}
	if err := c.Run(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if got := c.Choose(q); got != OutputReport {
		t.Errorf("chose %q, want the key after quit", got)
	}
	if !strings.Contains(out.String(), "1 selected, 0 dismissed, 0 deferred, 2 undecided of 3 findings\n\n1 finding selected. ") {
		t.Errorf("the prompt does not follow the summary:\n%s", out.String())
	}
	// An output that cannot be written discards: nothing is posted on a
	// person's behalf without their say.
	c = &Console{In: strings.NewReader("a\n"), Out: brokenPipe{}}
	if got := c.Choose(q); got != OutputDiscard {
		t.Errorf("chose %q with no output, want discard", got)
	}
}
