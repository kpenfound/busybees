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

	"github.com/kpenfound/busybees/internal/config"
)

// fakeClaude writes a shell script standing in for the claude binary.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nset -e\n" + body
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newRunner(t *testing.T, bin string) *Runner {
	t.Helper()
	return &Runner{ClaudeBin: bin, SessionsDir: t.TempDir(), StateDir: "/state", Repo: "a/b", Label: "bees", BeesBin: "/usr/local/bin/bees"}
}

func TestRunSuccess(t *testing.T) {
	bin := fakeClaude(t, `
# record what we were given
printf '%s' "$@" > "$BEES_SESSION_DIR/args.txt"
cat > "$BEES_SESSION_DIR/stdin.txt"
env > "$BEES_SESSION_DIR/env.txt"
echo '{"type":"system","subtype":"init"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"abc","num_turns":4,"total_cost_usd":0.25}'
printf '{"status":"pr-opened","pr":12,"note":"hi"}' > "$BEES_SESSION_DIR/outcome.json"
`)
	r := newRunner(t, bin)
	role := config.ResolvedRole{Name: "developer", Model: "opus", FallbackModel: "sonnet", MaxTurns: 10, Timeout: time.Minute,
		MCP:   map[string]config.MCPServer{"x": {Command: "srv", Env: map[string]string{"K": "$HOME"}}},
		Shell: "/bin/sh", Env: map[string]string{"FACTORY_TOKEN": "abc", "CACHE": "$HOME/cache"}}
	res, err := r.Run(context.Background(), Request{Name: "t1", Role: role, WorkDir: t.TempDir(), SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{"EXTRA": "1", EnvIssue: "12"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "all done" || res.NumTurns != 4 || res.CostUSD != 0.25 || res.ClaudeID != "abc" {
		t.Fatalf("result: %+v", res)
	}
	if !res.HasOutcome || res.Outcome.Status != "pr-opened" || res.Outcome.PR != 12 {
		t.Fatalf("outcome: %+v", res.Outcome)
	}
	args, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	for _, want := range []string{"-p", "--model", "opus", "--fallback-model", "sonnet", "--max-turns", "10", "--dangerously-skip-permissions", "--mcp-config", "--strict-mcp-config", "--append-system-prompt-file"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %s: %s", want, args)
		}
	}
	stdin, _ := os.ReadFile(filepath.Join(res.SessionDir, "stdin.txt"))
	if string(stdin) != "TASK" {
		t.Fatalf("stdin: %q", stdin)
	}
	env, _ := os.ReadFile(filepath.Join(res.SessionDir, "env.txt"))
	for _, want := range []string{"BEES_ROLE=developer", "BEES_STATE_DIR=/state", "BEES_REPO=a/b", "EXTRA=1", "BEES_BIN=/usr/local/bin/bees", "SHELL=/bin/sh", "FACTORY_TOKEN=abc", "CACHE=" + os.Getenv("HOME") + "/cache"} {
		if !strings.Contains(string(env), want) {
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

// A session started from inside another session must not inherit its BEES_*
// variables: only the ones the runner sets for it are visible.
func TestEnvDropsInheritedBeesVariables(t *testing.T) {
	bin := fakeClaude(t, `
env > "$BEES_SESSION_DIR/env.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute}
	run := func(t *testing.T, roleEnv, reqEnv map[string]string) []string {
		t.Helper()
		t.Setenv(EnvPR, "54")
		t.Setenv(EnvIssue, "99")
		t.Setenv(EnvStateDir, "/inherited")
		r := role
		r.Env = roleEnv
		res, err := newRunner(t, bin).Run(context.Background(), Request{Name: "t", Role: r, WorkDir: t.TempDir(), Env: reqEnv})
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(res.SessionDir, "env.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
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

func TestRunErrorAndNoOutcome(t *testing.T) {
	bin := fakeClaude(t, `
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"ran out","num_turns":10}'
exit 1
`)
	r := newRunner(t, bin)
	res, err := r.Run(context.Background(), Request{Name: "t2", Role: config.ResolvedRole{Name: "qa", Model: "opus", MaxTurns: 10}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.ErrorSubtype != "error_max_turns" || res.ExitCode != 1 || res.HasOutcome {
		t.Fatalf("result: %+v", res)
	}
}

func TestRunTimeout(t *testing.T) {
	bin := fakeClaude(t, `sleep 5`)
	r := newRunner(t, bin)
	res, err := r.Run(context.Background(), Request{Name: "t3", Role: config.ResolvedRole{Name: "qa", Model: "opus", MaxTurns: 1, Timeout: 200 * time.Millisecond}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || !res.IsError || res.ErrorSubtype != "timeout" {
		t.Fatalf("result: %+v", res)
	}
}

func TestOutcomeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := ReadOutcome(dir); ok || err != nil {
		t.Fatal("expected no outcome")
	}
	if err := WriteOutcome(dir, Outcome{Status: "approved", Note: "n"}); err != nil {
		t.Fatal(err)
	}
	o, ok, err := ReadOutcome(dir)
	if err != nil || !ok || o.Status != "approved" {
		t.Fatalf("%+v %v %v", o, ok, err)
	}
}

// readMCPConfig parses the mcp.json a session was given.
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

// TestRunAlwaysWritesMCPConfig covers a role with no configured servers: it
// still gets the built-in one, so --mcp-config is unconditional.
func TestRunAlwaysWritesMCPConfig(t *testing.T) {
	bin := fakeClaude(t, `
printf '%s' "$@" > "$BEES_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	role := config.ResolvedRole{Name: "reviewer", Model: "opus", MaxTurns: 10, Timeout: time.Minute}
	res, err := r.Run(context.Background(), Request{Name: "t5", Role: role, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	for _, want := range []string{"--mcp-config", "--strict-mcp-config"} {
		if !strings.Contains(string(args), want) {
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
