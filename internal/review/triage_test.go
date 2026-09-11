package review

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// judged is a review as the judge left it, written into a directory of its
// own: three findings, most severe first, from two angles of which one has a
// session to reopen (test_coverage), one failed (side_effects) and one never
// ran (acceptance_criteria, whose finding is in the list all the same).
func judged(t *testing.T) *Artifact {
	t.Helper()
	items := Merge([]Finding{
		{Angle: AngleTests, SessionID: "sess-tests", Category: "missing test", Severity: SeverityHigh, File: "gather.go", Lines: LineRange{12, 14}, Side: SideNew,
			Title: "Gather has no test for a source that cannot read", Body: "nothing exercises Skipped", Suggestion: "func TestASourceThatCannotRead(t *testing.T) {"},
		{Angle: AngleTests, SessionID: "sess-tests", Category: "docs", Severity: SeverityMedium, File: "README.md", Lines: LineRange{3, 3}, Side: SideNew,
			Title: "The README still claims a source failure stops everything", Body: "the sentence the change made false is still there"},
		{Angle: AngleAcceptance, Category: "scope", Severity: SeverityLow,
			Title: "The change renames Gather, which the issue did not ask for", Body: "every caller moves for a rename nobody wanted"},
	}, nil)
	a := &Artifact{
		Dir:   filepath.Join(t.TempDir(), "review"),
		Brief: testBrief(),
		Runs: []AngleRun{
			{Angle: AngleTests, Provider: config.AgentClaude, Model: "opus", Dir: t.TempDir(), SessionID: "sess-tests", Answer: `{"findings": []}`},
			{Angle: AngleSideEffects, Provider: config.AgentClaude, Model: "opus", Dir: "/tmp/checkout", Error: "exit status 1"},
		},
		Findings: &Findings{Items: items},
	}
	if err := a.Write(); err != nil {
		t.Fatal(err)
	}
	return a
}

// queueOf is a queue over judged, with reviewer notes that do not exist yet
// at a path of their own and no agent.
func queueOf(t *testing.T, a *Artifact) *Queue {
	t.Helper()
	q, err := NewQueue(a, nil, &Notes{Path: filepath.Join(t.TempDir(), "reviewer-notes.md")}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return q
}

func ids(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.ID)
	}
	return out
}

// written is the triage state as the artifact directory holds it, and an
// empty one when nothing has written it yet.
func written(t *testing.T, q *Queue) *Triage {
	t.Helper()
	tr, err := ReadTriage(q.Artifact.Dir)
	if os.IsNotExist(err) {
		return &Triage{}
	}
	if err != nil {
		t.Fatal(err)
	}
	return tr
}

