package agent

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

// hostListener is a caller's own MCP server on the host's loopback: it
// answers "ok" to a request that bears token and 401 to any other, and
// counts both. The test owns it: nothing the runner does closes it.
type hostListener struct {
	port     int
	answered atomic.Int32
	refused  atomic.Int32
}

func newHostListener(t *testing.T, token string) *hostListener {
	t.Helper()
	l := &hostListener{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			l.refused.Add(1)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		l.answered.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	if l.port, err = strconv.Atoi(u.Port()); err != nil {
		t.Fatal(err)
	}
	return l
}

// hostAgent is a fake agent for a sandbox session with host servers. It
// records its arguments and, when the sbx fake has one, its policy file
// as the agent starts, in the directory RUN_DIR names; calls the server on
// port with the token in HOST_TOKEN when port is not zero, the way the
// sandbox's proxy would carry a call to host.docker.internal there, and
// records the answer; then prints stream. A session that reaches the
// server with the token it was given leaves "ok" in reply.txt.
func hostAgent(t *testing.T, name, sbx string, port int, stream string) string {
	t.Helper()
	call := ""
	if port != 0 {
		if _, err := exec.LookPath("curl"); err != nil {
			t.Skip("curl is needed to call the host server:", err)
		}
		call = `curl -sS -H "Authorization: Bearer $HOST_TOKEN" "http://127.0.0.1:` + strconv.Itoa(port) + `/mcp" > "$RUN_DIR/reply.txt"` + "\n"
	}
	policy := filepath.Join(filepath.Dir(sbx), "sbx-policy.txt")
	return agenttest.Script(t, name, `cat >/dev/null
printf '%s\n' "$@" > "$RUN_DIR/agent-args.txt"
[ ! -f "`+policy+`" ] || cp "`+policy+`" "$RUN_DIR/policy-while-running.txt"
`+call+stream+"\n")
}

// claudeResult is what the fake claude prints.
const claudeResult = `echo '{"type":"result","subtype":"success","result":"ok"}'`

// hostServerRequest is a sandbox request for a session in dir with the
// given MCP entries, granted every one of them, the host servers named,
// RUN_DIR and HOST_TOKEN, and every built-in tool.
func hostServerRequest(dir string, entries map[string]MCPEntry, servers []HostServer) Request {
	g := &Grants{Env: []string{"PATH", "RUN_DIR", "HOST_TOKEN"}, Tools: []string{ToolsAll},
		Mounts: []Mount{{Path: dir, Access: ReadWrite}}, HostServers: servers}
	for name := range entries {
		g.Tools = append(g.Tools, "mcp__"+name)
	}
	for _, h := range servers {
		if _, ok := entries[h.Name]; !ok {
			g.Tools = append(g.Tools, "mcp__"+h.Name)
		}
	}
	return Request{Name: "hosted", SessionDir: dir, Workspace: fakeWorkspace{dir: dir}, Grants: g, Prompt: "TASK",
		Env:     map[string]string{"RUN_DIR": dir, "HOST_TOKEN": "host-secret"},
		Profile: Profile{Name: "builder", Sandbox: SandboxSbx, Timeout: time.Minute, MCP: entries}}
}

// A sandbox session reaches the caller's own servers on the host's
// loopback that it is granted, with the token the caller gave it: each
// entry is given at host.docker.internal, the created sandbox alone is
// allowed each granted port once, while the agent runs and not after, and
// the caller's listener is left running. An entry that is not granted is
// passed as it is and allowed nothing, and a granted server the profile
// does not name is allowed nothing either.
func TestSandboxReachesGrantedHostServers(t *testing.T) {
	alpha, beta := newHostListener(t, "host-secret"), newHostListener(t, "other-secret")
	sbx := agenttest.Sbx(t, "RUN_DIR")
	engine := filepath.Dir(sbx)
	dir := realTempDir(t)
	a, b := strconv.Itoa(alpha.port), strconv.Itoa(beta.port)
	entries := map[string]MCPEntry{
		"alpha": {Type: "http", URL: "http://127.0.0.1:" + a + "/mcp", BearerTokenEnv: "HOST_TOKEN"},
		"beta":  {Type: "http", URL: "http://host.docker.internal:" + b + "/mcp"},
		// Another server on beta's port: one rule for both.
		"gamma":  {Type: "http", URL: "http://localhost:" + b + "/other"},
		"remote": {Type: "http", URL: "https://mcp.example.com/mcp"},
		// On the host and not granted: the machine's policy decides.
		"stray": {Type: "http", URL: "http://host.docker.internal:9/mcp"},
	}
	servers := []HostServer{{Name: "alpha", Port: alpha.port}, {Name: "beta", Port: beta.port}, {Name: "gamma", Port: beta.port}, {Name: "delta", Port: 7}}
	r := Runner{ClaudeBin: hostAgent(t, "claude", sbx, alpha.port, claudeResult), SbxBin: sbx}
	res, err := r.Run(context.Background(), hostServerRequest(dir, entries, servers))
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}

	name := flagValue(lines(t, filepath.Join(engine, "sbx-create.txt")), "--name")
	wantPolicy := []string{
		"policy allow network --sandbox " + name + " localhost:" + a,
		"policy allow network --sandbox " + name + " localhost:" + b,
	}
	if got := lines(t, filepath.Join(dir, "policy-while-running.txt")); !slices.Equal(got, wantPolicy) {
		t.Errorf("rules while the agent ran:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantPolicy, "\n"))
	}
	wantPolicy = append(wantPolicy,
		"policy rm network --sandbox "+name+" --resource localhost:"+a+" --force",
		"policy rm network --sandbox "+name+" --resource localhost:"+b+" --force")
	if got := lines(t, filepath.Join(engine, "sbx-policy.txt")); !slices.Equal(got, wantPolicy) {
		t.Errorf("sbx policy calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantPolicy, "\n"))
	}
	if got, want := lines(t, filepath.Join(engine, "sbx-calls.txt")), []string{"create --quiet", "policy allow", "policy allow", "policy rm", "policy rm", "rm --force"}; !slices.Equal(got, want) {
		t.Errorf("sbx calls in order: %v, want %v", got, want)
	}

	cfg := readMCPConfig(t, dir)
	for server, want := range map[string]string{
		"alpha":  "http://host.docker.internal:" + a + "/mcp",
		"beta":   "http://host.docker.internal:" + b + "/mcp",
		"gamma":  "http://host.docker.internal:" + b + "/other",
		"remote": "https://mcp.example.com/mcp",
		"stray":  "http://host.docker.internal:9/mcp",
	} {
		if got := cfg[server].URL; got != want {
			t.Errorf("%s is given at %s, want %s", server, got, want)
		}
	}
	if _, ok := cfg["delta"]; ok {
		t.Error("a granted server the profile does not name was given to the session")
	}
	if got := cfg["alpha"].Headers["Authorization"]; got != "Bearer ${HOST_TOKEN}" {
		t.Errorf("alpha's authorization: %q, want a reference to HOST_TOKEN", got)
	}
	// The token reached the agent by name, and the server answered it.
	execArgs := lines(t, filepath.Join(dir, "sbx-exec-args.txt"))
	if !slices.Contains(execArgs, "HOST_TOKEN") || slices.ContainsFunc(execArgs, func(a string) bool { return strings.Contains(a, "host-secret") }) {
		t.Errorf("sbx exec args: %v, want HOST_TOKEN by name and never its value", execArgs)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "reply.txt")); err != nil || string(b) != "ok" || alpha.answered.Load() != 1 || alpha.refused.Load() != 0 {
		t.Errorf("the host server's answer: %q, %v (answered %d, refused %d)", b, err, alpha.answered.Load(), alpha.refused.Load())
	}
	// The listener is the caller's, and still serves.
	resp, err := http.Get("http://127.0.0.1:" + b + "/mcp")
	if err != nil {
		t.Fatalf("the caller's listener after the session: %v", err)
	}
	_ = resp.Body.Close()
}

// A host server the runner cannot scope a rule to exactly is refused
// before a sandbox is created, and so is a grant it cannot enforce.
func TestSandboxRefusesAHostServerItCannotScope(t *testing.T) {
	for name, tc := range map[string]struct {
		entry   MCPEntry
		servers []HostServer
		change  func(*Request)
		want    error
	}{
		"another port":          {MCPEntry{URL: "http://127.0.0.1:9090/mcp"}, []HostServer{{Name: "tools", Port: 8080}}, nil, ErrNotGranted},
		"off the host":          {MCPEntry{URL: "https://tools.example.com:8080/mcp"}, []HostServer{{Name: "tools", Port: 8080}}, nil, ErrNotGranted},
		"run inside":            {MCPEntry{Command: "tools"}, []HostServer{{Name: "tools", Port: 8080}}, nil, ErrUnsupported},
		"no port":               {MCPEntry{URL: "http://localhost/mcp"}, []HostServer{{Name: "tools", Port: 80}}, nil, ErrUnsupported},
		"not HTTP":              {MCPEntry{URL: "ws://127.0.0.1:8080/mcp"}, []HostServer{{Name: "tools", Port: 8080}}, nil, ErrUnsupported},
		"a server not granted":  {MCPEntry{URL: "http://127.0.0.1:8080/mcp"}, []HostServer{{Name: "tools", Port: 8080}}, func(r *Request) { r.Grants.Tools = []string{ToolsAll} }, ErrNotGranted},
		"granted twice":         {MCPEntry{URL: "http://127.0.0.1:8080/mcp"}, []HostServer{{Name: "tools", Port: 8080}, {Name: "tools", Port: 8080}}, nil, nil},
		"not a port":            {MCPEntry{URL: "http://127.0.0.1:8080/mcp"}, []HostServer{{Name: "tools", Port: 70000}}, nil, nil},
		"in a container":        {MCPEntry{URL: "http://127.0.0.1:8080/mcp"}, []HostServer{{Name: "tools", Port: 8080}}, func(r *Request) { r.Profile.Sandbox, r.Profile.SandboxImage = SandboxContainer, "image" }, ErrUnsupported},
		"on the host, unboxed":  {MCPEntry{URL: "http://127.0.0.1:8080/mcp"}, []HostServer{{Name: "tools", Port: 8080}}, func(r *Request) { r.Profile.Sandbox = SandboxNone }, ErrUnsupported},
		"the loopback, wrongly": {MCPEntry{URL: "http://[::1]:8081/mcp"}, []HostServer{{Name: "tools", Port: 8080}}, nil, ErrNotGranted},
	} {
		t.Run(name, func(t *testing.T) {
			sbx := agenttest.Sbx(t, "RUN_DIR")
			dir := realTempDir(t)
			r := Runner{ClaudeBin: hostAgent(t, "claude", sbx, 0, claudeResult), SbxBin: sbx, DockerBin: agenttest.Docker(t, "image", "RUN_DIR")}
			req := hostServerRequest(dir, map[string]MCPEntry{"tools": tc.entry}, tc.servers)
			if tc.change != nil {
				tc.change(&req)
			}
			_, err := r.Run(context.Background(), req)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) {
				t.Fatalf("run: %v, want a refusal (%v)", err, tc.want)
			}
			for _, left := range []string{filepath.Join(filepath.Dir(sbx), "sbx-calls.txt"), filepath.Join(dir, "agent-args.txt"), filepath.Join(dir, "docker-args.txt")} {
				if _, err := os.Stat(left); err == nil {
					t.Errorf("%s exists after a refusal: %v", filepath.Base(left), err)
				}
			}
		})
	}
}

