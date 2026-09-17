package agent

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/agent/procs"
)

func TestHostMCPTokenStaysInEnvironment(t *testing.T) {
	for _, backend := range []string{AgentClaude, AgentCodex, AgentOpenCode} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			body := `printf '%s\n' "$@" > "$RUN_DIR/args"
env > "$RUN_DIR/agent-env"
cat >/dev/null
`
			switch backend {
			case AgentClaude:
				body += `echo '{"type":"result","subtype":"success"}'`
			case AgentCodex:
				body += `echo '{"type":"turn.completed"}'`
			case AgentOpenCode:
				body += `echo '{"type":"step_finish","part":{"reason":"stop"}}'`
			}
			bin := agenttest.Script(t, backend, body)
			r := Runner{ClaudeBin: bin, CodexBin: bin, OpenCodeBin: bin, DockerBin: agenttest.Docker(t, "image", "RUN_DIR"), ContainerListen: "127.0.0.1:0"}
			req := Request{SessionDir: dir, Workspace: fakeWorkspace{dir: t.TempDir()}, Env: map[string]string{"RUN_DIR": dir, "PRIVATE_MCP_TOKEN": "stale"}, Profile: Profile{Agent: backend, Sandbox: SandboxContainer, SandboxImage: "image"}, HostMCP: &HostMCP{Name: "tools", Entry: MCPEntry{Command: agenttest.MCPServer(t, "RUN_DIR")}, ListenArgs: []string{"mcp", "serve", "--listen"}, TokenEnv: "PRIVATE_MCP_TOKEN", ListeningPrefix: "listening on ", Path: "/mcp"}}
			res, err := r.Run(context.Background(), grantAll(req))
			if err != nil || res.IsError {
				t.Fatalf("run: %+v, %v", res, err)
			}
			var token string
			for _, kv := range lines(t, filepath.Join(dir, "server-env.txt")) {
				if value, ok := strings.CutPrefix(kv, "PRIVATE_MCP_TOKEN="); ok {
					token = value
				}
			}
			if len(token) != 64 {
				t.Fatalf("expected fresh server token, got length %d", len(token))
			}
			for _, file := range []string{"docker-env.txt", "agent-env"} {
				if !slices.Contains(lines(t, filepath.Join(dir, file)), "PRIVATE_MCP_TOKEN="+token) {
					t.Errorf("%s missing the fresh token", file)
				}
			}
			dockerArgs := lines(t, filepath.Join(dir, "docker-args.txt"))
			if !strings.Contains(strings.Join(dockerArgs, " "), "--env PRIVATE_MCP_TOKEN ") {
				t.Error("token not forwarded into container by name")
			}
			files := []string{"args", "docker-args.txt", "server-args.txt"}
			switch backend {
			case AgentClaude:
				files = append(files, "mcp.json")
				if got := readMCPConfig(t, dir)["tools"].Headers["Authorization"]; got != "Bearer ${PRIVATE_MCP_TOKEN}" {
					t.Errorf("Claude header is not an environment reference")
				}
			case AgentCodex:
				if !slices.Contains(lines(t, filepath.Join(dir, "args")), `mcp_servers.tools.bearer_token_env_var="PRIVATE_MCP_TOKEN"`) {
					t.Error("Codex lacks bearer token environment reference")
				}
			case AgentOpenCode:
				files = append(files, "opencode.json")
				data, err := os.ReadFile(filepath.Join(dir, "opencode.json"))
				if err != nil {
					t.Fatal(err)
				}
				var cfg opencodeConfig
				if err := json.Unmarshal(data, &cfg); err != nil {
					t.Fatal(err)
				}
				if cfg.MCP["tools"].Headers["Authorization"] != "Bearer {env:PRIVATE_MCP_TOKEN}" {
					t.Error("OpenCode header is not an environment reference")
				}
			}
			for _, file := range files {
				data, err := os.ReadFile(filepath.Join(dir, file))
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(string(data), token) {
					t.Errorf("%s exposes the bearer token", file)
				}
			}
		})
	}
}

