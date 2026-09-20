package eval

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/fakegh"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
)

// writeCase writes a case directory under root: its case.toml and the
// files given by their path in the case directory ("repo/answer.txt").
func writeCase(t *testing.T, root, name, caseTOML string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(root, name)
	files[CaseFile] = caseTOML
	for rel, content := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// answerCase is a fixture whose answer.txt is wrong, graded by a check
// kept out of the fixture in grade/, with the one issue that asks for the
// fix and a hint mailed to the developer.
const answerCase = `
description = "the answer is wrong"
test = "sh check.sh"
timeout = "2m"
max_cost = 3

[[issues]]
number = 1
title = "Fix the answer"
body = "answer.txt should say fixed."
labels = ["bees:ready", "bees:size/xs"]

[[mail]]
to = "developer"
subject = "A hint"
body = "The answer is the word fixed."
issue = 1
`

func answerFiles() map[string]string {
	return map[string]string{
		"repo/answer.txt": "broken\n",
		"grade/check.sh":  "grep -qx fixed answer.txt\n",
	}
}

// testRunner runs the fake agent in TestMain as claude, and this test
// binary as bees, so the shim's gh is served by TestMain too.
func testRunner(t *testing.T) (*Runner, *bytes.Buffer) {
	t.Helper()
	t.Setenv("FAKE_CLAUDE", "1")
	var console bytes.Buffer
	return &Runner{Bees: os.Args[0], ClaudeBin: os.Args[0], CodexBin: os.Args[0], OpenCodeBin: os.Args[0], PiBin: os.Args[0],
		PassInterval: "100ms", Console: &console}, &console
}

func builtIn(t *testing.T) Selection {
	t.Helper()
	sel, err := SelectProfile("", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sel
}

func runOne(t *testing.T, caseTOML string, files map[string]string) (*Report, CaseResult) {
	t.Helper()
	root := t.TempDir()
	c, err := LoadCase(writeCase(t, root, "answer", caseTOML, files))
	if err != nil {
		t.Fatal(err)
	}
	r, console := testRunner(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	rep, err := r.Run(ctx, []Case{c}, builtIn(t), filepath.Join(root, "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() {
			b, _ := os.ReadFile(filepath.Join(rep.Dir, "answer", "state", "bees.log"))
			t.Logf("console:\n%s\nbees.log:\n%s", console.String(), b)
		}
	})
	if len(rep.Cases) != 1 {
		t.Fatalf("cases: %+v", rep.Cases)
	}
	return rep, rep.Cases[0]
}

func checkNamed(t *testing.T, res CaseResult, name string) Check {
	t.Helper()
	for _, c := range res.Checks {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no check %q in %+v", name, res.Checks)
	return Check{}
}

// The whole loop: the developer opens its pull request through the eval's
// gh, the reviewer approves it, the runner merges it, which closes the
// issue, and the hidden check passes on main.
func TestRunPassesACaseTheFactorySolves(t *testing.T) {
	rep, res := runOne(t, answerCase, answerFiles())
	if !res.Pass || res.Stop != StopDone || res.Error != "" || !rep.Pass() {
		t.Fatalf("result: %+v", res)
	}
	for _, name := range []string{"the test fails on the fixture", "#1 closed", "#1 has a pull request", "the test passes"} {
		if c := checkNamed(t, res, name); !c.Pass {
			t.Errorf("check %q failed: %+v", name, c)
		}
	}
	if c := checkNamed(t, res, "#1 has a pull request"); c.Detail != "#2" {
		t.Errorf("pull request check: %+v", c)
	}
	// Developer, reviewer and product manager, at 0.01 each.
	if res.Sessions < 3 || res.CostUSD < 0.03 || res.Turns < 6 || res.Profile != "default (built-in)" {
		t.Errorf("spend: %+v", res)
	}
	// The mail the case seeded reached the developer's session.
	devs, _ := filepath.Glob(filepath.Join(res.Dir, "state", "sessions", "*developer-issue-1*", "prompt.md"))
	if len(devs) == 0 {
		t.Fatal("no developer session")
	}
	if b, _ := os.ReadFile(devs[0]); !strings.Contains(string(b), "The answer is the word fixed.") {
		t.Errorf("the seeded mail is not in the developer's task:\n%s", b)
	}
	// The report on disk is the one returned.
	b, err := os.ReadFile(rep.Path())
	if err != nil {
		t.Fatal(err)
	}
	var onDisk Report
	if err := json.Unmarshal(b, &onDisk); err != nil {
		t.Fatal(err)
	}
	if len(onDisk.Cases) != 1 || !onDisk.Cases[0].Pass || onDisk.Profile.Source != BuiltIn || onDisk.Profile.Roles["developer"] != "claude opus" {
		t.Fatalf("report.json: %s", b)
	}
	table := rep.Table()
	if !strings.Contains(table, "answer") || !strings.Contains(table, "pass") || !strings.Contains(table, "done") {
		t.Fatalf("table:\n%s", table)
	}
}

// A pull request that changes the wrong thing merges and closes its issue,
// and the case fails on its test.
func TestRunFailsACaseWhoseTestStillFails(t *testing.T) {
	t.Setenv("FAKE_DEV_NOFIX", "1")
	rep, res := runOne(t, answerCase, answerFiles())
	if res.Pass || rep.Pass() || res.Stop != StopDone {
		t.Fatalf("result: %+v", res)
	}
	if c := checkNamed(t, res, "#1 closed"); !c.Pass {
		t.Errorf("the merged pull request did not close #1: %+v", c)
	}
	test := checkNamed(t, res, "the test passes")
	if test.Pass || !strings.HasSuffix(test.Detail, "after.log") {
		t.Fatalf("test check: %+v", test)
	}
	if table := rep.Table(); !strings.Contains(table, "FAIL") || !strings.Contains(table, "answer: the test still fails on the default branch") {
		t.Fatalf("table:\n%s", table)
	}
}

// A test that already passes on the fixture proves nothing: no session
// runs. The fixture here is built by setup.sh.
func TestRunRefusesACaseWhoseTestAlreadyPasses(t *testing.T) {
	rep, res := runOne(t, answerCase, map[string]string{
		SetupScript:      "echo fixed > answer.txt\n",
		"grade/check.sh": "grep -qx fixed answer.txt\n",
	})
	if res.Pass || res.Stop != StopInvalid || res.Sessions != 0 {
		t.Fatalf("result: %+v", res)
	}
	if c := checkNamed(t, res, "the test fails on the fixture"); c.Pass {
		t.Fatalf("check: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(res.Dir, "state")); !os.IsNotExist(err) {
		t.Fatalf("a factory was built for an invalid case: %v", err)
	}
	// The line says what happened, not the requirement it broke: the
	// test passed where it had to fail.
	table := rep.Table()
	if !strings.Contains(table, "answer: the test passed on the fixture, where it has to fail") {
		t.Fatalf("table:\n%s", table)
	}
	if strings.Contains(table, "answer: failed: the test fails on the fixture") {
		t.Fatalf("the line still reads as a failing test:\n%s", table)
	}
}

func TestRunStopsAtTheBudget(t *testing.T) {
	t.Setenv("FAKE_COST", "5")
	_, res := runOne(t, answerCase, answerFiles())
	if res.Pass || res.Stop != StopBudget || res.CostUSD < 3 {
		t.Fatalf("result: %+v", res)
	}
	if c := checkNamed(t, res, "#1 closed"); c.Pass {
		t.Fatalf("the run went on past its budget: %+v", c)
	}
}

// The budget is watched while a pass runs: the developer's session spends
// it, and the reviewer's, still running, is stopped rather than waited for.
func TestRunStopsAtTheBudgetWhileASessionRuns(t *testing.T) {
	t.Setenv("FAKE_COST", "5")
	t.Setenv("FAKE_REVIEW_HANG", "60")
	start := time.Now()
	_, res := runOne(t, answerCase, answerFiles())
	if res.Pass || res.Stop != StopBudget {
		t.Fatalf("result: %+v", res)
	}
	if d := time.Since(start); d > 30*time.Second {
		t.Fatalf("the running review was waited for: the run took %s", d)
	}
}

func TestRunStopsAtTheTimeout(t *testing.T) {
	t.Setenv("FAKE_DEV_HANG", "60")
	start := time.Now()
	_, res := runOne(t, strings.Replace(answerCase, `timeout = "2m"`, `timeout = "3s"`, 1), answerFiles())
	if res.Pass || res.Stop != StopTimeout {
		t.Fatalf("result: %+v", res)
	}
	if d := time.Since(start); d > 45*time.Second {
		t.Fatalf("the hung session was not stopped: the run took %s", d)
	}
}

// An issue the factory gives up on is held for a person: nothing is left
// to wait for, and the case fails on it.
func TestRunStopsWhenTheFactoryGivesUp(t *testing.T) {
	t.Setenv("FAKE_DEV_FAIL", "1")
	_, res := runOne(t, answerCase, answerFiles())
	if res.Pass || res.Stop != StopDone {
		t.Fatalf("result: %+v", res)
	}
	if c := checkNamed(t, res, "#1 closed"); c.Pass || c.Detail != "held for a person: bees:needs-human" {
		t.Fatalf("check: %+v", c)
	}
}

func TestCloses(t *testing.T) {
	got := closes("Closes #1, fixes: #2 and Resolved #3.\nsee #4, reclosed #5")
	if !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("closes: %v", got)
	}
}

// The factory a case starts with: its issues carrying the factory's label
// on top of their own, with their authors and comments, and its mail.
func TestFactorySeedsTheCase(t *testing.T) {
	root := t.TempDir()
	c, err := LoadCase(writeCase(t, root, "seeded", `test = "false"
[[issues]]
number = 3
title = "Untriaged"
author = "kyle"
[[issues.comments]]
body = "more detail"
[[issues]]
number = 4
title = "Ready"
labels = ["bees:ready", "bees"]
[[mail]]
to = "pjm"
subject = "Look at #3"
issue = 3
`, map[string]string{"repo/README.md": "x\n"}))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := filepath.Join(root, "out")
	fx, err := buildFixture(ctx, c, dir)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := testRunner(t)
	f, err := r.factory(ctx, c, builtIn(t), dir, fx)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	s := f.gh.Snapshot()
	i3, _ := s.Issue(3)
	i4, _ := s.Issue(4)
	if names := labelNames(i3); !slices.Equal(names, []string{"bees"}) || i3.Author.Login != "kyle" ||
		len(i3.Comments) != 1 || i3.Comments[0].Author.Login != DefaultAuthor || i3.Comments[0].Body != "more detail" {
		t.Fatalf("issue 3: %+v", i3)
	}
	if names := labelNames(i4); !slices.Equal(names, []string{"bees", "bees:ready"}) || i4.Author.Login != DefaultAuthor {
		t.Fatalf("issue 4: %+v", i4)
	}
	msgs, err := mail.Open(f.store.MailDir()).List(mail.Filter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].To != "project_manager" || msgs[0].From != DefaultAuthor || msgs[0].Subject != "Look at #3" || ghwork.Issue(msgs[0].Work) != 3 {
		t.Fatalf("mail: %+v", msgs)
	}
}

// An approved pull request that no longer merges stays open, marked the
// way GitHub marks it, which is what sends it back to the developer: the
// next pass mails them and moves the issue out of approved.
func TestMergeApprovedLeavesAConflictToTheDeveloper(t *testing.T) {
	t.Setenv("FAKE_DEV_FAIL", "1")
	root := t.TempDir()
	c, err := LoadCase(writeCase(t, root, "conflict", answerCase, answerFiles()))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dir := filepath.Join(root, "out")
	fx, err := buildFixture(ctx, c, dir)
	if err != nil {
		t.Fatal(err)
	}
	commit := func(content string, push ...string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(fx.project, "answer.txt"), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		for _, args := range [][]string{{"add", "-A"}, append(append([]string{}, gitIdentity...), "commit", "-q", "-m", content), append([]string{"push", "-q", "origin"}, push...)} {
			if _, err := git(ctx, fx.project, args...); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := git(ctx, fx.project, "checkout", "-q", "-b", "bees/issue-1"); err != nil {
		t.Fatal(err)
	}
	commit("fixed\n", "bees/issue-1")
	head, _ := git(ctx, fx.project, "rev-parse", "HEAD")
	if _, err := git(ctx, fx.project, "checkout", "-q", "main"); err != nil {
		t.Fatal(err)
	}
	commit("changed on main\n", "main")
	mainBefore, _ := git(ctx, fx.origin, "rev-parse", "main")

	r, _ := testRunner(t)
	f, err := r.factory(ctx, c, builtIn(t), dir, fx)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	approved := []github.Label{{Name: "bees"}, {Name: "bees:approved"}}
	if err := f.gh.Load(fakegh.Seed{
		Issues: []fakegh.SeedIssue{{Issue: github.Issue{Number: 1, Title: "Fix the answer", Labels: approved}}},
		PRs: []fakegh.SeedPR{{PR: github.PR{Number: 2, Title: "Fix the answer", Body: "Closes #1", Labels: approved,
			HeadRefName: "bees/issue-1", BaseRefName: "main", URL: "https://github.com/" + f.cfg.Project.Repo + "/pull/2"}}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := f.mergeApproved(ctx); err == nil {
		t.Fatal("a conflicting merge reported no error")
	}
	s := f.gh.Snapshot()
	p, _ := s.PR(2)
	if p.State != "OPEN" || p.MergedAt != nil || p.Mergeable != github.MergeableConflicting || p.HeadSHA != strings.TrimSpace(head) {
		t.Fatalf("pull request after the failed merge: %+v", p)
	}
	if i, _ := s.Issue(1); i.State != "OPEN" {
		t.Fatalf("issue 1 closed by a merge that did not happen: %+v", i)
	}
	if mainAfter, _ := git(ctx, fx.origin, "rev-parse", "main"); mainAfter != mainBefore {
		t.Fatal("main moved")
	}

	if err := f.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	msgs, err := mail.Open(f.store.MailDir()).List(mail.Filter{To: "developer", From: "orchestrator"})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0].Subject, "PR #2 conflicts with main") {
		t.Fatalf("mail to the developer: %+v", msgs)
	}
	if i, _ := f.gh.Snapshot().Issue(1); github.HasLabel(i.Labels, "bees:approved") {
		t.Fatalf("issue 1 is still approved: %+v", i.Labels)
	}
}
