package review

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/internal/text"
)

// Factory mode: triage by an agent. AgentTriage drives the Queue the console
// drives (triage.go), with the same four actions and the same reviewer
// notes, from what an agent session answers instead of the keys a person
// presses. It is what `bees review --agent` and `bees review triage --agent`
// run, for a review nobody sits at a terminal for.
//
// The agent triages in rounds. Each round is one read-only session through
// Agent (agent.go), told everything in its prompt: the task's instructions,
// how the review ends, what the reviewer notes have dismissed before, the
// brief, every finding still undecided with what was asked about it, what is
// decided, and what its last answer could not do. It answers with its
// decisions, which are taken on the queue in the order given, each written
// into the artifact before the next. A round is a new session, never a
// resumed one, so a triage stopped partway is picked up from the artifact
// alone, and codex, which cannot resume, triages the way claude does.
//
// Another round follows one that asked an angle something (the answer is in
// the next prompt, beside the finding), had anything refused, or left
// findings undecided. Triage stops when nothing is undecided and the end is
// settled, when a round took nothing and had nothing refused, or after
// Rounds rounds. What is still undecided then stays in the queue, for
// `bees review triage` to offer.
//
// How the review ends (output.go) is the command's when the command line or
// the global configuration chose it. When they leave it to ask, the agent
// chooses, as its instructions say, where a person would have been asked.

// triageFrame is what every triage session is told first: what triage is,
// the four actions, and what to answer with.
//
//go:embed prompts/triage.md
var triageFrame string

// TriageName is the name of the triage session, in an error message.
const TriageName = "triage"

// DefaultTriageRounds is how many rounds an agent triages for at most.
const DefaultTriageRounds = 5

// ends are the output modes an agent can end a review with: every one but
// the one that asks.
var ends = []string{OutputApprove, OutputComment, OutputReject, OutputReport, OutputDiscard}

// endEffects say what each end does, as a triage session is told it.
var endEffects = map[string]string{
	OutputApprove: "the selected findings are posted as review comments, and the pull request is approved",
	OutputComment: "the selected findings are posted as review comments, in a comment-only review",
	OutputReject:  "the selected findings are posted as review comments, in a review that requests changes",
	OutputReport:  "the selected findings are printed as a markdown report, and nothing is posted",
	OutputDiscard: "nothing is posted and nothing is printed",
}

// decided names the decisions in force in the past tense.
var decided = map[string]string{
	ActionSelect:  "selected",
	ActionDismiss: "dismissed",
	ActionDefer:   "deferred",
}

// AgentTriage triages a review's queue with an agent.
type AgentTriage struct {
	// Agent runs the sessions.
	Agent Agent
	// Dir is the checkout of the repository under review, which the
	// sessions' read-only tools can reach. It is "" on a machine that has
	// none, and the sessions then run in the artifact's scratch directory,
	// as the angles do.
	Dir string
	// Instructions are the task's own: what whoever runs the review wants
	// from its triage, in their words. Every session is told them.
	Instructions string
	// Mode is how the review ends when the command chose it, one of the
	// five ends, and OutputAsk or "" for the agent to choose.
	Mode string
	// Rounds is how many rounds triage runs at most, DefaultTriageRounds
	// when it is zero.
	Rounds int
	// Log is where each round and each action taken is written, one line
	// each, and nowhere when nil.
	Log io.Writer
}

// NewAgentTriage is the agent triage of a review, as the global
// configuration says: cfg's provider and model, and dir the checkout.
func NewAgentTriage(cfg *Config, dir string) *AgentTriage {
	return &AgentTriage{Agent: NewAgent(cfg), Dir: dir}
}

