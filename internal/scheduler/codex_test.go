package scheduler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/session"
)

// argsOf reads the command line a fake session recorded, one argument per
// line.
func argsOf(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// resultOf reads the result.json the runner wrote for a session.
func resultOf(t *testing.T, dir string) session.Result {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, session.ResultFile))
	if err != nil {
		t.Fatal(err)
	}
	var res session.Result
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// A developer configured agent = "codex" walks the whole loop — develop,
// review, develop again, review, approved — with every developer session
// started as `codex exec` and every other role's as `claude -p`: the agent
// is resolved per role, and the scheduler reads what each session did off
// the stream its own agent writes. The fake codex is the test binary, told
// apart from the fake claude by the `exec` the runner starts codex with.
func TestACodexDeveloperWalksTheWholeLoop(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.developer]\nagent = \"codex\"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	want := []string{"bees:in-progress", "bees:review", "bees:in-progress", "bees:review", "bees:approved"}
	if got := h.gh.history[1]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("issue 1 label history: %v", got)
	}
	if !github.HasLabel(h.gh.prs[fakePR].Labels, "bees:approved") {
		t.Fatalf("PR labels: %v", h.gh.prs[fakePR].Labels)
	}
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("no escalation expected: %v", h.gh.comments[1])
	}
	dev, rev := h.sessions(config.RoleDeveloper), h.sessions(config.RoleReviewer)
	if len(dev) != 2 || len(rev) != 2 {
		t.Fatalf("sessions: %d developer, %d reviewer, want 2 and 2", len(dev), len(rev))
	}
	for _, d := range dev {
		args := argsOf(t, d)
		if len(args) < 3 || args[1] != "exec" || args[2] != "--json" {
			t.Errorf("developer session %s did not run codex exec --json: %q", filepath.Base(d), args)
		}
		if strings.Contains(strings.Join(args, " "), "--dangerously-skip-permissions") {
			t.Errorf("developer session %s carries claude's flags: %q", filepath.Base(d), args)
		}
		// The built-in MCP server reached codex as configuration, with the
		// session's directory in its env.
		if !strings.Contains(strings.Join(args, "\n"), `mcp_servers.bees.env.BEES_SESSION_DIR="`+d+`"`) {
			t.Errorf("developer session %s: the built-in server is not in codex's overrides:\n%s", filepath.Base(d), strings.Join(args, "\n"))
		}
		res := resultOf(t, d)
		if res.CostKnown || res.CostUSD != 0 || res.NumTurns != 2 || res.ClaudeID != "fake-thread" || res.IsError {
			t.Errorf("developer session %s result: %+v", filepath.Base(d), res)
		}
	}
	for _, r := range rev {
		args := argsOf(t, r)
		if len(args) < 2 || args[1] != "-p" {
			t.Errorf("reviewer session %s did not run claude -p: %q", filepath.Base(r), args)
		}
		if res := resultOf(t, r); !res.CostKnown {
			t.Errorf("reviewer session %s (claude) has no known cost: %+v", filepath.Base(r), res)
		}
	}
	// The second developer session received the reviewer's mail, as it does
	// under claude: the mailbox is not the agent's.
	prompt, _ := os.ReadFile(filepath.Join(dev[1], "prompt.md"))
	if !strings.Contains(string(prompt), "please add tests") {
		t.Fatalf("second developer prompt:\n%s", prompt)
	}
	// The ledger carries the codex sessions with no cost and the claude ones
	// with theirs.
	ledger, err := h.store.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 4 {
		t.Fatalf("ledger has %d entries, want 4:\n%+v", len(ledger), ledger)
	}
	for _, e := range ledger {
		switch e.Role {
		case config.RoleDeveloper:
			if e.CostUSD != 0 || e.Turns != 2 {
				t.Errorf("codex ledger entry: %+v", e)
			}
		case config.RoleReviewer:
			if e.CostUSD != 0.01 || e.Turns != 2 {
				t.Errorf("claude ledger entry: %+v", e)
			}
		}
	}
	// The summary lines read the same whichever agent ran.
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

// The agent is [global]'s too: with agent = "codex" there, every role's
// session is a codex one.
func TestAGlobalCodexAgentRunsEveryRoleAsCodex(t *testing.T) {
	h := newHarness(t, strings.Replace(baseTOML, "[scheduler]", "[global]\nagent = \"codex\"\n[scheduler]", 1)+"\n[roles.developer]\nenabled = false\n[roles.reviewer]\nenabled = false\n")
	h.gh.issues[3] = &github.Issue{Number: 3, Title: "Spec me", Body: "vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{config.RoleProjectManager, config.RoleProductManager, config.RoleQA} {
		dirs := h.sessions(role)
		if len(dirs) != 1 {
			t.Errorf("%s sessions: %d, want 1", role, len(dirs))
			continue
		}
		if args := argsOf(t, dirs[0]); len(args) < 2 || args[1] != "exec" {
			t.Errorf("%s did not run codex: %q", role, args)
		}
		if res := resultOf(t, dirs[0]); res.CostKnown || res.HasOutcome != true {
			t.Errorf("%s result: %+v", role, res)
		}
	}
}
