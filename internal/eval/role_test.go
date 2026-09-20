package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/review"
)

// developerCase runs the developer alone on one ready issue, and grades
// what its session reported, the pull request it opened and a rubric.
const developerCase = `
description = "the answer is wrong"
issue = 1
timeout = "2m"
max_cost = 3

[[issues]]
number = 1
title = "Fix the answer"
body = "answer.txt should say fixed."
labels = ["bees:ready", "bees:size/xs"]

[expect]
outcome = "pr-opened"
pull_requests = [1]

[[expect.labels]]
issue = 1
missing = ["bees:ready"]

[[expect.graded]]
name = "the pull request says what changed"
rubric = "The pull request body says what changed and closes the issue."
`

// projectManagerCase runs the project manager alone on a triage queue, a
// blocked issue it owes an answer on, and a duplicate to close.
const projectManagerCase = `
description = "one issue to refine, one question to answer and one duplicate"
timeout = "2m"
max_cost = 3

[[issues]]
number = 1
title = "Fix the answer"
body = "answer.txt is wrong."
labels = ["bees:triage"]

[[issues]]
number = 2
title = "Another thing"
body = "asked about below."
labels = ["bees:blocked", "bees:size/xs"]

[[issues]]
number = 3
title = "answer.txt is wrong"
body = "the same as #1."
labels = ["bees:triage"]

[[mail]]
from = "developer"
to = "project_manager"
subject = "what does fixed mean"
body = "The issue does not say what to write."
issue = 2

[expect]
outcome = "done"
issues_closed = [3]

[[expect.labels]]
issue = 1
has = ["bees:ready"]
missing = ["bees:triage"]

[[expect.mail]]
to = "developer"
issue = 2
`

// qaCase runs QA alone and grades the bug it files and the report it owes
// the product manager.
const qaCase = `
description = "the default branch is broken"
timeout = "2m"
max_cost = 3

[[issues]]
number = 1
title = "Fix the answer"
body = "answer.txt should say fixed."
labels = ["bees:ready", "bees:size/xs"]

[expect]
outcome = "done"
issues_created = 1

[[expect.mail]]
to = "product_manager"
`