// Run triages q and returns how the review ends: Mode when the command
// chose it, otherwise the end the agent chose, and OutputDiscard when the
// agent chose none it could take, since nothing is posted without
// somebody's say. An error is one that stopped triage: a session that could
// not run or failed, or a decision that could not be written. The decisions
// taken before it are in the artifact, and the one it stopped on is not.
func (t *AgentTriage) Run(ctx context.Context, q *Queue) (string, error) {
	dir := t.Dir
	if dir == "" {
		dir = filepath.Join(q.Artifact.Dir, ScratchDir)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
	}
	rounds := t.Rounds
	if rounds <= 0 {
		rounds = DefaultTriageRounds
	}
	choosing := t.chooses()
	end := ""
	var refused []string
	for round := 1; round <= rounds; round++ {
		if len(q.Pending()) == 0 && (!choosing || end != "") {
			break
		}
		t.logf("triage round %d: %s undecided", round, text.Count(len(q.Pending()), "finding"))
		res, err := t.Agent.Run(ctx, AgentRequest{Name: TriageName, Prompt: t.prompt(q, refused), Dir: dir})
		if err != nil {
			return "", err
		}
		refused = nil
		ans, err := parseTriage(res.Text)
		if err != nil {
			refused = append(refused, err.Error())
			t.logf("  refused: %v", err)
			continue
		}
		for _, d := range ans.Decisions {
			err := t.take(ctx, q, d)
			var refusal *Refusal
			if errors.As(err, &refusal) {
				note := fmt.Sprintf("%s %s: %v", d.Action, d.Finding, err)
				refused = append(refused, note)
				t.logf("  refused: %s", note)
				continue
			}
			if err != nil {
				return "", err
			}
		}
		if choosing {
			if ans.End != "" {
				end = ans.End
			}
			// An end is checked against what is selected after every
			// round, not only the one that chose it: a later round can
			// leave a comment-only review with nothing to say.
			if end != "" {
				if err := endRefusal(end, q.Selected()); err != nil {
					note := fmt.Sprintf("end %s: %v", end, err)
					refused = append(refused, note)
					t.logf("  refused: %s", note)
					end = ""
				}
			}
		}
		if len(ans.Decisions) == 0 && len(refused) == 0 {
			break
		}
	}
	t.logf("%s", q.Summary())
	switch {
	case !choosing:
		return t.Mode, nil
	case end == "":
		t.logf("the agent chose no end it could take, so nothing is posted")
		return OutputDiscard, nil
	}
	t.logf("the agent chose to end the review: %s", end)
	return end, nil
}

// chooses reports whether the agent chooses how the review ends.
func (t *AgentTriage) chooses() bool { return t.Mode == "" || t.Mode == OutputAsk }

// take takes one of the agent's decisions on the queue and logs it. An
// action that is not one of the four is refused, as the queue refuses what
// it would not take.
func (t *AgentTriage) take(ctx context.Context, q *Queue, d triageDecision) error {
	if !slices.Contains(Actions, d.Action) {
		return refuse("%q is not an action: give one of %s", d.Action, strings.Join(Actions, ", "))
	}
	found, err := q.Find(d.Finding)
	if err != nil {
		return err
	}
	f := *found
	switch d.Action {
	case ActionSelect:
		if err := q.Select(f.ID, d.Comment); err != nil {
			return err
		}
		t.logf("  select %s: %s", f.ID, f.Title)
	case ActionDismiss:
		if err := q.Dismiss(f.ID, d.Reason); err != nil {
			return err
		}
		t.logf("  dismiss %s: %s", f.ID, strings.TrimSpace(d.Reason))
	case ActionDefer:
		if err := q.Defer(f.ID); err != nil {
			return err
		}
		t.logf("  defer %s: %s", f.ID, f.Title)
	case ActionAsk:
		answer, err := q.Ask(ctx, f.ID, d.Question)
		if err != nil {
			return err
		}
		t.logf("  ask %s: %s", f.ID, strings.TrimSpace(d.Question))
		t.logf("%s", indent(indent(answer.Text)))
		for _, a := range answer.Added {
			t.logf("    added %s  %s  %s", a.ID, a.Severity, a.Title)
		}
	}
	return nil
}

// endRefusal says why a review cannot end as end: it is not one of the five
// ends, or it posts a review GitHub would not take with what is selected
// (emptyReview). It is nil for an end that can be taken.
func endRefusal(end string, selected []Selection) error {
	if !slices.Contains(ends, end) {
		return fmt.Errorf("not an end: give one of %s", strings.Join(ends, ", "))
	}
	if Posts(end) {
		return emptyReview(end, selected)
	}
	return nil
}

// triageAnswer is what a triage session answers with.
type triageAnswer struct {
	Decisions []triageDecision `json:"decisions"`
	// End is how the review ends, when the session chose.
	End string `json:"end"`
}

// triageDecision is one action a triage session takes on a finding, with
// what the action needs: the comment text of a selection that edits it,
// the reason of a dismissal, the question of an ask.
type triageDecision struct {
	Finding  string `json:"finding"`
	Action   string `json:"action"`
	Comment  string `json:"comment"`
	Reason   string `json:"reason"`
	Question string `json:"question"`
}

// parseTriage reads a triage session's answer: the JSON object in it, with
// the ids, actions and end trimmed and the actions and end lowercased. An
// answer with no object, or one that is not a triage answer, is an error
// saying so, which the next round is told.
func parseTriage(text string) (*triageAnswer, error) {
	obj, ok := jsonObject(text)
	if !ok {
		return nil, errors.New("your answer had no JSON object in it: answer with the JSON object alone")
	}
	var ans triageAnswer
	if err := json.Unmarshal([]byte(obj), &ans); err != nil {
		return nil, fmt.Errorf("your answer's JSON object is not a triage answer: %v", err)
	}
	for i := range ans.Decisions {
		d := &ans.Decisions[i]
		d.Finding = strings.TrimSpace(d.Finding)
		d.Action = strings.ToLower(strings.TrimSpace(d.Action))
	}
	ans.End = strings.ToLower(strings.TrimSpace(ans.End))
	return &ans, nil
}

