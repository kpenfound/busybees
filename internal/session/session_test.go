package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
	res, err := r.Run(context.Background(), Request{Name: "t1", Role: role, WorkDir: t.TempDir(), SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{"EXTRA": "1"}})
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
	var mcp struct {
		MCPServers map[string]struct {
			Command string            `json:"command"`
			Env     map[string]string `json:"env"`
		} `json:"mcpServers"`
	}
	b, _ := os.ReadFile(filepath.Join(res.SessionDir, "mcp.json"))
	if err := json.Unmarshal(b, &mcp); err != nil || mcp.MCPServers["x"].Command != "srv" || mcp.MCPServers["x"].Env["K"] != os.Getenv("HOME") {
		t.Fatalf("mcp config: %s %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "transcript.jsonl")); err != nil {
		t.Fatal("transcript missing")
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "result.json")); err != nil {
		t.Fatal("result.json missing")
	}
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
