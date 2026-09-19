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

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/internal/config"
)

// A role resolved with agent = "pi" runs as pi with its pi_packages loaded
// after the adapter, its provider's variables and nothing of claude's, and
// the built-in server in the adapter's configuration as `bees mcp serve`
// for the session's role.
func TestPiRoleSession(t *testing.T) {
	bin := agenttest.Script(t, "pi", `
printf '%s\n' "$@" > "$BEES_SESSION_DIR/args.txt"
env > "$BEES_SESSION_DIR/env.txt"
cat > /dev/null
echo '{"type":"session","id":"pi-1"}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"stop","usage":{"cost":{"total":0.5}}}}'
echo '{"type":"turn_end"}'
`)
	t.Setenv("OPENROUTER_API_KEY", "or-key")
	t.Setenv("PI_CODING_AGENT_DIR", "/pi")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "claude-secret")
	r := newRunner(t, "")
	r.PiBin = bin
	role := config.ResolvedRole{Name: config.RoleDeveloper, Agent: config.AgentPi, Model: "openrouter/qwen", MaxTurns: 1, Timeout: time.Minute,
		PiPackages: []string{"npm:@acme/pi-tools"}}
	res, err := r.Run(context.Background(), Request{Name: "developer-issue-1-r1", Profile: ProfileForRole(role), Workspace: vcs.Directory(t.TempDir()), Prompt: "TASK", Env: map[string]string{EnvIssue: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "ok" || res.CostUSD != 0.5 || res.ClaudeID != "pi-1" {
		t.Fatalf("result: %+v", res)
	}
	args, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	for _, want := range []string{"-e\n" + agent.PiMCPAdapter + "\n-e\nnpm:@acme/pi-tools\n", "--name\nbees-developer-issue-1-r1\n", "--model\nopenrouter/qwen\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args lack %q:\n%s", want, args)
		}
	}
	env := strings.Split(string(must(os.ReadFile(filepath.Join(res.SessionDir, "env.txt")))), "\n")
	for _, want := range []string{"OPENROUTER_API_KEY=or-key", "PI_CODING_AGENT_DIR=/pi", EnvPiMCPConfigMode + "=exclusive"} {
		if !slices.Contains(env, want) {
			t.Errorf("env missing %s", want)
		}
	}
	for _, l := range env {
		if strings.HasPrefix(l, "CLAUDE_CODE_OAUTH_TOKEN=") {
			t.Errorf("a pi session inherited %s", l)
		}
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command     string            `json:"command"`
			Args        []string          `json:"args"`
			Env         map[string]string `json:"env"`
			Lifecycle   string            `json:"lifecycle"`
			DirectTools bool              `json:"directTools"`
		} `json:"mcpServers"`
	}
	b := must(os.ReadFile(filepath.Join(res.SessionDir, PiMCPConfigFile)))
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	bees := cfg.MCPServers[config.BuiltinMCPServer]
	if bees.Command != "/usr/local/bin/bees" || !slices.Equal(bees.Args, []string{"mcp", "serve"}) || bees.Lifecycle != "eager" || !bees.DirectTools {
		t.Errorf("built-in server: %+v", bees)
	}
	for k, want := range map[string]string{EnvRole: config.RoleDeveloper, EnvSessionDir: res.SessionDir, EnvIssue: "1", EnvStateDir: "/state"} {
		if bees.Env[k] != want {
			t.Errorf("built-in server %s = %q, want %q", k, bees.Env[k], want)
		}
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}
