package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/agentcore/agent/agenttest"
	"github.com/kpenfound/agentcore/vcs"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
)

func newRunner(t *testing.T, bin string) *Runner {
	t.Helper()
	return &Runner{ClaudeBin: bin, SessionsDir: t.TempDir(), StateDir: "/state", Repo: "a/b", Label: "bees", BeesBin: "/usr/local/bin/bees"}
}

// TestRunSuccess drives a full session through Runner.Run and checks every
// artifact a caller can observe: the claude invocation (args, stdin, env),
// the written mcp.json, the system prompt file, the parsed result and
// outcome, and that the transcript and result files exist. The session fixture
// records the invocation and emits a scripted answer; it does not confirm the
// real claude CLI accepts these flags or emits this exact stream-json shape.
func TestRunSuccess(t *testing.T) {
	fixture := agenttest.Session(t, "claude", agenttest.Turn{
		ResultText: "all done", SessionID: "abc", NumTurns: 4, CostUSD: 0.25,
		Outcome: &agenttest.SessionOutcome{Status: "pr-opened", Work: ghwork.New(0, 12), Note: "hi"},
	})
	r := newRunner(t, fixture.Bin)
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 10, Timeout: time.Minute,
		MCP:   map[string]config.MCPServer{"x": {Command: "srv", Env: map[string]string{"K": "$HOME"}}},
		Shell: "/bin/sh", Env: map[string]string{"FACTORY_TOKEN": "abc", "CACHE": "$HOME/cache"}}
	res, err := r.Run(context.Background(), Request{Name: "t1", Profile: ProfileForRole(role), Workspace: vcs.Directory(t.TempDir()), SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{"EXTRA": "1", EnvIssue: "12"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "all done" || res.NumTurns != 4 || res.CostUSD != 0.25 || res.ClaudeID != "abc" {
		t.Fatalf("result: %+v", res)
	}
	if !res.HasOutcome || res.Outcome.Status != "pr-opened" || ghwork.PR(res.Outcome.Work) != 12 {
		t.Fatalf("outcome: %+v", res.Outcome)
	}
	invocation := fixture.Record(t).Sessions[0]
	args := invocation.Args
	for _, want := range []string{"-p", "--model", "opus", "--max-turns", "10", "--dangerously-skip-permissions", "--mcp-config", "--strict-mcp-config", "--append-system-prompt-file"} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %s: %s", want, args)
		}
	}
	if invocation.Prompt != "TASK" {
		t.Fatalf("stdin: %q", invocation.Prompt)
	}
	env := invocation.Environment
	for _, want := range []string{"BEES_ROLE=developer", "BEES_STATE_DIR=/state", "BEES_REPO=a/b", "EXTRA=1", "BEES_BIN=/usr/local/bin/bees", "SHELL=/bin/sh", "FACTORY_TOKEN=abc", "CACHE=" + os.Getenv("HOME") + "/cache"} {
		key, value, _ := strings.Cut(want, "=")
		if env[key] != value {
			t.Errorf("env missing %s", want)
		}
	}
	sys, _ := os.ReadFile(filepath.Join(res.SessionDir, "system-prompt.md"))
	if string(sys) != "SYS" {
		t.Fatalf("system prompt: %q", sys)
	}
	servers := readMCPConfig(t, res.SessionDir)
	if servers["x"].Command != "srv" || servers["x"].Env["K"] != os.Getenv("HOME") {
		t.Fatalf("configured server: %+v", servers["x"])
	}
	// The built-in server sits next to the configured one.
	builtin := servers[config.BuiltinMCPServer]
	if builtin.Command != "/usr/local/bin/bees" || strings.Join(builtin.Args, " ") != "mcp serve" {
		t.Fatalf("built-in server: %+v", builtin)
	}
	for k, want := range map[string]string{
		"BEES_ROLE": "developer", "BEES_STATE_DIR": "/state", "BEES_SESSION_DIR": res.SessionDir,
		"BEES_REPO": "a/b", "BEES_LABEL": "bees", "BEES_ISSUE": "12", "BEES_BIN": "/usr/local/bin/bees",
	} {
		if builtin.Env[k] != want {
			t.Errorf("built-in server env %s = %q, want %q", k, builtin.Env[k], want)
		}
	}
	if _, ok := builtin.Env["EXTRA"]; ok {
		t.Errorf("built-in server env leaked a non-BEES variable: %v", builtin.Env)
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "transcript.jsonl")); err != nil {
		t.Fatal("transcript missing")
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "result.json")); err != nil {
		t.Fatal("result.json missing")
	}
}

