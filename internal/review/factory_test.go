package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// triageAgent answers the triage sessions in turn, one answer per round,
// and once they run out a triage answer that takes nothing, or fail from
// then on when fail is set. Every other session, an angle an ask reopened,
// it answers with angle.
type triageAgent struct {
	rounds []string
	angle  string
	fail   error
	reqs   []AgentRequest
}

func (f *triageAgent) Run(_ context.Context, req AgentRequest) (*AgentResult, error) {
	f.reqs = append(f.reqs, req)
	if req.Name != TriageName {
		return &AgentResult{ID: req.ResumeID, Text: f.angle}, nil
	}
	n := len(f.prompts())
	if n > len(f.rounds) {
		if f.fail != nil {
			return nil, f.fail
		}
		return &AgentResult{ID: "sess-triage", Text: `{"decisions": []}`}, nil
	}
	return &AgentResult{ID: "sess-triage", Text: f.rounds[n-1]}, nil
}

// prompts are what the triage sessions were told, round by round.
func (f *triageAgent) prompts() []string {
	var out []string
	for _, r := range f.reqs {
		if r.Name == TriageName {
			out = append(out, r.Prompt)
		}
	}
	return out
}

// triageRun runs tr over q and returns the end and what it logged.
func triageRun(t *testing.T, q *Queue, tr *AgentTriage) (string, string) {
	t.Helper()
	var log bytes.Buffer
	tr.Log = &log
	end, err := tr.Run(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return end, log.String()
}

func TestAnAgentTriagesThroughTheQueue(t *testing.T) {
	a := judged(t)
	f := a.Findings.Items
	agent := &triageAgent{rounds: []string{fmt.Sprintf("Here is my triage:\n\n```json\n"+`{"decisions": [
		{"finding": " %s ", "action": " Select ", "comment": "Gather needs a test for Skipped"},
		{"finding": "%s", "action": "dismiss", "reason": " the README is rewritten in #574 "},
		{"finding": "%s", "action": "defer"}
	], "end": " Comment "}`+"\n```\n", f[0].ID, f[1].ID, f[2].ID)}}
	q := askingQueue(t, a, agent)
	checkout := t.TempDir()
	end, log := triageRun(t, q, &AgentTriage{Agent: agent, Dir: checkout, Instructions: "  Hold the change to its issue.  "})
	if end != OutputComment {
		t.Errorf("end = %q, want the agent's comment", end)
	}
	// One session decided everything: nothing is left to ask it about.
	if len(agent.reqs) != 1 {
		t.Fatalf("%d sessions, want the one triage session", len(agent.reqs))
	}
	if req := agent.reqs[0]; req.Name != TriageName || req.Dir != checkout || req.ResumeID != "" {
		t.Errorf("ran %+v, want a new %s session in the checkout", req, TriageName)
	}
	// The decisions are the queue's, written as the console's would be.
	want := []Decision{
		{Finding: f[0].ID, Action: ActionSelect, Comment: "Gather needs a test for Skipped"},
		{Finding: f[1].ID, Action: ActionDismiss, Reason: "the README is rewritten in #574"},
		{Finding: f[2].ID, Action: ActionDefer},
	}
	if got := written(t, q).Decisions; !reflect.DeepEqual(got, want) {
		t.Errorf("triage.json holds %+v, want %+v", got, want)
	}
	if got := q.Selected(); len(got) != 1 || got[0].Comment != "Gather needs a test for Skipped" {
		t.Errorf("selected %+v, want the edited comment", got)
	}
	if notes := readFile(t, q.Notes.Path); !strings.Contains(notes, "- [acme/widgets] [test_coverage] [docs] the README is rewritten in #574\n") {
		t.Errorf("the dismissal is not in the reviewer notes:\n%s", notes)
	}
	prompt := agent.reqs[0].Prompt
	if !strings.HasPrefix(prompt, triageFrame) {
		t.Errorf("the prompt does not start with the frame:\n%s", prompt)
	}
	wants := []string{
		"## Your instructions\n\nWhoever runs this review wants this from its triage.",
		" in how you end the review.\n\nHold the change to its issue.\n",
		"## How the review ends\n\nOnce triage is over, the review ends one of five ways, and you choose which with `end`:\n\n",
		"Approve only when your instructions allow it",
		a.Brief.Text(),
		"## Undecided findings\n",
		"gather.go:12-14\n\nGather has no test for a source that cannot read\n\nnothing exercises Skipped\n\nSuggestion:\n  func TestASourceThatCannotRead",
		"\n---\n\nTriage the review of acme/widgets#7. Answer with the JSON object alone.\n",
	}
	for _, e := range ends {
		wants = append(wants, "- `"+e+"`: "+endEffects[e]+"\n")
	}
	for _, g := range f {
		wants = append(wants, "\n### "+g.ID+" · "+g.Severity+" · "+g.Angle+" · "+g.Category+"\n")
	}
	for _, w := range wants {
		if !strings.Contains(prompt, w) {
			t.Errorf("the prompt lacks %q:\n%s", w, prompt)
		}
	}
	for _, absent := range []string{"## Decided", "## What your last answer", "## Dismissed before"} {
		if strings.Contains(prompt, absent) {
			t.Errorf("the prompt has %q, and nothing called for it:\n%s", absent, prompt)
		}
	}
	for _, w := range []string{
		"triage round 1: 3 findings undecided\n",
		"  select " + f[0].ID + ": " + f[0].Title + "\n",
		"  dismiss " + f[1].ID + ": the README is rewritten in #574\n",
		"  defer " + f[2].ID + ": " + f[2].Title + "\n",
		"1 selected, 1 dismissed, 1 deferred, 0 undecided of 3 findings\n",
		"the agent chose to end the review: comment\n",
	} {
		if !strings.Contains(log, w) {
			t.Errorf("the log lacks %q:\n%s", w, log)
		}
	}
}

func TestAnAgentsAskIsAnsweredInItsNextRound(t *testing.T) {
	a := judged(t)
	f := a.Findings.Items
	agent := &triageAgent{
		angle: "It is covered by nothing: the table test never reaches Skipped.\n\n```json\n" + sessionAnswer(rawFromAsk) + "\n```\n",
		rounds: []string{
			fmt.Sprintf(`{"decisions": [{"finding": "%s", "action": "ask", "question": " Is the table test not enough? "}]}`, f[0].ID),
			fmt.Sprintf(`{"decisions": [{"finding": "%s", "action": "select"}], "end": "report"}`, f[0].ID),
		},
	}
	q := askingQueue(t, a, agent)
	end, log := triageRun(t, q, &AgentTriage{Agent: agent, Dir: t.TempDir()})
	// The angle's own session was reopened with the question.
	if len(agent.reqs) < 2 || agent.reqs[1].Name != AngleTests || agent.reqs[1].ResumeID != "sess-tests" || !strings.Contains(agent.reqs[1].Prompt, "Is the table test not enough?") {
		t.Fatalf("sessions %+v, want the %s angle resumed with the question second", agent.reqs, AngleTests)
	}
	decisions := written(t, q).Decisions
	if len(decisions) != 2 || decisions[0].Action != ActionAsk || len(decisions[0].Added) != 1 {
		t.Fatalf("triage.json holds %+v, want the ask with its added finding, then the selection", decisions)
	}
	added := decisions[0].Added[0]
	// Round two reads the answer beside the finding and the finding it
	// added; round three reads what round two decided, took nothing, and
	// triage stopped with three findings undecided.
	prompts := agent.prompts()
	if len(prompts) != 3 {
		t.Fatalf("%d triage rounds, want 3", len(prompts))
	}
	if strings.Contains(prompts[0], "\nQ: ") {
		t.Errorf("round one was told of an ask before one was made:\n%s", prompts[0])
	}
	for _, w := range []string{
		"\nQ: Is the table test not enough?\nA: It is covered by nothing: the table test never reaches Skipped.\n",
		"\n### " + added + " · high · test_coverage · error handling\nsources.go:40-41\n\nCollect swallows the read error\n",
	} {
		if !strings.Contains(prompts[1], w) {
			t.Errorf("round two lacks %q:\n%s", w, prompts[1])
		}
	}
	if !strings.Contains(prompts[2], "\n## Decided\n\n- "+f[0].ID+" selected: "+f[0].Title+"\n") || strings.Contains(prompts[2], "### "+f[0].ID) {
		t.Errorf("round three does not have the selection as decided:\n%s", prompts[2])
	}
	if end != OutputReport {
		t.Errorf("end = %q, want the report round two chose", end)
	}
	for _, w := range []string{
		"  ask " + f[0].ID + ": Is the table test not enough?\n",
		"    It is covered by nothing: the table test never reaches Skipped.\n",
		"    added " + added + "  high  Collect swallows the read error\n",
		"triage round 2: 4 findings undecided\n",
		"triage round 3: 3 findings undecided\n",
		"1 selected, 0 dismissed, 0 deferred, 3 undecided of 4 findings\n",
	} {
		if !strings.Contains(log, w) {
			t.Errorf("the log lacks %q:\n%s", w, log)
		}
	}
}

func TestWhatAnAgentsAnswerCouldNotDoIsToldToTheNextRound(t *testing.T) {
	a := judged(t)
	f := a.Findings.Items
	agent := &triageAgent{rounds: []string{
		"I would select the first one.",
		`{"decisions": "all of them"}`,
		fmt.Sprintf(`{"decisions": [
			{"finding": "%s", "action": "dismiss"},
			{"finding": "nope0000", "action": "select"},
			{"finding": "%s", "action": "flag"},
			{"finding": "%s", "action": "ask", "question": "why?"}
		], "end": "merge"}`, f[0].ID, f[1].ID, f[2].ID),
		fmt.Sprintf(`{"decisions": [{"finding": "%s", "action": "select"}, {"finding": "%s", "action": "select"}, {"finding": "%s", "action": "select"}], "end": "reject"}`, f[0].ID, f[1].ID, f[2].ID),
	}}
	q := askingQueue(t, a, agent)
	end, log := triageRun(t, q, &AgentTriage{Agent: agent, Dir: t.TempDir()})
	prompts := agent.prompts()
	if len(prompts) != 4 {
		t.Fatalf("%d triage rounds, want 4", len(prompts))
	}
	const told = "## What your last answer could not do\n\n"
	if strings.Contains(prompts[0], told) {
		t.Errorf("round one was told of a round before it:\n%s", prompts[0])
	}
	if !strings.Contains(prompts[1], told+"- your answer had no JSON object in it: answer with the JSON object alone\n") {
		t.Errorf("round two was not told the answer had no JSON:\n%s", prompts[1])
	}
	if !strings.Contains(prompts[2], told+"- your answer's JSON object is not a triage answer: ") {
		t.Errorf("round three was not told the answer was no triage answer:\n%s", prompts[2])
	}
	for _, w := range []string{
		told + "- dismiss " + f[0].ID + ": a dismissal needs a reason: it is what your reviewer notes are made of\n",
		"- select nope0000: no finding nope0000 in this review\n",
		"- flag " + f[1].ID + ": \"flag\" is not an action: give one of select, dismiss, defer, ask\n",
		"- ask " + f[2].ID + ": the acceptance_criteria angle has no session in this review, so there is nothing to ask\n",
		"- end merge: not an end: give one of approve, comment, reject, report, discard\n",
	} {
		if !strings.Contains(prompts[3], w) {
			t.Errorf("round four lacks %q:\n%s", w, prompts[3])
		}
	}
	// Only the round before is told: round four's answer is not round
	// two's.
	if strings.Contains(prompts[3], "no JSON object") {
		t.Errorf("round four was told what round two could not do:\n%s", prompts[3])
	}
	if end != OutputReject {
		t.Errorf("end = %q, want the reject round four chose", end)
	}
	decisions := written(t, q).Decisions
	if len(decisions) != 3 || decisions[0].Action != ActionSelect || decisions[1].Action != ActionSelect || decisions[2].Action != ActionSelect {
		t.Errorf("triage.json holds %+v, want the three selections alone", decisions)
	}
	if _, err := os.Stat(q.Notes.Path); !os.IsNotExist(err) {
		t.Errorf("the refused dismissal reached the reviewer notes: %v", err)
	}
	if !strings.Contains(log, "  refused: dismiss "+f[0].ID+": a dismissal needs a reason") {
		t.Errorf("the log does not say what was refused:\n%s", log)
	}
}

func TestTheEndIsTheCommandsWhenItChoseOne(t *testing.T) {
	a := judged(t)
	f := a.Findings.Items
	agent := &triageAgent{rounds: []string{fmt.Sprintf(`{"decisions": [{"finding": "%s", "action": "defer"}, {"finding": "%s", "action": "defer"}, {"finding": "%s", "action": "defer"}], "end": "approve"}`, f[0].ID, f[1].ID, f[2].ID)}}
	q := askingQueue(t, a, agent)
	// Nothing selected for a comment-only review: the command refuses it
	// when it posts, as it does whoever triaged. It is not the agent's to
	// answer.
	end, log := triageRun(t, q, &AgentTriage{Agent: agent, Dir: t.TempDir(), Mode: OutputComment})
	if end != OutputComment {
		t.Errorf("end = %q, want the command's comment", end)
	}
	prompts := agent.prompts()
	if len(prompts) != 1 {
		t.Fatalf("%d triage rounds, want 1", len(prompts))
	}
	if !strings.Contains(prompts[0], "## How the review ends\n\nWhoever runs this review chose how it ends: `comment`, so the selected findings are posted as review comments, in a comment-only review. Leave `end` out of your answer.\n") {
		t.Errorf("the prompt does not say how the review ends:\n%s", prompts[0])
	}
	if strings.Contains(prompts[0], "- `approve`:") {
		t.Errorf("the agent was offered the ends to choose from:\n%s", prompts[0])
	}
	if strings.Contains(log, "the agent chose") || strings.Contains(log, "refused") {
		t.Errorf("the agent's end was read:\n%s", log)
	}
	// Nothing undecided and the end chosen: nobody is asked anything.
	end, _ = triageRun(t, q, &AgentTriage{Agent: agent, Dir: t.TempDir(), Mode: OutputReport})
	if end != OutputReport || len(agent.reqs) != 1 {
		t.Errorf("end = %q after %d sessions, want report and no new session", end, len(agent.reqs))
	}
}

func TestAnEndTheAgentCannotTakeIsRefusedUntilItCan(t *testing.T) {
	a := judged(t)
	f := a.Findings.Items
	agent := &triageAgent{rounds: []string{
		fmt.Sprintf(`{"decisions": [{"finding": "%s", "action": "select"}], "end": "comment"}`, f[0].ID),
		fmt.Sprintf(`{"decisions": [{"finding": "%s", "action": "dismiss", "reason": "covered by the table test"}, {"finding": "%s", "action": "defer"}, {"finding": "%s", "action": "defer"}]}`, f[0].ID, f[1].ID, f[2].ID),
		`{"decisions": [], "end": " Report "}`,
	}}
	q := askingQueue(t, a, agent)
	end, log := triageRun(t, q, &AgentTriage{Agent: agent, Dir: t.TempDir(), Mode: OutputAsk})
	prompts := agent.prompts()
	if len(prompts) != 3 {
		t.Fatalf("%d triage rounds, want 3", len(prompts))
	}
	// Round one's comment-only end held while a finding was selected;
	// round two dismissed it, which left the end with nothing to post.
	if strings.Contains(prompts[1], "## What your last answer") {
		t.Errorf("round two was told of a refusal:\n%s", prompts[1])
	}
	for _, w := range []string{
		"- end comment: nothing was selected, and a comment-only review with nothing in it cannot be posted\n",
		"\n## Decided\n\n- " + f[0].ID + " dismissed: " + f[0].Title + " (covered by the table test)\n",
		"## Undecided findings\n\nNothing is undecided.\n",
	} {
		if !strings.Contains(prompts[2], w) {
			t.Errorf("round three lacks %q:\n%s", w, prompts[2])
		}
	}
	if end != OutputReport {
		t.Errorf("end = %q, want the report round three chose", end)
	}
	if !strings.Contains(log, "  refused: end comment: nothing was selected") || !strings.Contains(log, "the agent chose to end the review: report\n") {
		t.Errorf("the log does not say what became of the end:\n%s", log)
	}
}

func TestAnAgentThatChoosesNoEndDiscards(t *testing.T) {
	a := judged(t)
	f := a.Findings.Items
	agent := &triageAgent{rounds: []string{fmt.Sprintf(`{"decisions": [{"finding": "%s", "action": "select"}, {"finding": "%s", "action": "defer"}, {"finding": "%s", "action": "defer"}]}`, f[0].ID, f[1].ID, f[2].ID)}}
	q := askingQueue(t, a, agent)
	end, log := triageRun(t, q, &AgentTriage{Agent: agent, Dir: t.TempDir()})
	// Everything decided and no end: one more round is asked for one,
	// takes nothing, and nothing is posted.
	if prompts := agent.prompts(); len(prompts) != 2 || !strings.Contains(prompts[1], "Nothing is undecided.") {
		t.Errorf("%d triage rounds, want a second asking for the end", len(prompts))
	}
	if end != OutputDiscard || !strings.Contains(log, "the agent chose no end it could take, so nothing is posted\n") {
		t.Errorf("end = %q, want discard, said so:\n%s", end, log)
	}
}

func TestAgentTriageStopsAfterItsRounds(t *testing.T) {
	a := judged(t)
	agent := &triageAgent{rounds: slices.Repeat([]string{"still thinking"}, 10)}
	q := askingQueue(t, a, agent)
	end, log := triageRun(t, q, &AgentTriage{Agent: agent, Dir: t.TempDir(), Rounds: 2})
	if n := len(agent.prompts()); n != 2 {
		t.Errorf("%d triage rounds, want 2", n)
	}
	if end != OutputDiscard || !strings.Contains(log, "0 selected, 0 dismissed, 0 deferred, 3 undecided of 3 findings\n") {
		t.Errorf("end = %q, want discard with everything undecided:\n%s", end, log)
	}
	agent.reqs = nil
	triageRun(t, q, &AgentTriage{Agent: agent, Dir: t.TempDir()})
	if n := len(agent.prompts()); n != DefaultTriageRounds {
		t.Errorf("%d triage rounds, want %d", n, DefaultTriageRounds)
	}
}

func TestAFailedTriageSessionStopsTriageWithWhatWasDecided(t *testing.T) {
	a := judged(t)
	f := a.Findings.Items
	agent := &triageAgent{
		rounds: []string{fmt.Sprintf(`{"decisions": [{"finding": "%s", "action": "select"}]}`, f[0].ID)},
		fail:   errors.New("triage session: rate limited"),
	}
	q := askingQueue(t, a, agent)
	_, err := (&AgentTriage{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), q)
	if err == nil || !strings.Contains(err.Error(), "rate limited") {
		t.Errorf("err = %v, want the session's failure", err)
	}
	if got := written(t, q).Decisions; len(got) != 1 || got[0].Finding != f[0].ID {
		t.Errorf("triage.json holds %+v, want round one's selection", got)
	}
}

func TestADecisionAnAgentTookThatCannotBeWrittenStopsTriage(t *testing.T) {
	a := judged(t)
	f := a.Findings.Items
	agent := &triageAgent{rounds: []string{fmt.Sprintf(`{"decisions": [{"finding": "%s", "action": "select"}]}`, f[0].ID)}}
	q := askingQueue(t, a, agent)
	unwritable(q)
	_, err := (&AgentTriage{Agent: agent, Dir: t.TempDir()}).Run(context.Background(), q)
	if err == nil || isRefusal(err) {
		t.Errorf("err = %v, want the write that failed", err)
	}
	if n := len(agent.prompts()); n != 1 {
		t.Errorf("%d triage rounds, want the one that stopped", n)
	}
	if len(q.Artifact.Triage.Decisions) != 0 {
		t.Errorf("kept %+v, which was never written", q.Artifact.Triage.Decisions)
	}
}

func TestAgentTriageRunsInTheScratchDirectoryWithoutACheckout(t *testing.T) {
	a := judged(t)
	agent := &triageAgent{}
	q := askingQueue(t, a, agent)
	triageRun(t, q, &AgentTriage{Agent: agent})
	want := filepath.Join(a.Dir, ScratchDir)
	if len(agent.reqs) != 1 || agent.reqs[0].Dir != want {
		t.Fatalf("ran %+v, want one session in %s", agent.reqs, want)
	}
	if info, err := os.Stat(want); err != nil || !info.IsDir() {
		t.Errorf("the scratch directory is not there: %v", err)
	}
	// One that cannot be made stops triage before any session runs.
	unwritable(q)
	agent.reqs = nil
	if _, err := (&AgentTriage{Agent: agent}).Run(context.Background(), q); err == nil || len(agent.reqs) != 0 {
		t.Errorf("err = %v after %d sessions, want the directory's error and none", err, len(agent.reqs))
	}
}

func TestAnAgentIsToldWhatTheReviewerDismissedInThisRepository(t *testing.T) {
	a := judged(t)
	agent := &triageAgent{}
	q := askingQueue(t, a, agent)
	q.Notes.Rules = []Rule{
		{Repo: testRepo, Angle: AngleStyle, Category: "naming", Action: RuleDrop, Text: " receiver names are short here "},
		{Repo: anyValue, Action: RuleDownrank, Text: "generated files are not reviewed"},
		{Repo: "other/repo", Angle: AngleStyle, Action: RuleDrop, Text: "not about this repository"},
		{Repo: testRepo, Category: "docs", Action: RuleDrop},
	}
	triageRun(t, q, &AgentTriage{Agent: agent, Dir: t.TempDir()})
	prompt := agent.reqs[0].Prompt
	want := "## Dismissed before\n\nThe reviewer of this repository has dismissed findings like these in earlier reviews, from the angle and in the category named:\n\n" +
		"- [style] [naming] receiver names are short here\n- [*] [*] generated files are not reviewed\n\n" +
		"Dismiss a finding that reads like one of them, unless this change makes it newly wrong.\n"
	if !strings.Contains(prompt, want) {
		t.Errorf("the prompt lacks the rules:\n%s", prompt)
	}
	if strings.Contains(prompt, "not about this repository") || strings.Contains(prompt, "[docs]") {
		t.Errorf("the prompt has a rule about another repository, or one that says nothing:\n%s", prompt)
	}
}

func TestNewAgentTriageRunsTheConfiguredAgent(t *testing.T) {
	cfg, err := ParseConfig("provider = \"codex\"\nmodel = \"gpt-5\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	tr := NewAgentTriage(cfg, "/checkout")
	agent, ok := tr.Agent.(*CLIAgent)
	if !ok || agent.Provider != config.AgentCodex || agent.Model != "gpt-5" || tr.Dir != "/checkout" || !tr.chooses() {
		t.Errorf("NewAgentTriage = %+v with agent %+v, want codex's gpt-5 in the checkout, choosing the end", tr, tr.Agent)
	}
}
