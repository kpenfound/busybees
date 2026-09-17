package session

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/internal/config"
)

// A session's environment is its role's grants: the host's shell, toolchain,
// provider and VCS variables, the role's own env, and nothing else.
func TestSessionEnvironmentIsTheRolesGrants(t *testing.T) {
	bin := fakeClaude(t, `
env > "$BEES_SESSION_DIR/env.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	t.Setenv("DELIVERY_WEBHOOK_SECRET", "delivery")
	t.Setenv("OPENAI_API_KEY", "other-provider")
	t.Setenv("ANTHROPIC_API_KEY", "own-provider")
	t.Setenv("SSH_AUTH_SOCK", "/agent.sock")
	t.Setenv("GOFLAGS", "-mod=mod")
	t.Setenv("PASSED_ON", "by-role")
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute, Env: map[string]string{"PASSED_ON": "$PASSED_ON"}}
	res, err := newRunner(t, bin).Run(context.Background(), Request{Name: "t", Profile: ProfileForRole(role), Workspace: vcs.Directory(t.TempDir())})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(res.SessionDir, "env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Split(string(b), "\n")
	for _, want := range []string{"ANTHROPIC_API_KEY=own-provider", "SSH_AUTH_SOCK=/agent.sock", "GOFLAGS=-mod=mod", "PASSED_ON=by-role"} {
		if !slices.Contains(env, want) {
			t.Errorf("missing %s", want)
		}
	}
	for _, name := range []string{"DELIVERY_WEBHOOK_SECRET", "OPENAI_API_KEY"} {
		for _, l := range env {
			if strings.HasPrefix(l, name+"=") {
				t.Errorf("inherited ungranted %s", l)
			}
		}
	}
}

func TestRoleGrantsFollowTheSandbox(t *testing.T) {
	work, state := t.TempDir(), t.TempDir()
	r := &Runner{StateDir: state, AddDirs: []string{state}}
	role := config.ResolvedRole{Name: "developer", Agent: agent.AgentClaude, AllowedTools: []string{"mcp__plugin__search"},
		MCP: map[string]config.MCPServer{"docs": {Command: "docs"}}}
	for mode, want := range map[string][]agent.Mount{
		config.SandboxNone:      {{Path: "/", Access: agent.ReadWrite}},
		config.SandboxClaude:    {{Path: "/", Access: agent.ReadOnly}, {Path: work, Access: agent.ReadWrite}, {Path: state, Access: agent.ReadWrite}},
		config.SandboxContainer: {{Path: work, Access: agent.ReadWrite}, {Path: state, Access: agent.ReadWrite}},
	} {
		t.Run(mode, func(t *testing.T) {
			role := role
			role.Sandbox, role.SandboxImage = mode, "image"
			req := r.prepare(Request{Profile: ProfileForRole(role), Workspace: vcs.Directory(work)}, t.TempDir())
			g := req.Grants
			if !slices.Equal(g.Mounts, want) {
				t.Errorf("mounts = %v, want %v", g.Mounts, want)
			}
			if !g.VCS {
				t.Error("role not granted VCS")
			}
			if want := []string{agent.ToolsAll, "mcp__bees", "mcp__docs", "mcp__plugin"}; !slices.Equal(g.Tools, want) {
				t.Errorf("tools = %v, want %v", g.Tools, want)
			}
			if _, err := (&agent.Runner{AddDirs: r.AddDirs}).Verify(req); err != nil {
				t.Errorf("busybees policy refused: %v", err)
			}
		})
	}
}