// TestEnvDropsInheritedBeesVariables checks that a session never inherits
// BEES_* state from the process that started it (only a session's own
// request and configured role env reach it). The fixture records its
// environment; it does not confirm the real claude binary would launch
// correctly with whatever is or isn't in that environment.
func TestEnvDropsInheritedBeesVariables(t *testing.T) {
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute}
	run := func(t *testing.T, roleEnv, reqEnv map[string]string) []string {
		t.Helper()
		t.Setenv(EnvPR, "54")
		t.Setenv(EnvIssue, "99")
		t.Setenv(EnvStateDir, "/inherited")
		r := role
		r.Env = roleEnv
		fixture := agenttest.Session(t, "claude", agenttest.Turn{ResultText: "ok"})
		_, err := newRunner(t, fixture.Bin).Run(context.Background(), Request{Name: "t", Profile: ProfileForRole(r), Workspace: vcs.Directory(t.TempDir()), Env: reqEnv})
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for key, value := range fixture.Record(t).Sessions[0].Environment {
			lines = append(lines, key+"="+value)
		}
		return lines
	}
	has := func(lines []string, want string) bool { return slices.Contains(lines, want) }
	// lookup returns every line setting name, so a failure names the offender
	// instead of dumping the whole environment.
	lookup := func(lines []string, name string) []string {
		var got []string
		for _, l := range lines {
			if strings.HasPrefix(l, name+"=") {
				got = append(got, l)
			}
		}
		return got
	}

	t.Run("absent when the session has none", func(t *testing.T) {
		lines := run(t, nil, nil)
		for _, name := range []string{EnvPR, EnvIssue, EnvBranch} {
			if got := lookup(lines, name); got != nil {
				t.Errorf("%s leaked from the parent process: %v", name, got)
			}
		}
		// The runner's own variables are still there, with its values.
		if !has(lines, EnvStateDir+"=/state") || !has(lines, EnvRole+"=developer") {
			t.Errorf("runner variables missing: %v", lines)
		}
	})

	t.Run("the session's own value wins", func(t *testing.T) {
		lines := run(t, nil, map[string]string{EnvPR: "12"})
		if got := lookup(lines, EnvPR); len(got) != 1 || got[0] != EnvPR+"=12" {
			t.Errorf("%s lines = %v, want exactly [%s=12]", EnvPR, got, EnvPR)
		}
		if got := lookup(lines, EnvIssue); got != nil {
			t.Errorf("%s leaked from the parent process: %v", EnvIssue, got)
		}
	})

	// The strip is namespace-wide, so it also drops operator knobs like
	// BEES_CLAUDE_BIN that are not session state. Configured role env is the
	// documented way to give them to sessions: it is applied after the strip.
	t.Run("configured role env reaches the session", func(t *testing.T) {
		t.Setenv("BEES_CACHE_DIR", "/inherited-cache")
		lines := run(t, map[string]string{"BEES_CACHE_DIR": "/configured-cache"}, nil)
		if got := lookup(lines, "BEES_CACHE_DIR"); len(got) != 1 || got[0] != "BEES_CACHE_DIR=/configured-cache" {
			t.Errorf("BEES_CACHE_DIR lines = %v, want exactly [BEES_CACHE_DIR=/configured-cache]", got)
		}
	})
}

