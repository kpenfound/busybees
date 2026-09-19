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
	return &Runner{Bees: os.Args[0], ClaudeBin: os.Args[0], CodexBin: os.Args[0], OpenCodeBin: os.Args[0],
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
	if table := rep.Table(); !strings.Contains(table, "FAIL") || !strings.Contains(table, "answer: failed: the test passes") {
		t.Fatalf("table:\n%s", table)
	}
}

// A test that already passes on the fixture proves nothing: no session
// runs. The fixture here is built by setup.sh.
func TestRunRefusesACaseWhoseTestAlreadyPasses(t *testing.T) {
	_, res := runOne(t, answerCase, map[string]string{
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

func TestCloses(t *testing.T) {
	got := closes("Closes #1, fixes: #2 and Resolved #3.\nsee #4, reclosed #5")
	if !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("closes: %v", got)
	}
}