// prompt is the whole of what one triage session is told: the frame, the
// task's instructions, how the review ends, what the reviewer notes have
// dismissed before in this repository, the brief, the findings, and what
// the last answer could not do, in that order. The findings can be many, so
// the instruction they follow is repeated in one line at the end.
func (t *AgentTriage) prompt(q *Queue, refused []string) string {
	var out strings.Builder
	out.WriteString(triageFrame)
	if instructions := strings.TrimSpace(t.Instructions); instructions != "" {
		out.WriteString("\n---\n\n## Your instructions\n\n")
		out.WriteString("Whoever runs this review wants this from its triage. Follow it in what you select, dismiss and defer, and in how you end the review.\n\n")
		out.WriteString(instructions + "\n")
	}
	out.WriteString("\n---\n\n")
	out.WriteString(t.endSection())
	if dismissed := triageNoiseSection(q.rules(), q.repo()); dismissed != "" {
		out.WriteString("\n---\n\n")
		out.WriteString(dismissed)
	}
	out.WriteString("\n---\n\n")
	out.WriteString(q.Artifact.Brief.Text())
	out.WriteString("\n---\n\n")
	out.WriteString(findingsSection(q))
	if len(refused) > 0 {
		out.WriteString("\n---\n\n## What your last answer could not do\n\n")
		for _, r := range refused {
			out.WriteString("- " + r + "\n")
		}
	}
	fmt.Fprintf(&out, "\n---\n\nTriage the review of %s. Answer with the JSON object alone.\n", q.Artifact.Brief.Ref)
	return out.String()
}

// endSection is what a triage session is told about how the review ends:
// the end the command chose, or the five to choose from and how.
func (t *AgentTriage) endSection() string {
	var out strings.Builder
	out.WriteString("## How the review ends\n\n")
	if !t.chooses() {
		fmt.Fprintf(&out, "Whoever runs this review chose how it ends: `%s`, so %s. Leave `end` out of your answer.\n", t.Mode, endEffects[t.Mode])
		return out.String()
	}
	out.WriteString("Once triage is over, the review ends one of five ways, and you choose which with `end`:\n\n")
	for _, e := range ends {
		fmt.Fprintf(&out, "- `%s`: %s\n", e, endEffects[e])
	}
	out.WriteString("\nChoose as your instructions say. Where they say nothing about it, request changes when a selected finding must be fixed before the change merges, comment when you selected anything else, and discard when you selected nothing. Approve only when your instructions allow it: an approval is the reviewer's to give. ")
	out.WriteString("A comment-only or request-changes review needs at least one selected finding. Give `end` once nothing is left undecided; the last one you give is the one taken.\n")
	return out.String()
}

// triageNoiseSection is what a triage session reviewing repo is told about
// the reviewer notes' rules: every rule that names the repository and says
// something, whichever angle it is about. It is empty when none does.
func triageNoiseSection(rules []Rule, repo string) string {
	var out strings.Builder
	for _, r := range rules {
		if !matchesPattern(r.Repo, repo) || strings.TrimSpace(r.Text) == "" {
			continue
		}
		if out.Len() == 0 {
			out.WriteString("## Dismissed before\n\n")
			out.WriteString("The reviewer of this repository has dismissed findings like these in earlier reviews, from the angle and in the category named:\n\n")
		}
		fmt.Fprintf(&out, "- [%s] [%s] %s\n", or(r.Angle, anyValue), or(r.Category, anyValue), strings.TrimSpace(r.Text))
	}
	if out.Len() == 0 {
		return ""
	}
	out.WriteString("\nDismiss a finding that reads like one of them, unless this change makes it newly wrong.\n")
	return out.String()
}

// findingsSection is the findings as a triage session reads them: every one
// still undecided, whole, then every decided one in a line saying what was
// decided.
func findingsSection(q *Queue) string {
	var out strings.Builder
	pending := q.Pending()
	out.WriteString("## Undecided findings\n")
	if len(pending) == 0 {
		out.WriteString("\nNothing is undecided.\n")
	}
	for _, f := range pending {
		fmt.Fprintf(&out, "\n### %s · %s · %s · %s\n%s", f.ID, f.Severity, f.Angle, f.Category, q.Describe(f))
	}
	var lines []string
	for _, f := range q.Findings() {
		d, ok := q.DecisionOn(f.ID)
		if !ok {
			continue
		}
		line := fmt.Sprintf("- %s %s: %s", f.ID, decided[d.Action], f.Title)
		if d.Reason != "" {
			line += " (" + d.Reason + ")"
		}
		lines = append(lines, line)
	}
	if len(lines) > 0 {
		out.WriteString("\n## Decided\n\n" + strings.Join(lines, "\n") + "\n")
	}
	return out.String()
}

// logf writes one line of what triage is doing.
func (t *AgentTriage) logf(format string, args ...any) {
	if t.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(t.Log, format+"\n", args...)
}