// A host server's rule is added after the sandbox is created and before
// anything runs in it, and removed before the sandbox, whichever way the
// session ends: it succeeds, the agent fails, a probe the runner runs
// before the agent fails, sbx refuses the rule, or the session is
// cancelled. A rule sbx refuses is never removed, and one sbx cannot
// remove is logged and the sandbox removed all the same.
func TestSandboxHostServerRulesAreRemovedFirst(t *testing.T) {
	failing := `echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"broke"}'
exit 1`
	for name, tc := range map[string]struct {
		agent  string // "codex" runs a held codex turn, whose probe fails
		stream string
		file   string // a file the sbx fake fails on
		cancel bool
		ok     bool
		calls  []string
		logged string
	}{
		"success":         {stream: claudeResult, ok: true, calls: []string{"create --quiet", "policy allow", "policy allow", "policy rm", "policy rm", "rm --force"}},
		"agent failure":   {stream: failing, calls: []string{"create --quiet", "policy allow", "policy allow", "policy rm", "policy rm", "rm --force"}},
		"probe failure":   {agent: AgentCodex, calls: []string{"create --quiet", "policy allow", "policy allow", "policy rm", "policy rm", "rm --force"}},
		"rule refused":    {stream: claudeResult, file: "fail-policy", calls: []string{"create --quiet", "policy allow", "rm --force"}},
		"cancelled":       {stream: "sleep 30", cancel: true, calls: []string{"create --quiet", "policy allow", "policy allow", "policy rm", "policy rm", "rm --force"}},
		"removal refused": {stream: claudeResult, file: "fail-policy-rm", ok: true, calls: []string{"create --quiet", "policy allow", "policy allow", "policy rm", "policy rm", "rm --force"}, logged: "policy store is locked"},
	} {
		t.Run(name, func(t *testing.T) {
			sbx := agenttest.Sbx(t, "RUN_DIR")
			engine := filepath.Dir(sbx)
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(engine, tc.file), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			dir := realTempDir(t)
			var log bytes.Buffer
			r := Runner{SbxBin: sbx, Logger: slog.New(slog.NewTextHandler(&log, nil))}
			entries := map[string]MCPEntry{"alpha": {URL: "http://127.0.0.1:4101/mcp"}, "beta": {URL: "http://localhost:4102/mcp"}}
			req := hostServerRequest(dir, entries, []HostServer{{Name: "alpha", Port: 4101}, {Name: "beta", Port: 4102}})
			if tc.agent == AgentCodex {
				// A held codex turn asks codex for its features in the
				// sandbox before it starts; this one cannot answer.
				r.CodexBin = agenttest.Script(t, "codex", `[ ! -f "`+engine+`/sbx-policy.txt" ] || cp "`+engine+`/sbx-policy.txt" "$RUN_DIR/policy-while-probing.txt"
echo "codex: no such feature store" >&2
exit 1
`)
				req.Profile.Agent = AgentCodex
				req.Grants.Tools = []string{"apply_patch", "mcp__alpha", "mcp__beta"}
			} else {
				r.ClaudeBin = hostAgent(t, "claude", sbx, 0, tc.stream)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancel {
				go func() {
					for ctx.Err() == nil {
						if _, err := os.Stat(filepath.Join(dir, "agent-args.txt")); err == nil {
							cancel()
							return
						}
						time.Sleep(10 * time.Millisecond)
					}
				}()
			}
			res, err := r.Run(ctx, req)
			switch {
			case tc.ok && (err != nil || res.IsError):
				t.Fatalf("run: %+v, %v", res, err)
			case !tc.ok && err == nil && !res.IsError:
				t.Fatalf("run succeeded: %+v", res)
			}
			if got := lines(t, filepath.Join(engine, "sbx-calls.txt")); !slices.Equal(got, tc.calls) {
				t.Errorf("sbx calls in order: %v, want %v", got, tc.calls)
			}
			if tc.agent == AgentCodex {
				if got := lines(t, filepath.Join(dir, "policy-while-probing.txt")); len(got) != 2 {
					t.Errorf("rules while codex was asked for its features: %v, want both", got)
				}
			}
			if tc.file == "fail-policy" {
				if _, err := os.Stat(filepath.Join(dir, "agent-args.txt")); err == nil {
					t.Error("the agent ran without its rule")
				}
			}
			if tc.logged != "" && !strings.Contains(log.String(), tc.logged) {
				t.Errorf("log does not say %q:\n%s", tc.logged, log.String())
			}
		})
	}
}
