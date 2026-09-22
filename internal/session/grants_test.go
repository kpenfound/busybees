package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/skills"
)

// ProviderEnv grants each agent the provider environment of its backend
// descriptor, and nothing more: a hand-written map in its place is what
// drifted from the backends when opencode and pi were added.
func TestProviderEnvMatchesBackends(t *testing.T) {
	if len(ProviderEnv) != len(agent.Backends) {
		t.Errorf("ProviderEnv holds %d agents, want the %d backends", len(ProviderEnv), len(agent.Backends))
	}
	for _, b := range agent.Backends {
		if !slices.Equal(ProviderEnv[b.Name], b.ProviderEnv) {
			t.Errorf("ProviderEnv[%s] = %v, want the descriptor's %v", b.Name, ProviderEnv[b.Name], b.ProviderEnv)
		}
	}
}

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

// Another provider's credentials never reach an agent through a toolchain
// entry: GOOGLE_* is claude's and opencode's, not codex's.
func TestCodexDoesNotInheritGoogleCredentials(t *testing.T) {
	t.Setenv("GOOGLE_API_KEY", "google-secret")
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", "/creds.json")
	t.Setenv("GOPATH", "/go")
	r := &Runner{StateDir: t.TempDir()}
	role := config.ResolvedRole{Name: "developer", Agent: agent.AgentCodex}
	req := r.prepare(Request{Profile: ProfileForRole(role), Workspace: vcs.Directory(t.TempDir())}, t.TempDir())
	turn, err := (&agent.Runner{}).Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(turn.Env, "GOPATH=/go") {
		t.Error("GOPATH missing")
	}
	for _, kv := range turn.Env {
		if strings.HasPrefix(kv, "GOOGLE_") {
			t.Errorf("codex inherited %s", kv)
		}
	}
}

// A request its grants refuse leaves no session directory behind.
func TestRefusedSessionLeavesNoDirectory(t *testing.T) {
	r := newRunner(t, fakeClaude(t, `touch "$BEES_SESSION_DIR/ran"`))
	role := config.ResolvedRole{Name: "developer", Agent: agent.AgentCodex, Sandbox: config.SandboxNone}
	profile := ProfileForRole(role)
	profile.VCSAccess = false
	_, err := r.Run(context.Background(), Request{Name: "refused", Profile: profile, Workspace: vcs.Directory(t.TempDir())})
	if !errors.Is(err, agent.ErrUnsupported) {
		t.Fatalf("run: %v, want ErrUnsupported", err)
	}
	if entries, _ := os.ReadDir(r.SessionsDir); len(entries) != 0 {
		t.Fatalf("refused session left %v", entries)
	}
}

// gitWorkspace is a worktree whose shared metadata lives elsewhere.
type gitWorkspace struct{ dir, metadata string }

func (w gitWorkspace) Directory() string { return w.dir }
func (w gitWorkspace) VCS() *vcs.Access  { return &vcs.Access{Mounts: []string{w.metadata}} }

func TestRoleGrantsFollowTheSandbox(t *testing.T) {
	work, state, metadata := t.TempDir(), t.TempDir(), t.TempDir()
	r := &Runner{StateDir: state, AddDirs: []string{state}}
	role := config.ResolvedRole{Name: "developer", Agent: agent.AgentClaude, AllowedTools: []string{"mcp__plugin__search"},
		MCP: map[string]config.MCPServer{"docs": {Command: "docs"}}}
	for mode, want := range map[string][]agent.Mount{
		config.SandboxNone:      {{Path: "/", Access: agent.ReadWrite}},
		config.SandboxClaude:    {{Path: "/", Access: agent.ReadOnly}, {Path: work, Access: agent.ReadWrite}, {Path: state, Access: agent.ReadWrite}},
		config.SandboxContainer: {{Path: work, Access: agent.ReadWrite}, {Path: state, Access: agent.ReadWrite}, {Path: metadata, Access: agent.ReadWrite}},
		config.SandboxSbx:       {{Path: work, Access: agent.ReadWrite}, {Path: state, Access: agent.ReadWrite}, {Path: metadata, Access: agent.ReadWrite}},
	} {
		t.Run(mode, func(t *testing.T) {
			role := role
			role.Sandbox, role.SandboxImage = mode, "image"
			req := r.prepare(Request{Profile: ProfileForRole(role), Workspace: gitWorkspace{dir: work, metadata: metadata}}, t.TempDir())
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
			if _, err := (&agent.Runner{AddDirs: r.AddDirs, MountDirs: []string{state}}).Verify(req); err != nil {
				t.Errorf("busybees policy refused: %v", err)
			}
		})
	}
}

// A container session is granted the sessions directory it is created in
// and, when its role has skills, the skills cache read-only; the busybees
// runner verifies and runs it with nothing else of the host.
func TestContainerGrantsCoverTheRunnersDirectories(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk")
	base := t.TempDir()
	work := t.TempDir()
	r := newRunner(t, fakeClaude(t, `echo '{"type":"result","subtype":"success","result":"ok"}'`))
	r.SessionsDir = filepath.Join(base, "sessions")
	r.StateDir = t.TempDir()
	r.Skills = &skills.Manager{CacheDir: filepath.Join(base, "cache")}
	r.GitHub = config.GitHub{Login: "bot", Token: "t"}
	role := ProfileForRole(config.ResolvedRole{Name: "developer", Sandbox: config.SandboxContainer, SandboxImage: "image", Skills: []string{"https://example.com/skills"}})
	req := r.prepare(Request{Profile: role, Workspace: vcs.Directory(work)}, "")
	want := []agent.Mount{{Path: r.SessionsDir, Access: agent.ReadWrite}, {Path: r.Skills.CacheDir, Access: agent.ReadOnly}}
	for _, m := range want {
		if !slices.Contains(req.Grants.Mounts, m) {
			t.Errorf("mounts %v lack %v", req.Grants.Mounts, m)
		}
	}
	// Neither directory exists yet: Run creates them before verifying.
	// The built-in server cannot start, which stops the session after
	// verification and before any skill is fetched.
	r.BeesBin = filepath.Join(base, "missing", "bees")
	r.ContainerListen = "127.0.0.1:0"
	_, err := r.Run(context.Background(), Request{Name: "boxed", Profile: role, Workspace: vcs.Directory(work)})
	if err == nil || errors.Is(err, agent.ErrNotGranted) || errors.Is(err, agent.ErrUnsupported) || !strings.Contains(err.Error(), "built-in MCP server") {
		t.Fatalf("run: %v, want the server to fail after verification", err)
	}
	for _, d := range []string{r.SessionsDir, r.Skills.CacheDir} {
		if _, err := os.Stat(d); err != nil {
			t.Errorf("%s not created: %v", d, err)
		}
	}
}
