package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/workspace"
)

// A developer configured agent = "opencode" walks the whole loop — develop,
// review, develop again, review, approved — with every developer session
// started as `opencode run` and every other role's as `claude -p`. The fake
// opencode is the test binary, told apart from the fake claude by the `run`
// the runner starts opencode with. The built-in MCP server reaches it
// through the configuration file OPENCODE_CONFIG names, written in the
// session directory and never in the worktree: the fake developer commits
// with `git add .`, so a file written there would be on the branch.
func TestAnOpenCodeDeveloperWalksTheWholeLoop(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.developer]\nagent = \"opencode\"\nmodel = \"ollama/llama3\"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	want := []string{"bees:in-progress", "bees:review", "bees:in-progress", "bees:review", "bees:approved"}
	if got := h.gh.History[1]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("issue 1 label history: %v", got)
	}
	if !github.HasLabel(h.gh.PRs[fakePR].Labels, "bees:approved") {
		t.Fatalf("PR labels: %v", h.gh.PRs[fakePR].Labels)
	}
	if len(h.gh.Comments[1]) != 0 {
		t.Fatalf("no escalation expected: %v", h.gh.Comments[1])
	}
	dev, rev := h.sessions(config.RoleDeveloper), h.sessions(config.RoleReviewer)
	if len(dev) != 2 || len(rev) != 2 {
		t.Fatalf("sessions: %d developer, %d reviewer, want 2 and 2", len(dev), len(rev))
	}
	for i, d := range dev {
		args := argsOf(t, d)
		if len(args) < 4 || args[1] != "run" || args[2] != "--format" || args[3] != "json" {
			t.Errorf("developer session %s did not run opencode run --format json: %q", filepath.Base(d), args)
		}
		if !slices.Contains(args, "--auto") {
			t.Errorf("developer session %s runs without --auto: %q", filepath.Base(d), args)
		}
		if j := slices.Index(args, "--model"); j < 0 || args[j+1] != "ollama/llama3" {
			t.Errorf("developer session %s: the role's model did not reach opencode: %q", filepath.Base(d), args)
		}
		for _, gone := range []string{"--dangerously-skip-permissions", "--mcp-config", "--resume", "exec"} {
			if slices.Contains(args, gone) {
				t.Errorf("developer session %s carries %s: %q", filepath.Base(d), gone, args)
			}
		}
		// The built-in MCP server reached opencode through the file
		// OPENCODE_CONFIG names, in the session directory.
		b, _ := os.ReadFile(filepath.Join(d, "opencode-config.txt"))
		if string(b) != filepath.Join(d, session.OpenCodeConfigFile) {
			t.Errorf("developer session %s: OPENCODE_CONFIG = %q, want the session's own file", filepath.Base(d), b)
		}
		cfg, _ := os.ReadFile(filepath.Join(d, session.OpenCodeConfigFile))
		for _, want := range []string{`"bees": {`, `"type": "local"`, `"mcp",`, `"serve"`, `"BEES_SESSION_DIR": "` + d + `"`, `"instructions": [`, filepath.Join(d, "system-prompt.md")} {
			if !strings.Contains(string(cfg), want) {
				t.Errorf("developer session %s: opencode.json lacks %q:\n%s", filepath.Base(d), want, cfg)
			}
		}
		res := resultOf(t, d)
		if !res.CostKnown || res.CostUSD != 0.01 || res.NumTurns != 2 || res.IsError {
			t.Errorf("developer session %s result: %+v", filepath.Base(d), res)
		}
		// Round 2 continues round 1's opencode session, as it would
		// claude's conversation.
		wantID := "sid-developer-issue-1-r" + string(rune('1'+i))
		if res.ClaudeID != wantID {
			t.Errorf("developer session %s: session id %q, want %q", filepath.Base(d), res.ClaudeID, wantID)
		}
		if j := slices.Index(args, "--session"); (i == 0) != (j < 0) || (j >= 0 && args[j+1] != "sid-developer-issue-1-r1") {
			t.Errorf("developer session %s: --session in %q", filepath.Base(d), args)
		}
	}
	for _, r := range rev {
		args := argsOf(t, r)
		if len(args) < 2 || args[1] != "-p" {
			t.Errorf("reviewer session %s did not run claude -p: %q", filepath.Base(r), args)
		}
	}
	// Nothing opencode-specific reached the branch the developer pushed.
	if _, err := workspace.Git(ctx, h.clone, "fetch", "-q", "origin"); err != nil {
		t.Fatal(err)
	}
	files, err := workspace.Git(ctx, h.clone, "ls-tree", "-r", "--name-only", "origin/bees/issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files, "opencode.json") || !strings.Contains(files, "work-1.txt") {
		t.Errorf("the branch's files:\n%s", files)
	}
	// The ledger carries the opencode sessions with the cost their steps
	// added up to, and the first round's review pipeline once:
	// the second round verifies and runs none.
	ledger, err := h.store.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 5 {
		t.Fatalf("ledger has %d entries, want 4 sessions and 1 review:\n%+v", len(ledger), ledger)
	}
	for _, e := range ledger {
		if e.Outcome == "reviewed" {
			continue
		}
		if e.CostUSD != 0.01 || e.Turns != 2 {
			t.Errorf("ledger entry: %+v", e)
		}
	}
	for _, line := range []string{
		"✓ developer issue #1 → PR #101 opened",
		"✗ reviewer PR #101 changes requested",
		"✓ developer issue #1 → PR #101 updated",
		"✓ reviewer PR #101 approved",
	} {
		if !strings.Contains(h.logs.String(), line) {
			t.Errorf("missing summary %q in:\n%s", line, h.logs.String())
		}
	}
}