// runRoleCase runs one per-role case end to end with the fake agents.
func runRoleCase(t *testing.T, role, caseTOML string, files map[string]string, tune func(*Runner)) (*Report, CaseResult) {
	t.Helper()
	root := t.TempDir()
	dir := writeCase(t, filepath.Join(root, "evals", role), "subject", caseTOML, files)
	c, err := LoadRoleCase(dir, role)
	if err != nil {
		t.Fatal(err)
	}
	r, console := testRunner(t)
	r.Grader = &review.CLIAgent{ClaudeBin: os.Args[0]}
	if tune != nil {
		tune(r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rep, err := r.Run(ctx, []Case{c}, builtIn(t), filepath.Join(root, "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(rep.Dir, "subject", "state", "bees.log"))
			t.Logf("console:\n%s\nbees.log:\n%s", console.String(), b)
		}
	})
	if len(rep.Cases) != 1 {
		t.Fatalf("cases: %+v", rep.Cases)
	}
	return rep, rep.Cases[0]
}

func answerRepo() map[string]string {
	return map[string]string{"repo/answer.txt": "broken\n"}
}

// The developer runs alone: it opens its pull request, every check the case
// declared passes, the graded check carries the score the grader answered,
// and no other role ran.
func TestRoleCaseRunsTheDeveloperAlone(t *testing.T) {
	rep, res := runRoleCase(t, config.RoleDeveloper, developerCase, answerRepo(), nil)
	if !res.Pass || res.Stop != StopDone || res.Error != "" || !rep.Pass() {
		t.Fatalf("result: %+v", res)
	}
	if res.Role != config.RoleDeveloper || rep.Role != config.RoleDeveloper {
		t.Fatalf("role: %q %q", res.Role, rep.Role)
	}
	for _, name := range []string{`the session reported "pr-opened"`, "#1 has a pull request",
		"#1 no longer carries bees:ready", "the pull request says what changed"} {
		if c := checkNamed(t, res, name); !c.Pass {
			t.Errorf("check %q failed: %+v", name, c)
		}
	}
	graded := checkNamed(t, res, "the pull request says what changed")
	if graded.Score == nil || *graded.Score != 0.9 || graded.Detail != "the fake grader read the rubric" {
		t.Fatalf("graded check: %+v", graded)
	}
	if res.Score == nil || *res.Score != 0.9 {
		t.Fatalf("case score: %+v", res.Score)
	}
	// The grader's session is counted with the rest of the spend.
	if res.Sessions != 2 {
		t.Errorf("sessions: %+v", res)
	}
	// Nothing but the developer was enabled, so no other role could run.
	var written struct {
		Roles map[string]struct {
			Enabled *bool `toml:"enabled"`
		} `toml:"roles"`
	}
	path := filepath.Join(res.Dir, "project", "bees.toml")
	if _, err := toml.DecodeFile(path, &written); err != nil {
		t.Fatal(err)
	}
	for _, role := range config.Roles {
		enabled := written.Roles[role].Enabled
		if role == config.RoleDeveloper {
			if enabled != nil {
				t.Fatalf("the role under eval carries enabled = %v", *enabled)
			}
			continue
		}
		if enabled == nil || *enabled {
			t.Errorf("the %s is not disabled in %s", role, path)
		}
	}
	if table := rep.Table(); !strings.Contains(table, "SCORE") || !strings.Contains(table, "0.90") {
		t.Fatalf("table:\n%s", table)
	}
}

// The project manager runs alone: the labels it moved, the mail it sent and
// the duplicate it closed are each a check of their own.
func TestRoleCaseChecksLabelsMailAndAClosedIssue(t *testing.T) {
	t.Setenv("FAKE_PM", "1")
	_, res := runRoleCase(t, config.RoleProjectManager, projectManagerCase, answerRepo(), nil)
	if !res.Pass || res.Stop != StopDone || res.Error != "" {
		t.Fatalf("result: %+v", res)
	}
	for _, name := range []string{`the session reported "done"`, "#1 carries bees:ready",
		"#1 no longer carries bees:triage", "mail to the developer about #2", "#3 closed"} {
		if c := checkNamed(t, res, name); !c.Pass {
			t.Errorf("check %q failed: %+v", name, c)
		}
	}
	if c := checkNamed(t, res, "mail to the developer about #2"); c.Detail != "Re: what does fixed mean" {
		t.Errorf("mail check: %+v", c)
	}
}

// A case that declares checks the role does not meet fails on each of them,
// and the table says what was observed rather than what was asked for.
func TestRoleCaseFailsEveryCheckTheRoleDidNotMeet(t *testing.T) {
	rep, res := runRoleCase(t, config.RoleProjectManager, projectManagerCase, answerRepo(), nil)
	if res.Pass || rep.Pass() {
		t.Fatalf("a project manager that did nothing passed: %+v", res)
	}
	for _, name := range []string{"#1 carries bees:ready", "mail to the developer about #2", "#3 closed"} {
		if c := checkNamed(t, res, name); c.Pass {
			t.Errorf("check %q passed: %+v", name, c)
		}
	}
	table := rep.Table()
	for _, line := range []string{
		"subject: #1 does not carry bees:ready",
		"subject: the project_manager sent the developer no mail about #2",
		"subject: #3 was not closed",
	} {
		if !strings.Contains(table, line) {
			t.Errorf("no %q in the table:\n%s", line, table)
		}
	}
}

// QA runs alone: the bug it filed is an issue the case did not seed, and
// the report it owes the product manager is mail.
func TestRoleCaseCountsTheIssuesTheRoleCreated(t *testing.T) {
	t.Setenv("FAKE_QA", "1")
	_, res := runRoleCase(t, config.RoleQA, qaCase, answerRepo(), nil)
	if !res.Pass || res.Stop != StopDone || res.Error != "" {
		t.Fatalf("result: %+v", res)
	}
	created := checkNamed(t, res, "1 issue created")
	if !created.Pass || !strings.Contains(created.Detail, "greet.sh drops the comma") {
		t.Fatalf("created check: %+v", created)
	}
}

// recordingGrader answers every rubric with the same thing and keeps the
// prompt it was given.
type recordingGrader struct {
	answer  string
	prompts []string
}

func (g *recordingGrader) Run(_ context.Context, req review.AgentRequest) (*review.AgentResult, error) {
	g.prompts = append(g.prompts, req.Prompt)
	return &review.AgentResult{Text: g.answer, Turns: 1, CostKnown: true}, nil
}

// A grader session is given the rubric, the transcript of the role's
// session and the state the run left behind.
func TestGraderSessionSeesTheTranscriptAndTheEndState(t *testing.T) {
	g := &recordingGrader{answer: `{"score": 0.4, "reasons": "because"}`}
	_, res := runRoleCase(t, config.RoleDeveloper, developerCase, answerRepo(), func(r *Runner) { r.Grader = g })
	if len(g.prompts) != 1 {
		t.Fatalf("%d grader sessions", len(g.prompts))
	}
	prompt := g.prompts[0]
	for _, want := range []string{
		"The pull request body says what changed and closes the issue.",
		"the answer is wrong",            // the case description
		"What the developer session did", // the transcript's heading
		`"type":"result"`,                // a line of the session's own stream
		"#1 Fix the answer",              // the end state
		"Closes #1",                      // the pull request the developer opened
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("no %q in the grader's prompt:\n%s", want, prompt)
		}
	}
	// 0.4 is under the default pass score, so the check and the case fail.
	graded := checkNamed(t, res, "the pull request says what changed")
	if graded.Pass || res.Pass {
		t.Fatalf("a check scored 0.4 passed: %+v", graded)
	}
	if graded.Failure != "the pull request says what changed: scored 0.40, under 0.70" {
		t.Fatalf("failure line: %q", graded.Failure)
	}
}

// With no grader, a graded check fails rather than passing unjudged, and
// the mechanical checks still stand.
func TestGradedCheckFailsWithoutAGrader(t *testing.T) {
	_, res := runRoleCase(t, config.RoleDeveloper, developerCase, answerRepo(), func(r *Runner) { r.Grader = nil })
	if res.Pass {
		t.Fatalf("the case passed with nothing grading it: %+v", res)
	}
	graded := checkNamed(t, res, "the pull request says what changed")
	if graded.Pass || graded.Score != nil || !strings.Contains(graded.Failure, "no grader agent") {
		t.Fatalf("graded check: %+v", graded)
	}
	if c := checkNamed(t, res, "#1 has a pull request"); !c.Pass {
		t.Errorf("the mechanical check did not stand: %+v", c)
	}
}
