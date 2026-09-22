package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

[[issues.comments]]
author = "kpenfound"
body = "Lowercase, on one line, nothing else."

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
	// Nothing but the developer ran: one session directory, and it is the
	// developer's. The reviewer never saw the pull request.
	sessions, err := filepath.Glob(filepath.Join(res.Dir, "state", "sessions", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !strings.Contains(filepath.Base(sessions[0]), config.RoleDeveloper) {
		t.Fatalf("sessions: %v", sessions)
	}
	// A per-role run merges nothing and takes no second pass: the default
	// branch is still the one commit the fixture was built with.
	log, err := git(context.Background(), filepath.Join(res.Dir, "origin.git"), "log", "--oneline", "main")
	if err != nil {
		t.Fatal(err)
	}
	if lines := strings.Count(strings.TrimSpace(log), "\n") + 1; lines != 1 {
		t.Fatalf("the default branch moved:\n%s", log)
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
	for _, name := range []string{"#1 carries bees:ready", "#1 no longer carries bees:triage",
		"mail to the developer about #2", "#3 closed"} {
		if c := checkNamed(t, res, name); c.Pass {
			t.Errorf("check %q passed: %+v", name, c)
		}
	}
	table := rep.Table()
	for _, line := range []string{
		"subject: #1 does not carry bees:ready",
		"subject: #1 still carries bees:triage",
		"subject: the project_manager sent the developer no mail about #2",
		"subject: #3 was not closed",
	} {
		if !strings.Contains(table, line) {
			t.Errorf("no %q in the table:\n%s", line, table)
		}
	}
}

// The outcome check is the status the session reported, not any status: a
// developer that opened a pull request did not report "done".
func TestRoleCaseChecksWhichOutcomeTheSessionReported(t *testing.T) {
	_, res := runRoleCase(t, config.RoleDeveloper,
		strings.Replace(developerCase, `outcome = "pr-opened"`, `outcome = "question"`, 1), answerRepo(), nil)
	if res.Pass {
		t.Fatalf("a session that reported pr-opened passed a case wanting question: %+v", res)
	}
	c := checkNamed(t, res, `the session reported "question"`)
	if c.Pass || c.Failure != `the session reported "pr-opened", not "question"` {
		t.Fatalf("outcome check: %+v", c)
	}
}

// Mail about one issue does not answer an expectation about another.
func TestRoleCaseMailCheckIsAboutTheIssueItNames(t *testing.T) {
	t.Setenv("FAKE_PM", "1")
	_, res := runRoleCase(t, config.RoleProjectManager,
		projectManagerCase+"\n[[expect.mail]]\nto = \"developer\"\nissue = 1\n", answerRepo(), nil)
	if res.Pass {
		t.Fatalf("mail about #2 answered an expectation about #1: %+v", res)
	}
	if c := checkNamed(t, res, "mail to the developer about #2"); !c.Pass {
		t.Fatalf("the mail that was sent: %+v", c)
	}
	c := checkNamed(t, res, "mail to the developer about #1")
	if c.Pass || c.Failure != "the project_manager sent the developer no mail about #1" {
		t.Fatalf("mail check: %+v", c)
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

// issues_created is a count, not "at least one".
func TestRoleCaseCountsExactlyTheIssuesAsked(t *testing.T) {
	t.Setenv("FAKE_QA", "1")
	_, res := runRoleCase(t, config.RoleQA, strings.Replace(qaCase, "issues_created = 1", "issues_created = 2", 1), answerRepo(), nil)
	if res.Pass {
		t.Fatalf("one issue answered an expectation of two: %+v", res)
	}
	c := checkNamed(t, res, "2 issues created")
	if c.Pass || c.Failure != "the run created 1 issue, not 2" {
		t.Fatalf("created check: %+v", c)
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

// The grader is a review agent on the shared restricted execution, and the
// review configuration can select any of the four agents for it: the fake
// answers in each backend's stream format, and every rubric is scored.
func TestTheGraderRunsOnEveryProvider(t *testing.T) {
	for _, provider := range []string{"", config.AgentClaude, config.AgentCodex, config.AgentOpenCode, config.AgentPi} {
		name := provider
		if name == "" {
			name = "default"
		}
		t.Run(name, func(t *testing.T) {
			g := &review.CLIAgent{Provider: provider, ClaudeBin: os.Args[0], CodexBin: os.Args[0], OpenCodeBin: os.Args[0], PiBin: os.Args[0]}
			_, res := runRoleCase(t, config.RoleDeveloper, developerCase, answerRepo(), func(r *Runner) { r.Grader = g })
			graded := checkNamed(t, res, "the pull request says what changed")
			if !graded.Pass {
				t.Fatalf("the %s grader scored the case under the pass score: %+v", name, graded)
			}
		})
	}
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
		"the answer is wrong",                   // the case description
		"What the developer session did",        // the transcript's heading
		`"type":"result"`,                       // a line of the session's own stream
		"#1 Fix the answer",                     // the end state
		"Lowercase, on one line, nothing else.", // a comment the case seeded on the issue
		"Closes #1",                             // the pull request the developer opened
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

// routingCase seeds an issue nobody has labelled, which the scheduler
// routes on the first pass.
const routingCase = `
description = "an issue nobody has labelled"
timeout = "2m"
max_cost = 3

[[issues]]
number = 1
title = "Something is off"
body = "no labels at all"

[expect]
outcome = "done"

[[expect.labels]]
issue = 1
has = ["bees:feedback"]
missing = ["bees:triage", "bees:ready"]
`

// Running one role does not move the workflow the rest of the factory
// would follow: a new issue is routed to the product manager, the way the
// configured factory routes it, even though only the project manager runs.
// Scoping with roles.<name>.enabled instead would send it to the ready
// queue, because reconcile reads the configuration and not the scope.
func TestRoleCaseLeavesTheFactorysRoutingAlone(t *testing.T) {
	_, res := runRoleCase(t, config.RoleProjectManager, routingCase, answerRepo(), nil)
	if !res.Pass || res.Stop != StopDone || res.Error != "" {
		t.Fatalf("result: %+v", res)
	}
	for _, name := range []string{"#1 carries bees:feedback", "#1 no longer carries bees:ready"} {
		if c := checkNamed(t, res, name); !c.Pass {
			t.Errorf("check %q failed: %+v", name, c)
		}
	}
}

// The case's timeout bounds a per-role run the way it bounds a
// whole-factory one: the session still running is stopped, not waited for.
func TestRoleCaseStopsAtTheTimeout(t *testing.T) {
	t.Setenv("FAKE_DEV_HANG", "60")
	start := time.Now()
	_, res := runRoleCase(t, config.RoleDeveloper,
		strings.Replace(developerCase, `timeout = "2m"`, `timeout = "3s"`, 1), answerRepo(), nil)
	if res.Pass || res.Stop != StopTimeout {
		t.Fatalf("result: %+v", res)
	}
	if d := time.Since(start); d > 45*time.Second {
		t.Fatalf("the hung session was not stopped: the run took %s", d)
	}
	// Nothing was graded off a session that never finished.
	if c := checkNamed(t, res, `the session reported "pr-opened"`); c.Pass {
		t.Fatalf("outcome check: %+v", c)
	}
}

// The case's budget bounds it too: the session's cost passes max_cost and
// the run stops there.
func TestRoleCaseStopsAtTheBudget(t *testing.T) {
	t.Setenv("FAKE_COST", "5")
	_, res := runRoleCase(t, config.RoleProjectManager,
		strings.Replace(projectManagerCase, "max_cost = 3", "max_cost = 1", 1), answerRepo(), nil)
	if res.Pass || res.Stop != StopBudget || res.CostUSD < 1 {
		t.Fatalf("result: %+v", res)
	}
}

// A developer that gives up reports `failed`, the factory hands the issue
// to a person, and the run still ends as done: a case can declare that
// outcome and pass on it.
func TestRoleCaseCanExpectAFailedDeveloper(t *testing.T) {
	t.Setenv("FAKE_DEV_FAIL", "1")
	_, res := runRoleCase(t, config.RoleDeveloper, `
issue = 1
timeout = "2m"
max_cost = 3

[[issues]]
number = 1
title = "Fix the answer"
body = "answer.txt should say fixed."
labels = ["bees:ready", "bees:size/xs"]

[expect]
outcome = "failed"

[[expect.labels]]
issue = 1
has = ["bees:needs-human"]
`, answerRepo(), nil)
	if !res.Pass || res.Stop != StopDone || res.Error != "" {
		t.Fatalf("result: %+v", res)
	}
}

// A per-role case that seeds a pull request hands it to the session: the
// developer is given the pull request of the issue's branch, and the
// GitHub it reads answers with the title, body, comments, reviews and diff
// the case declared.
func TestRoleCaseSeedsAPullRequestTheSessionReads(t *testing.T) {
	t.Setenv("FAKE_DEV_SEES_PR", "1")
	_, res := runRoleCase(t, config.RoleDeveloper, prCase, prFiles(), nil)
	if !res.Pass || res.Stop != StopDone || res.Error != "" {
		t.Fatalf("result: %+v", res)
	}
	// The session was handed the seeded pull request, not a new one: it
	// reported pr-updated.
	if c := checkNamed(t, res, `the session reported "pr-updated"`); !c.Pass {
		t.Fatalf("outcome check: %+v", c)
	}
	seen, err := os.ReadFile(filepath.Join(res.Dir, "state", "seen-pr.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"title":"Fix the answer"`,
		`"body":"Closes #1"`,
		`"headRefName":"bees/issue-1"`,
		`"baseRefName":"main"`,
		`"bees:wip"`,           // a label the case gave the pull request
		"-broken",              // the diff of the seeded branch
		"+half fixed",          //
		"it still says broken", // the comment
		"CHANGES_REQUESTED",    // the review
		"one line, lowercase",
	} {
		if !strings.Contains(string(seen), want) {
			t.Errorf("no %q in what the session read:\n%s", want, seen)
		}
	}
}

// reviewerCase runs the reviewer alone on a seeded pull request: it is the
// review loop's review stage, so the review pipeline runs before the
// session that reports the verdict.
const reviewerCase = `
description = "a pull request that says it fixes the answer"
issue = 1
pr = 2
timeout = "2m"
max_cost = 3

[[issues]]
number = 1
title = "Fix the answer"
body = "answer.txt should say fixed."
labels = ["bees:review", "bees:size/xs"]

[[pull_requests]]
number = 2
title = "Fix the answer"
body = "Closes #1"
head = "bees/issue-1"

[expect]
outcome = "approved"

[[expect.labels]]
issue = 1
has = ["bees:approved"]
`

// The reviewer runs alone on the pull request the case seeded: it approves
// it, the issue ends up waiting for a person to merge, and no other role's
// session ran.
func TestRoleCaseRunsTheReviewerAlone(t *testing.T) {
	_, res := runRoleCase(t, config.RoleReviewer, reviewerCase, prFiles(), nil)
	if !res.Pass || res.Stop != StopDone || res.Error != "" {
		t.Fatalf("result: %+v", res)
	}
	for _, name := range []string{`the session reported "approved"`, "#1 carries bees:approved"} {
		if c := checkNamed(t, res, name); !c.Pass {
			t.Errorf("check %q failed: %+v", name, c)
		}
	}
	sessions, err := filepath.Glob(filepath.Join(res.Dir, "state", "sessions", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !strings.Contains(filepath.Base(sessions[0]), config.RoleReviewer) {
		t.Fatalf("sessions: %v", sessions)
	}
}

// A reviewer case that ends in changes requested ends there: the review
// loop's next stage is a developer session, which a per-role run must not
// start, so the one round the eval configures escalates the issue to a
// person instead. Without it the developer would run, and the verdict the
// case grades would be a later round's.
func TestRoleCaseReviewerStopsAtOneRound(t *testing.T) {
	t.Setenv("FAKE_REVIEW_CHANGES", "1")
	_, res := runRoleCase(t, config.RoleReviewer, strings.NewReplacer(
		`outcome = "approved"`, "outcome = \"changes-requested\"\n\n[[expect.mail]]\nto = \"developer\"\nissue = 1",
		`has = ["bees:approved"]`, `has = ["bees:needs-human"]`).Replace(reviewerCase), prFiles(), nil)
	if !res.Pass || res.Stop != StopDone || res.Error != "" {
		t.Fatalf("result: %+v", res)
	}
	for _, name := range []string{`the session reported "changes-requested"`, "#1 carries bees:needs-human",
		"mail to the developer about #1"} {
		if c := checkNamed(t, res, name); !c.Pass {
			t.Errorf("check %q failed: %+v", name, c)
		}
	}
	// One session, the reviewer's: the developer round the verdict would
	// otherwise lead to never started.
	sessions, err := filepath.Glob(filepath.Join(res.Dir, "state", "sessions", "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sessions) != 1 || !strings.Contains(filepath.Base(sessions[0]), config.RoleReviewer) {
		t.Fatalf("sessions: %v", sessions)
	}
}