func TestAQueueNeedsAJudgedReview(t *testing.T) {
	a := judged(t)
	a.Findings = nil
	if _, err := NewQueue(a, nil, nil, nil); err == nil || !strings.Contains(err.Error(), "before the judge") {
		t.Errorf("a review with no findings opened a queue: %v", err)
	}
	if _, err := NewQueue(nil, nil, nil, nil); err == nil {
		t.Error("no review opened a queue")
	}
	// A judged review never triaged has everything pending and an empty
	// triage state to fill in.
	a = judged(t)
	a.Triage = nil
	q, err := NewQueue(a, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(q.Pending()); !reflect.DeepEqual(got, ids(a.Findings.Items)) {
		t.Errorf("pending %v, want every finding %v", got, ids(a.Findings.Items))
	}
	if q.Artifact.Triage == nil || len(q.Artifact.Triage.Decisions) != 0 {
		t.Errorf("triage state %+v, want empty", q.Artifact.Triage)
	}
}

func TestSelectingAFindingRecordsOnlyAnEditedComment(t *testing.T) {
	q := queueOf(t, judged(t))
	first, second := q.Findings()[0], q.Findings()[1]
	if err := q.Select(first.ID, ""); err != nil {
		t.Fatal(err)
	}
	// Selected as written: the comment is the finding's own and nothing is
	// recorded for it, not even the same text.
	if err := q.Select(second.ID, "  "+second.Comment()+"\n"); err != nil {
		t.Fatal(err)
	}
	want := []Decision{{Finding: first.ID, Action: ActionSelect}, {Finding: second.ID, Action: ActionSelect}}
	if got := written(t, q).Decisions; !reflect.DeepEqual(got, want) {
		t.Errorf("triage.json holds %+v, want %+v", got, want)
	}
	selected := q.Selected()
	if len(selected) != 2 || selected[0].Comment != first.Title+"\n\n"+first.Body || selected[1].Comment != second.Comment() {
		t.Errorf("selected %+v, want both findings with their own text", selected)
	}
	if got := ids(q.Pending()); len(got) != 1 || got[0] != q.Findings()[2].ID {
		t.Errorf("pending %v, want the one finding not selected", got)
	}
	// Edited: the text is recorded and is what the selection carries.
	if err := q.Select(first.ID, "Say it shorter.\n"); err != nil {
		t.Fatal(err)
	}
	d, ok := q.DecisionOn(first.ID)
	if !ok || d.Comment != "Say it shorter." {
		t.Errorf("decision %+v, want the edited text", d)
	}
	if got := q.Selected()[0].Comment; got != "Say it shorter." {
		t.Errorf("selection carries %q, want the edited text", got)
	}
	if err := q.Select("00000000", ""); err == nil || !strings.Contains(err.Error(), "no finding 00000000") {
		t.Errorf("an unknown id was selected: %v", err)
	}
}

func TestADismissalIsAppendedToTheNotesBeforeItIsRecorded(t *testing.T) {
	q := queueOf(t, judged(t))
	f := q.Findings()[0]
	if err := q.Dismiss(f.ID, " it is covered by the table test "); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(q.Notes.Path)
	if err != nil {
		t.Fatal(err)
	}
	line := "- [acme/widgets] [test_coverage] [missing test] it is covered by the table test\n"
	if !strings.Contains(string(body), line) {
		t.Errorf("the notes do not hold the dismissal:\n%s", body)
	}
	if n := len(q.Notes.Dismissals); n != 1 || q.Notes.Dismissals[0].Reason != "it is covered by the table test" {
		t.Errorf("the notes in memory hold %d dismissals, want the one just made", n)
	}
	want := []Decision{{Finding: f.ID, Action: ActionDismiss, Reason: "it is covered by the table test"}}
	if got := written(t, q).Decisions; !reflect.DeepEqual(got, want) {
		t.Errorf("triage.json holds %+v, want %+v", got, want)
	}
	if got := ids(q.Pending()); len(got) != 2 || got[0] == f.ID {
		t.Errorf("pending %v, want the finding gone", got)
	}
	if len(q.Selected()) != 0 {
		t.Error("a dismissed finding is selected")
	}
}

func TestADismissalWithoutAReasonOrNotesIsRefused(t *testing.T) {
	q := queueOf(t, judged(t))
	f := q.Findings()[0]
	if err := q.Dismiss(f.ID, " \n"); err == nil || !strings.Contains(err.Error(), "needs a reason") {
		t.Errorf("a dismissal with no reason went through: %v", err)
	}
	if _, err := os.Stat(q.Notes.Path); !os.IsNotExist(err) {
		t.Errorf("the notes were written for a refused dismissal: %v", err)
	}
	if got := written(t, q).Decisions; len(got) != 0 {
		t.Errorf("triage.json holds %+v, want nothing", got)
	}
	// Notes that cannot be written: under a path that is a file.
	q.Notes.Path = filepath.Join(q.Artifact.Dir, BriefFile, "reviewer-notes.md")
	if err := q.Dismiss(f.ID, "fine"); err == nil {
		t.Error("a dismissal whose notes could not be written went through")
	}
	q.Notes = nil
	if err := q.Dismiss(f.ID, "fine"); err == nil || !strings.Contains(err.Error(), "no reviewer notes") {
		t.Errorf("a dismissal with nowhere to record it went through: %v", err)
	}
	if len(q.Pending()) != 3 {
		t.Error("a refused dismissal decided the finding")
	}
	if got := written(t, q).Decisions; len(got) != 0 {
		t.Errorf("triage.json holds %+v, want nothing", got)
	}
}

func TestDeferringAFindingRecordsNothingInTheNotes(t *testing.T) {
	q := queueOf(t, judged(t))
	f := q.Findings()[0]
	if err := q.Defer(f.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(q.Notes.Path); !os.IsNotExist(err) {
		t.Errorf("a deferral wrote the notes: %v", err)
	}
	want := []Decision{{Finding: f.ID, Action: ActionDefer}}
	if got := written(t, q).Decisions; !reflect.DeepEqual(got, want) {
		t.Errorf("triage.json holds %+v, want %+v", got, want)
	}
	if len(q.Selected()) != 0 || len(q.Pending()) != 2 {
		t.Errorf("a deferred finding is selected (%d) or pending (%d)", len(q.Selected()), len(q.Pending()))
	}
	if err := q.Defer("00000000"); err == nil {
		t.Error("an unknown id was deferred")
	}
}

func TestTheLatestDecisionOnAFindingIsTheOneInForce(t *testing.T) {
	q := queueOf(t, judged(t))
	f := q.Findings()[0]
	if err := q.Defer(f.ID); err != nil {
		t.Fatal(err)
	}
	if err := q.Select(f.ID, ""); err != nil {
		t.Fatal(err)
	}
	if d, ok := q.DecisionOn(f.ID); !ok || d.Action != ActionSelect {
		t.Errorf("decision in force %+v, want the selection that came after the deferral", d)
	}
	if got := q.Selected(); len(got) != 1 || got[0].Finding.ID != f.ID {
		t.Errorf("selected %+v, want the finding", got)
	}
	// Both are kept: the state is a log.
	if got := written(t, q).Decisions; len(got) != 2 || got[0].Action != ActionDefer || got[1].Action != ActionSelect {
		t.Errorf("triage.json holds %+v, want the deferral and the selection in order", got)
	}
	if got := q.Asked(f.ID); len(got) != 0 {
		t.Errorf("asked %+v, want nothing: neither decision was a question", got)
	}
}

func TestTriageIsPickedUpWhereItStopped(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	if err := q.Select(a.Findings.Items[0].ID, "shorter"); err != nil {
		t.Fatal(err)
	}
	if err := q.Dismiss(a.Findings.Items[1].ID, "not this review"); err != nil {
		t.Fatal(err)
	}
	// Another process: the artifact read back from disk alone.
	again, err := ReadArtifact(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	q2, err := NewQueue(again, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(q2.Pending()); !reflect.DeepEqual(got, []string{a.Findings.Items[2].ID}) {
		t.Errorf("pending after reopening %v, want the one undecided finding", got)
	}
	if got := q2.Selected(); len(got) != 1 || got[0].Comment != "shorter" {
		t.Errorf("selected after reopening %+v, want the edited selection", got)
	}
}

// askingQueue is a queue over judged whose test_coverage angle answers with
// answer when reopened.
func askingQueue(t *testing.T, a *Artifact, agent Agent) *Queue {
	t.Helper()
	q := queueOf(t, a)
	q.Angles = &Angles{Agent: agent, Provider: config.AgentClaude, Model: "opus"}
	return q
}

func TestAnAskReopensTheAnglesSessionAndSettlesNothing(t *testing.T) {
	a := judged(t)
	agent := &fakeAgent{answer: "  It is covered by nothing: the table test never reaches Skipped.\n", id: "sess-tests"}
	q := askingQueue(t, a, agent)
	f := a.Findings.Items[0]
	got, err := q.Ask(context.Background(), f.ID, " Is the table test not enough? ")
	if err != nil {
		t.Fatal(err)
	}
	// The angle's own session, where it ran, asked the question.
	run := a.Runs[0]
	if agent.req.ResumeID != run.SessionID || agent.req.Dir != run.Dir || agent.req.Name != AngleTests {
		t.Errorf("asked %+v, want the %s session %s resumed in %s", agent.req, AngleTests, run.SessionID, run.Dir)
	}
	if !strings.Contains(agent.req.Prompt, "Is the table test not enough?") {
		t.Errorf("the question is not in the prompt:\n%s", agent.req.Prompt)
	}
	if got.Text != "It is covered by nothing: the table test never reaches Skipped." || len(got.Added) != 0 {
		t.Errorf("answer %+v, want the text alone", got)
	}
	// Recorded beside the finding, which is still to be decided.
	want := []Decision{{Finding: f.ID, Action: ActionAsk, Question: "Is the table test not enough?", Answer: got.Text}}
	if decisions := written(t, q).Decisions; !reflect.DeepEqual(decisions, want) {
		t.Errorf("triage.json holds %+v, want %+v", decisions, want)
	}
	if !reflect.DeepEqual(q.Asked(f.ID), want) {
		t.Errorf("asked %+v, want %+v", q.Asked(f.ID), want)
	}
	if _, ok := q.DecisionOn(f.ID); ok {
		t.Error("an ask decided the finding")
	}
	if got := ids(q.Pending()); len(got) != 3 || got[0] != f.ID {
		t.Errorf("pending %v, want the finding still first", got)
	}
	// The session id the angle answered under is the one it had, so the
	// run is as it was.
	runs, err := ReadAngleRuns(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].SessionID != "sess-tests" {
		t.Errorf("the run's session is %s, want the one it had", runs[0].SessionID)
	}
}

const (
	// rawFromAsk is a finding the review did not have.
	rawFromAsk = `{"category": "error handling", "severity": "high", "file": "sources.go", "lines": [40, 41], "side": "new", "title": "Collect swallows the read error", "body": "the error is assigned and never returned"}`
	// rawAlreadyThere is the fixture's first finding said again.
	rawAlreadyThere = `{"category": "missing test", "severity": "medium", "file": "gather.go", "lines": [13, 13], "side": "new", "title": "Gather has no test for a source that cannot read", "body": "still nothing exercises Skipped"}`
	// rawDismissedBefore is what a rule in the reviewer notes drops.
	rawDismissedBefore = `{"category": "naming", "severity": "low", "file": "widget.go", "lines": [1, 1], "side": "new", "title": "Receiver names here are short", "body": "one letter"}`
	// rawPinnedOff is in a category the project turned off.
	rawPinnedOff = `{"category": "scope", "severity": "low", "title": "The change also touches the Makefile", "body": "the issue did not ask for it"}`
)

func TestAnAskAddsToTheQueueTheFindingsTheAnswerTurnsUp(t *testing.T) {
	a := judged(t)
	answer := "Reading sources.go again, there is one more.\n\n```json\n" + sessionAnswer(rawFromAsk, rawAlreadyThere, rawDismissedBefore, rawPinnedOff) + "\n```\n"
	q := askingQueue(t, a, &fakeAgent{answer: answer, id: "sess-tests"})
	q.Project = projectWith(t, "[categories]\nscope = \"off\"\n")
	q.Notes.Rules = []Rule{{Repo: testRepo, Angle: AngleTests, Category: "naming", Action: RuleDrop, Text: "receiver names are short here"}}
	f := a.Findings.Items[0]
	before := ids(a.Findings.Items)
	got, err := q.Ask(context.Background(), f.ID, "Anything else in sources.go?")
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "Reading sources.go again, there is one more." {
		t.Errorf("answer text %q, want the prose without the findings", got.Text)
	}
	// One finding is new: the one already in the list is not added again,
	// the one the notes drop is silenced, and the one the project pinned
	// off is dropped.
	if len(got.Added) != 1 || got.Added[0].Title != "Collect swallows the read error" {
		t.Fatalf("added %+v, want the one finding the review did not have", got.Added)
	}
	added := got.Added[0]
	if added.Angle != AngleTests || added.SessionID != "sess-tests" || added.Severity != SeverityHigh || added.ID == "" {
		t.Errorf("added %+v, want it marked as the angle's, from its session, with an id", added)
	}
	// In the list where its severity puts it: after the high finding in
	// gather.go, before the medium one.
	items := q.Findings()
	if want := []string{before[0], added.ID, before[1], before[2]}; !reflect.DeepEqual(ids(items), want) {
		t.Errorf("findings %v, want %v", ids(items), want)
	}
	if silenced := a.Findings.Silenced; len(silenced) != 1 || silenced[0].Title != "Receiver names here are short" || silenced[0].Action != RuleDrop {
		t.Errorf("silenced %+v, want the finding the rule dropped", silenced)
	}
	// Persisted: the findings file and the decision naming what it added.
	read, err := ReadFindings(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read, a.Findings) {
		t.Errorf("findings.json holds\n%+v\nwant\n%+v", read, a.Findings)
	}
	decisions := written(t, q).Decisions
	if len(decisions) != 1 || !reflect.DeepEqual(decisions[0].Added, []string{added.ID}) || decisions[0].Answer != got.Text {
		t.Errorf("triage.json holds %+v, want the ask naming the finding it added", decisions)
	}
	if got := ids(q.Pending()); len(got) != 4 || got[1] != added.ID {
		t.Errorf("pending %v, want the added finding among them", got)
	}
}

func TestAnAnswerThatIsProseAloneAddsNothing(t *testing.T) {
	for _, answer := range []string{
		"No, the table test covers it: see line {40}.",
		"```json\n{\"findings\": []}\n```",
		"{\"summary\": \"not a findings list\"}",
	} {
		a := judged(t)
		q := askingQueue(t, a, &fakeAgent{answer: answer, id: "sess-tests"})
		got, err := q.Ask(context.Background(), a.Findings.Items[0].ID, "Sure?")
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Added) != 0 || len(q.Findings()) != 3 || got.Text != answer {
			t.Errorf("answer %q added %+v and left %d findings, want nothing added and the text as said", answer, got.Added, len(q.Findings()))
		}
	}
}

func TestAReopenedSessionIsResumedUnderTheIdItAnsweredWith(t *testing.T) {
	a := judged(t)
	agent := &fakeAgent{answer: "Yes.", id: "sess-tests-2"}
	q := askingQueue(t, a, agent)
	f := a.Findings.Items[0]
	if _, err := q.Ask(context.Background(), f.ID, "Sure?"); err != nil {
		t.Fatal(err)
	}
	runs, err := ReadAngleRuns(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].SessionID != "sess-tests-2" {
		t.Errorf("the run's session is %s, want the id the reopened session answered with", runs[0].SessionID)
	}
	if _, err := q.Ask(context.Background(), f.ID, "Really?"); err != nil {
		t.Fatal(err)
	}
	if agent.req.ResumeID != "sess-tests-2" {
		t.Errorf("the second ask resumed %s, want the id the first answered with", agent.req.ResumeID)
	}
}

func TestAnAskWithNothingToReopenIsRefusedAndRecordsNothing(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	tests, scope := a.Findings.Items[0].ID, a.Findings.Items[2].ID
	if _, err := q.Ask(context.Background(), tests, "Sure?"); err == nil || !strings.Contains(err.Error(), "no agent") {
		t.Errorf("a queue without an agent asked: %v", err)
	}
	q.Angles = &Angles{Agent: &fakeAgent{answer: "Yes."}, Provider: config.AgentClaude}
	if _, err := q.Ask(context.Background(), scope, "Sure?"); err == nil || !strings.Contains(err.Error(), "acceptance_criteria angle has no session") {
		t.Errorf("a finding whose angle never ran was asked about: %v", err)
	}
	if _, err := q.Ask(context.Background(), tests, " "); err == nil || !strings.Contains(err.Error(), "nothing to ask") {
		t.Errorf("an empty question was asked: %v", err)
	}
	q.Angles.Agent = &fakeAgent{err: errors.New("no capacity")}
	if _, err := q.Ask(context.Background(), tests, "Sure?"); err == nil || !strings.Contains(err.Error(), "no capacity") {
		t.Errorf("a session that failed answered: %v", err)
	}
	if _, err := q.Ask(context.Background(), "00000000", "Sure?"); err == nil {
		t.Error("an unknown id was asked about")
	}
	if got := written(t, q).Decisions; len(got) != 0 {
		t.Errorf("triage.json holds %+v, want nothing for asks that failed", got)
	}
}

func TestAFindingsCommentIsItsTitleThenItsBody(t *testing.T) {
	f := Finding{Title: "one line", Body: "what is wrong"}
	if got := f.Comment(); got != "one line\n\nwhat is wrong" {
		t.Errorf("comment %q", got)
	}
	f.Body = ""
	if got := f.Comment(); got != "one line" {
		t.Errorf("comment of a finding with no body %q", got)
	}
}

// unwritable moves the queue's artifact to a path nothing can be written
// under: a directory inside a file the fixture really wrote.
func unwritable(q *Queue) {
	q.Artifact.Dir = filepath.Join(q.Artifact.Dir, BriefFile, "review")
}

func isRefusal(err error) bool {
	var r *Refusal
	return errors.As(err, &r)
}

func TestADecisionThatCannotBeWrittenIsNotKept(t *testing.T) {
	a := judged(t)
	q := askingQueue(t, a, &fakeAgent{answer: "Reading sources.go again, there is one more.\n\n```json\n" + sessionAnswer(rawFromAsk) + "\n```\n", id: "sess-tests-2"})
	unwritable(q)
	f := a.Findings.Items[0]
	for name, act := range map[string]func() error{
		"select":  func() error { return q.Select(f.ID, "shorter") },
		"dismiss": func() error { return q.Dismiss(f.ID, "not this review") },
		"defer":   func() error { return q.Defer(f.ID) },
		"ask":     func() error { _, err := q.Ask(context.Background(), f.ID, "Anything else?"); return err },
	} {
		err := act()
		if err == nil || isRefusal(err) {
			t.Errorf("%s under an unwritable artifact: err = %v, want a failure that is not a refusal", name, err)
		}
	}
	// Nothing an action would have recorded is in memory: no decision, no
	// answer, no finding the answer added, no new session id.
	if got := q.Artifact.Triage.Decisions; len(got) != 0 {
		t.Errorf("decisions %+v, want none kept", got)
	}
	if len(q.Pending()) != 3 || len(q.Findings()) != 3 || len(q.Asked(f.ID)) != 0 {
		t.Errorf("pending %d, findings %d, asked %d: want the queue as it was", len(q.Pending()), len(q.Findings()), len(q.Asked(f.ID)))
	}
	if a.Runs[0].SessionID != "sess-tests" {
		t.Errorf("the run's session is %s, want the one it had", a.Runs[0].SessionID)
	}
	// The one thing that was written before the failure: the dismissal's
	// line in the notes, which is the notes' record and not the review's.
	if _, err := os.Stat(q.Notes.Path); err != nil {
		t.Errorf("the dismissal's line was not appended to the notes: %v", err)
	}
}

func TestAnAskWhoseFindingsCannotBeWrittenAddsNothing(t *testing.T) {
	// The run file is written before the findings, and the findings before
	// the decision: a findings write that fails leaves the list and the
	// decisions as they were, whatever the run file now says.
	a := judged(t)
	q := askingQueue(t, a, &fakeAgent{answer: "One more.\n\n```json\n" + sessionAnswer(rawFromAsk) + "\n```\n", id: "sess-tests-2"})
	if err := os.Remove(filepath.Join(a.Dir, FindingsFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(a.Dir, FindingsFile), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := q.Ask(context.Background(), a.Findings.Items[0].ID, "Sure?")
	if err == nil || isRefusal(err) || !strings.Contains(err.Error(), FindingsFile) {
		t.Fatalf("err = %v, want the findings write that failed", err)
	}
	if len(q.Findings()) != 3 || len(q.Artifact.Findings.Silenced) != 0 || len(q.Artifact.Triage.Decisions) != 0 {
		t.Errorf("findings %d, silenced %d, decisions %d: want the queue as it was", len(q.Findings()), len(q.Artifact.Findings.Silenced), len(q.Artifact.Triage.Decisions))
	}
	// The session did hear the question, so the run file names the
	// session it answered as: the next ask resumes the one that knows.
	runs, err := ReadAngleRuns(a.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if runs[0].SessionID != "sess-tests-2" || a.Runs[0].SessionID != "sess-tests-2" {
		t.Errorf("run session %s (file %s), want the id the session answered with", a.Runs[0].SessionID, runs[0].SessionID)
	}
}

func TestARefusalIsToldApartFromAFailure(t *testing.T) {
	a := judged(t)
	q := queueOf(t, a)
	tests, scope := a.Findings.Items[0].ID, a.Findings.Items[2].ID
	for name, err := range map[string]error{
		"an unknown finding":         q.Defer("00000000"),
		"a dismissal with no reason": q.Dismiss(tests, ""),
		"a dismissal with no notes": func() error {
			n := q.Notes
			q.Notes = nil
			defer func() { q.Notes = n }()
			return q.Dismiss(tests, "fine")
		}(),
		"an empty question":     func() error { _, err := q.Ask(context.Background(), tests, ""); return err }(),
		"a queue with no agent": func() error { _, err := q.Ask(context.Background(), tests, "Sure?"); return err }(),
	} {
		if !isRefusal(err) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
	cause := errors.New("no capacity")
	q.Angles = &Angles{Agent: &fakeAgent{err: cause}, Provider: config.AgentClaude}
	for name, id := range map[string]string{"an angle with no session": scope, "a session that failed": tests} {
		_, err := q.Ask(context.Background(), id, "Sure?")
		if !isRefusal(err) {
			t.Errorf("%s: err = %v, want a refusal", name, err)
		}
	}
	// Refusals leave the queue as it was, and a refusal made of a session's
	// failure reads as that failure and unwraps to it.
	if _, err := q.Ask(context.Background(), tests, "Sure?"); err == nil || err.Error() != "no capacity" || !errors.Is(err, cause) {
		t.Errorf("err = %v, want the session's own failure wrapped", err)
	}
	if got := written(t, q).Decisions; len(got) != 0 || len(q.Pending()) != 3 {
		t.Errorf("a refusal decided something: %+v", got)
	}
}
