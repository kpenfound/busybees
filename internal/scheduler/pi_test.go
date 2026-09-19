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

// A developer configured agent = "pi" walks the whole loop — develop,
// review, develop again, review, approved — with every developer session
// started as `pi -p --mode json` and every other role's as `claude -p`. The
// fake pi is the test binary, told apart from the fake claude by the
// `--mode` after `-p`. The built-in MCP server reaches it through
// pi-mcp-adapter, loaded ahead of the role's pi packages, and the adapter's
// configuration file, written in the session directory and never in the
// worktree: the fake developer commits with `git add .`, so a file written
// there would be on the branch.
func TestAPiDeveloperWalksTheWholeLoop(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.developer]\nagent = \"pi\"\nmodel = \"openrouter/qwen\"\npi_packages = [\"npm:@acme/pi-tools\"]\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
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
		if len(args) < 5 || strings.Join(args[1:5], " ") != "-p --mode json --no-extensions" {
			t.Errorf("developer session %s did not run pi -p --mode json --no-extensions: %q", filepath.Base(d), args)
		}
		var ext []string
		for j, a := range args {
			if a == "-e" && j+1 < len(args) {
				ext = append(ext, args[j+1])
			}
		}
		if want := []string{config.PiMCPAdapter, "npm:@acme/pi-tools"}; !slices.Equal(ext, want) {
			t.Errorf("developer session %s loads %q, want %q", filepath.Base(d), ext, want)
		}
		if j := slices.Index(args, "--model"); j < 0 || args[j+1] != "openrouter/qwen" {
			t.Errorf("developer session %s: the role's model did not reach pi: %q", filepath.Base(d), args)
		}
		for _, gone := range []string{"--dangerously-skip-permissions", "--output-format", "--resume", "--strict-mcp-config", "run", "exec"} {
			if slices.Contains(args, gone) {
				t.Errorf("developer session %s carries %s: %q", filepath.Base(d), gone, args)
			}
		}
		// The built-in MCP server reached the adapter through the file
		// --mcp-config names, in the session directory, read alone.
		if j := slices.Index(args, "--mcp-config"); j < 0 || args[j+1] != filepath.Join(d, session.PiMCPConfigFile) {
			t.Errorf("developer session %s: --mcp-config in %q, want the session's own file", filepath.Base(d), args)
		}
		if b, _ := os.ReadFile(filepath.Join(d, "pi-mcp-mode.txt")); string(b) != "exclusive" {
			t.Errorf("developer session %s: %s = %q", filepath.Base(d), session.EnvPiMCPConfigMode, b)
		}
		cfg, _ := os.ReadFile(filepath.Join(d, session.PiMCPConfigFile))
		for _, want := range []string{`"bees": {`, `"command": "`, `"mcp",`, `"serve"`, `"BEES_ROLE": "developer"`, `"BEES_SESSION_DIR": "` + d + `"`, `"lifecycle": "eager"`, `"directTools": true`} {
			if !strings.Contains(string(cfg), want) {
				t.Errorf("developer session %s: %s lacks %q:\n%s", filepath.Base(d), session.PiMCPConfigFile, want, cfg)
			}
		}
		res := resultOf(t, d)
		if !res.CostKnown || res.CostUSD != 0.01 || res.NumTurns != 2 || res.IsError {
			t.Errorf("developer session %s result: %+v", filepath.Base(d), res)
		}
		// Round 2 continues round 1's pi session, as it would claude's
		// conversation.
		wantID := "sid-developer-issue-1-r" + string(rune('1'+i))
		if res.ClaudeID != wantID {
			t.Errorf("developer session %s: session id %q, want %q", filepath.Base(d), res.ClaudeID, wantID)
		}
		if j := slices.Index(args, "--session-id"); (i == 0) != (j < 0) || (j >= 0 && args[j+1] != "sid-developer-issue-1-r1") {
			t.Errorf("developer session %s: --session-id in %q", filepath.Base(d), args)
		}
	}
	for _, r := range rev {
		args := argsOf(t, r)
		if len(args) < 3 || args[1] != "-p" || args[2] == "--mode" {
			t.Errorf("reviewer session %s did not run claude -p: %q", filepath.Base(r), args)
		}
	}
	// Nothing pi-specific reached the branch the developer pushed.
	if _, err := workspace.Git(ctx, h.clone, "fetch", "-q", "origin"); err != nil {
		t.Fatal(err)
	}
	files, err := workspace.Git(ctx, h.clone, "ls-tree", "-r", "--name-only", "origin/bees/issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(files, session.PiMCPConfigFile) || !strings.Contains(files, "work-1.txt") {
		t.Errorf("the branch's files:\n%s", files)
	}
	// The ledger carries the pi sessions with the cost their responses
	// added up to, and the first round's review pipeline once.
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
}