func readMCPConfig(t *testing.T, sessionDir string) map[string]MCPEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sessionDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		MCPServers map[string]MCPEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatalf("mcp.json: %v: %s", err, b)
	}
	return file.MCPServers
}

// TestRunAlwaysWritesMCPConfig checks that mcp.json and its strict flags are
// present even for a role with no [mcp] servers of its own: the built-in
// bees server is always configured. The fixture never starts that server,
// so this leaves unverified whether the real claude CLI actually starts the
// built-in server from it.
func TestRunAlwaysWritesMCPConfig(t *testing.T) {
	fixture := agenttest.Session(t, "claude", agenttest.Turn{ResultText: "ok"})
	r := newRunner(t, fixture.Bin)
	role := config.ResolvedRole{Name: "reviewer", Model: "opus", MaxTurns: 10, Timeout: time.Minute}
	res, err := r.Run(context.Background(), Request{Name: "t5", Profile: ProfileForRole(role), Workspace: vcs.Directory(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	invocation := fixture.Record(t).Sessions[0]
	args := invocation.Args
	for _, want := range []string{"--mcp-config", "--strict-mcp-config"} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %s: %s", want, args)
		}
	}
	servers := readMCPConfig(t, res.SessionDir)
	if len(servers) != 1 {
		t.Fatalf("servers: %+v", servers)
	}
	builtin := servers[config.BuiltinMCPServer]
	if builtin.Command != "/usr/local/bin/bees" || strings.Join(builtin.Args, " ") != "mcp serve" {
		t.Fatalf("built-in server: %+v", builtin)
	}
	for _, k := range []string{EnvStateDir, EnvSessionDir, EnvRole} {
		if builtin.Env[k] == "" {
			t.Errorf("built-in server env is missing %s: %v", k, builtin.Env)
		}
	}
}

func TestConfiguredSkillsNeedAPreparer(t *testing.T) {
	r := newRunner(t, agenttest.Session(t, "claude", agenttest.Turn{}).Bin)
	_, err := r.Run(context.Background(), Request{Workspace: vcs.Directory(t.TempDir()), Profile: ProfileForRole(config.ResolvedRole{Name: "developer", Skills: []string{"https://example.com/skills"}})})
	if err == nil || !strings.Contains(err.Error(), "no skills manager") {
		t.Fatalf("missing preparer: %v", err)
	}
}

// ProfileForRole carries the role's fallback chain, each profile of it under
// the role's own settings, so a retry that moves down the chain
// (ops.SelectProfile) keeps the tools, turn limit and timeout it had; a
// claude session with a claude fallback is told its model.
func TestProfileForRoleCarriesTheFallbackChain(t *testing.T) {
	cfg, err := config.Parse(`version = 5
[profiles.main]
agent = "claude"
model = "opus"
fallback = "cheap"
[profiles.cheap]
agent = "codex"
model = "gpt"
fallback = "last"
[profiles.last]
agent = "opencode"
model = "ollama/x"
[roles.developer]
profile = "main"
max_turns = 9
allowed_tools = ["Bash"]
`, filepath.Join(t.TempDir(), "bees.toml"))
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.Role(config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	p := ProfileForRole(role)
	if p.Agent != "claude" || p.Fallback == nil || p.Fallback.Agent != "codex" || p.Fallback.Model != "gpt" || p.Fallback.Fallback == nil || p.Fallback.Fallback.Agent != "opencode" || p.Fallback.Fallback.Fallback != nil {
		t.Fatalf("chain: %+v", p)
	}
	for _, step := range []Profile{p, *p.Fallback, *p.Fallback.Fallback} {
		if step.MaxTurns != 9 || !slices.Equal(step.AllowedTools, []string{"Bash"}) || step.Name != config.RoleDeveloper {
			t.Errorf("a step of the chain lost the role's settings: %+v", step)
		}
	}
	if ProfileForRole(config.ResolvedRole{Name: "qa"}).Fallback != nil {
		t.Error("a role without a fallback has a chain")
	}
}