func TestContainerMountsStandaloneSessionDirectory(t *testing.T) {
	for _, mode := range []string{"external", "workdir", "parent", "exact", "symlink-parent", "symlink-session", "symlink-in-workdir"} {
		t.Run(mode, func(t *testing.T) {
			base, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			work := filepath.Join(base, "work")
			session := filepath.Join(base, "sessions", "one")
			r := &Runner{}
			switch mode {
			case "workdir":
				session = filepath.Join(work, "session")
			case "parent":
				r.MountDirs = []string{filepath.Dir(session)}
			case "exact":
				r.MountDirs = []string{session, session}
			case "symlink-in-workdir":
				if err := os.MkdirAll(work, 0o755); err != nil {
					t.Fatal(err)
				}
				external := t.TempDir()
				session = filepath.Join(work, "session")
				if err := os.Symlink(external, session); err != nil {
					t.Fatal(err)
				}
			case "symlink-session":
				link := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(base, link); err != nil {
					t.Fatal(err)
				}
				r.MountDirs = []string{filepath.Dir(session)}
				session = filepath.Join(link, "sessions", "one")
			case "symlink-parent":
				link := filepath.Join(t.TempDir(), "alias")
				if err := os.Symlink(base, link); err != nil {
					t.Fatal(err)
				}
				r.MountDirs = []string{filepath.Join(link, "sessions")}
			}
			for _, dir := range []string{work, session} {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			c := container{r: r, req: Request{Workspace: fakeWorkspace{dir: work}}, sessionDir: session}
			args, err := c.mounts(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			covered := false
			destinations := map[string]bool{}
			direct := 0
			for _, arg := range args {
				if !strings.HasPrefix(arg, "type=bind,") {
					continue
				}
				for _, part := range strings.Split(arg, ",") {
					dst, ok := strings.CutPrefix(part, "destination=")
					if !ok {
						continue
					}
					if destinations[dst] {
						t.Errorf("duplicate mount destination %s", dst)
					}
					destinations[dst] = true
					rel, err := filepath.Rel(dst, session)
					if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
						covered = true
					}
					if dst == session {
						direct++
					}
				}
			}
			if !covered {
				t.Error("session directory has no container mount")
			}
			real, err := filepath.EvalSymlinks(session)
			if err != nil {
				t.Fatal(err)
			}
			realCovered := false
			for dst := range destinations {
				rel, err := filepath.Rel(dst, real)
				if err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
					realCovered = true
				}
			}
			if !realCovered {
				t.Error("session symlink target has no container mount")
			}
			if mode != "external" && mode != "exact" && mode != "symlink-session" && mode != "symlink-in-workdir" && direct != 0 {
				t.Error("redundant session mount despite covering parent")
			}
		})
	}
}

func TestNoMCPCodexOrphanDiscovery(t *testing.T) {
	for _, prefix := range []string{"", "CUSTOM_"} {
		t.Run(prefix, func(t *testing.T) {
			var markers []procs.Markers
			if prefix != "" {
				markers = []procs.Markers{{Codex: procs.CodexMarker(prefix)}}
			}

			sessions := t.TempDir()
			dir := filepath.Join(sessions, "one")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			bin := agenttest.Script(t, "codex", `echo '{"type":"thread.started","thread_id":"test"}'
while :; do sleep 1; done`)
			r := Runner{CodexBin: bin, EnvironmentPrefix: prefix}
			ctx, cancel := context.WithCancel(context.Background())
			ended := make(chan error, 1)
			go func() {
				_, err := r.Run(ctx, grantAll(Request{SessionDir: dir, Workspace: fakeWorkspace{dir: sessions}, Profile: Profile{Agent: AgentCodex}}))
				ended <- err
			}()
			defer func() { cancel(); <-ended }()
			deadline := time.Now().Add(5 * time.Second)
			var pid int
			for time.Now().Before(deadline) {
				if p, ok := procs.FromPIDFile(dir, nil); ok {
					pid = p.PID
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid == 0 {
				t.Fatal("fake Codex did not start")
			}
			found, err := procs.Find(context.Background(), sessions, markers...)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(found, func(p procs.Proc) bool { return p.PID == pid }) {
				t.Error("live no-MCP Codex missing from orphan discovery")
			}
			if _, err := os.Stat(filepath.Join(dir, procs.PIDFile)); err != nil {
				t.Errorf("live Codex PID file removed: %v", err)
			}
			procs.RemovePID(dir)
			found, err = procs.Find(context.Background(), sessions, markers...)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.ContainsFunc(found, func(p procs.Proc) bool { return p.PID == pid }) {
				t.Error("no-MCP Codex missing from process scan without PID file")
			}
			foreign, err := procs.Find(context.Background(), t.TempDir(), markers...)
			if err != nil {
				t.Fatal(err)
			}
			if slices.ContainsFunc(foreign, func(p procs.Proc) bool { return p.PID == pid }) {
				t.Error("Codex matched another caller's sessions")
			}

		})
	}
}
