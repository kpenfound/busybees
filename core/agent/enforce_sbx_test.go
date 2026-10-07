package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/agent/procs"
	"github.com/kpenfound/busybees/core/vcs"
)

// sbxLayout is a sandbox session's directories: a read-only working
// directory, a writable session directory, and one no grant names.
type sbxLayout struct{ work, session, outside string }

func newSbxLayout(t *testing.T) sbxLayout {
	t.Helper()
	base := realTemp(t)
	l := sbxLayout{work: filepath.Join(base, "work"), session: filepath.Join(base, "session"), outside: filepath.Join(base, "outside")}
	for _, dir := range []string{l.work, l.session, l.outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

// grants are the layout's, with the caller's server on the host's port.
func (l sbxLayout) grants(tools []string, port int) Grants {
	return Grants{
		Env:         []string{"PATH", "RUN_DIR", "HOST_TOKEN"},
		Tools:       append(slices.Clone(tools), "mcp__tools"),
		Mounts:      []Mount{{Path: l.work, Access: ReadOnly}, {Path: l.session, Access: ReadWrite}},
		HostServers: []HostServer{{Name: "tools", Port: port}},
	}
}

// turn is a request that leaves the sandbox, its template, its agent and
// its grants to the session, and gives the caller's server at url.
func (l sbxLayout) turn(url string) Request {
	return Request{Name: "review", Workspace: vcs.Directory(l.work), SessionDir: l.session, Prompt: "TASK",
		Env: map[string]string{"RUN_DIR": l.session, "HOST_TOKEN": "host-secret"},
		Profile: Profile{Name: "reviewer", Timeout: time.Minute,
			MCP: map[string]MCPEntry{"tools": {Type: "http", URL: url, BearerTokenEnv: "HOST_TOKEN"}}}}
}

// A sandbox session reports, after Prepare, what sbx is told when a turn
// starts: the granted mounts as the sandbox's workspaces with their access,
// the agent it is created for and from which template, the built-in tools
// the agent is started with, and the one host port it may reach. It says
// what the sandbox cannot do: a VCS executable is denied by name alone.
func TestASandboxSessionEnforcesThePolicyItReports(t *testing.T) {
	l := newSbxLayout(t)
	secret := filepath.Join(l.outside, "secret.txt")
	pinned := filepath.Join(l.work, "readme.txt")
	for _, p := range []string{secret, pinned} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	server := newHostListener(t, "host-secret")
	sbx := agenttest.Sbx(t, "RUN_DIR")
	engine := filepath.Dir(sbx)
	r := Runner{ClaudeBin: hostAgent(t, "claude", sbx, server.port, claudeResult), SbxBin: sbx}
	s, err := NewSbx(r, "", "").Prepare(context.Background(), l.grants([]string{"Read", "Grep"}, server.port))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(engine, "sbx-calls.txt")); err == nil {
		t.Error("Prepare created a sandbox")
	}

	p := s.Policy()
	want := Policy{
		Sandbox:           SandboxSbx,
		Env:               []string{"PATH", "RUN_DIR", "HOST_TOKEN"},
		Tools:             []string{"Read", "Grep"},
		MCPServers:        []string{"tools"},
		Mounts:            []Mount{{Path: l.work, Access: ReadOnly}, {Path: l.session, Access: ReadWrite}},
		DeniedExecutables: VCSExecutables,
		Binds:             []Bind{{Source: l.work, Destination: l.work, Access: ReadOnly}, {Source: l.session, Destination: l.session, Access: ReadWrite}},
		Agent:             AgentClaude,
		HostServers:       []HostServer{{Name: "tools", Port: server.port}},
	}
	if got, want := jsonOf(t, p), jsonOf(t, want); got != want {
		t.Fatalf("policy after Prepare:\n got %s\nwant %s", got, want)
	}
	for name, tc := range map[string]struct{ got, want bool }{
		"reads its working directory":      {p.Reads(pinned), true},
		"reads outside its workspaces":     {p.Reads(secret), false},
		"writes its read-only working dir": {p.Writes(pinned), false},
		"creates in its session directory": {p.Writes(filepath.Join(l.session, "new.txt")), true},
		// sbx binds directories: nothing is laid over the template's git.
		"runs git by its path":            {p.Runs("/usr/bin/git"), true},
		"runs the image's other programs": {p.Runs("/usr/bin/env"), true},
		"has an ungranted tool":           {p.Allows("Bash"), false},
		"has the caller's server":         {p.Allows("mcp__tools__done"), true},
	} {
		if tc.got != tc.want {
			t.Errorf("the policy says the turn %s: %v, want %v", name, tc.got, tc.want)
		}
	}

	res, err := s.Run(context.Background(), l.turn("http://127.0.0.1:"+strconv.Itoa(server.port)+"/mcp"))
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	// The sandbox was created for claude from sbx's own template, with
	// the policy's binds as its workspaces, in path order behind the
	// runner's own primary one: sbx refuses a read-only primary workspace.
	// The runner's is removed with the sandbox.
	create := lines(t, filepath.Join(engine, "sbx-create.txt"))
	name := flagValue(create, "--name")
	primary := create[7]
	if got, want := strings.Join(create[4:], " "), "--skills off claude "+primary+" "+l.session+" "+l.work+":ro"; got != want || primary == l.work {
		t.Errorf("sbx create %v, want the policy's binds (%s)", create, want)
	}
	if _, err := os.Stat(primary); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the primary workspace %s outlived the sandbox: %v", primary, err)
	}
	if slices.Contains(create, "--template") {
		t.Errorf("sbx create names a template: %v", create)
	}
	wantPolicy := []string{
		"policy allow network --sandbox " + name + " localhost:" + strconv.Itoa(server.port),
		"policy rm network --sandbox " + name + " --resource localhost:" + strconv.Itoa(server.port) + " --force",
	}
	if got := lines(t, filepath.Join(engine, "sbx-policy.txt")); !slices.Equal(got, wantPolicy) {
		t.Errorf("sbx policy calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantPolicy, "\n"))
	}
	if got := strings.Join(lines(t, filepath.Join(l.session, "agent-args.txt")), " "); !strings.Contains(got, "--tools Read,Grep ") || !strings.Contains(got, "--strict-mcp-config") {
		t.Errorf("the agent was not started with the granted tools alone: %s", got)
	}
	if got := readMCPConfig(t, l.session)["tools"].URL; got != "http://host.docker.internal:"+strconv.Itoa(server.port)+"/mcp" {
		t.Errorf("the caller's server is given at %s", got)
	}
	if b, err := os.ReadFile(filepath.Join(l.session, "reply.txt")); err != nil || string(b) != "ok" {
		t.Errorf("the caller's server answered %q, %v", b, err)
	}
	if procs.SandboxName(l.session) != "" {
		t.Error("the sandbox's name is left behind after the turn")
	}

	if err := s.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := s.Release(context.Background()); err != nil {
		t.Errorf("a second Release: %v", err)
	}
	if _, err := s.Run(context.Background(), l.turn("http://127.0.0.1:"+strconv.Itoa(server.port)+"/mcp")); !errors.Is(err, ErrReleased) {
		t.Errorf("run after release: %v, want ErrReleased", err)
	}
}

// Claude, codex and opencode each run in a sandbox created for them, from
// the template the session was prepared with or from sbx's own, and reach
// the caller's server with its token whether the profile names it by the
// host's loopback or by host.docker.internal.
func TestASandboxSessionRunsClaudeCodexAndOpenCode(t *testing.T) {
	streams := map[string]string{
		AgentClaude: claudeResult,
		AgentCodex: `echo '{"jsonrpc":"2.0","method":"item/completed","params":{"item":{"type":"agent_message","text":"ok"}}}'
echo '{"jsonrpc":"2.0","method":"turn/completed","params":{"turn":{"status":"completed"}}}'`,
		AgentOpenCode: `echo '{"type":"text","sessionID":"s1","part":{"type":"text","text":"ok"}}'
echo '{"type":"step_finish","sessionID":"s1","part":{"type":"step-finish","reason":"stop","cost":0}}'`,
	}
	for _, agent := range []string{AgentClaude, AgentCodex, AgentOpenCode} {
		for _, template := range []string{"", "acme/" + agent + ":1"} {
			for _, host := range []string{"127.0.0.1", "host.docker.internal"} {
				t.Run(agent+"/"+strings.ReplaceAll(template, "/", "-")+"/"+host, func(t *testing.T) {
					l := newSbxLayout(t)
					server := newHostListener(t, "host-secret")
					sbx := agenttest.Sbx(t, "RUN_DIR")
					bin := hostAgent(t, agent, sbx, server.port, streams[agent])
					r := Runner{ClaudeBin: bin, CodexBin: bin, OpenCodeBin: bin, SbxBin: sbx}
					s, err := NewSbx(r, agent, template).Prepare(context.Background(), l.grants([]string{ToolsAll}, server.port))
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = s.Release(context.Background()) }()
					if p := s.Policy(); p.Agent != agent || p.Image != template {
						t.Errorf("policy: agent %q, template %q", p.Agent, p.Image)
					}
					port := strconv.Itoa(server.port)
					req := l.turn("http://" + host + ":" + port + "/mcp")
					if template != "" {
						// A profile may name what the session holds.
						req.Profile.Agent, req.Profile.SandboxImage = agent, template
					}
					res, err := s.Run(context.Background(), req)
					if err != nil || res.IsError || res.Agent != agent {
						t.Fatalf("run: %+v, %v", res, err)
					}
					create := lines(t, filepath.Join(filepath.Dir(sbx), "sbx-create.txt"))
					switch i := slices.Index(create, "--template"); {
					case template == "" && i >= 0:
						t.Errorf("sbx create names a template: %v", create)
					case template != "" && (i < 0 || create[i+1] != template || create[i+2] != agent):
						t.Errorf("sbx create %v, want the template %s for %s", create, template, agent)
					case template == "" && !slices.Contains(create, agent):
						t.Errorf("sbx create %v, want a sandbox for %s", create, agent)
					}
					inside := "http://host.docker.internal:" + port + "/mcp"
					var given string
					switch agent {
					case AgentClaude:
						given = readMCPConfig(t, l.session)["tools"].URL
					case AgentCodex:
						if args := strings.Join(lines(t, filepath.Join(l.session, "agent-args.txt")), "\n"); strings.Contains(args, `mcp_servers.tools.url="`+inside+`"`) {
							given = inside
						}
					case AgentOpenCode:
						var cfg struct {
							MCP map[string]struct{ URL string } `json:"mcp"`
						}
						b, err := os.ReadFile(filepath.Join(l.session, OpenCodeConfigFile))
						if err != nil {
							t.Fatal(err)
						}
						if err := json.Unmarshal(b, &cfg); err != nil {
							t.Fatal(err)
						}
						given = cfg.MCP["tools"].URL
					}
					if given != inside {
						t.Errorf("%s was given the caller's server at %q, want %s", agent, given, inside)
					}
					if b, err := os.ReadFile(filepath.Join(l.session, "reply.txt")); err != nil || string(b) != "ok" || server.answered.Load() != 1 {
						t.Errorf("the caller's server answered %q, %v", b, err)
					}
				})
			}
		}
	}
}

// What a sandbox session cannot hold, or a turn it was not prepared for, is
// refused before a sandbox is created or an agent started.
func TestASandboxSessionRefusesBeforeLaunch(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for name, tc := range map[string]struct {
		agent, template string
		file            string // a file the sbx fake fails on
		sbx             func(t *testing.T) string
		ctx             context.Context
		grants          func(l sbxLayout, g *Grants)
		want            []error
	}{
		"an agent sbx has no template for": {agent: AgentPi, want: []error{ErrUnsupported}},
		"no sbx that answers":              {file: "fail-version", want: []error{ErrUnsupported}},
		"a real sbx": {sbx: func(t *testing.T) string {
			sh, err := exec.LookPath("sh")
			if err != nil {
				t.Skip(err)
			}
			return sh
		}, want: []error{ErrUnsupported, agentbin.ErrRealAgent}},
		"a cancelled context":         {ctx: cancelled, want: []error{context.Canceled}},
		"the host's root":             {grants: func(_ sbxLayout, g *Grants) { g.Mounts = append(g.Mounts, Mount{Path: "/", Access: ReadOnly}) }, want: []error{ErrUnsupported}},
		"a host server not granted":   {grants: func(_ sbxLayout, g *Grants) { g.Tools = []string{ToolsAll} }, want: []error{ErrNotGranted}},
		"a VCS variable without VCS":  {grants: func(_ sbxLayout, g *Grants) { g.Env = append(g.Env, "GH_TOKEN") }, want: []error{ErrNotGranted}},
		"an engine that is no engine": {grants: func(_ sbxLayout, g *Grants) { g.DaggerEngine = "dagger.sock" }},
	} {
		t.Run(name, func(t *testing.T) {
			l := newSbxLayout(t)
			sbx := agenttest.Sbx(t, "RUN_DIR")
			if tc.sbx != nil {
				sbx = tc.sbx(t)
			}
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(filepath.Dir(sbx), tc.file), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			g := l.grants([]string{ToolsAll}, 8080)
			if tc.grants != nil {
				tc.grants(l, &g)
			}
			ctx := tc.ctx
			if ctx == nil {
				ctx = context.Background()
			}
			s, err := NewSbx(Runner{ClaudeBin: okAgent(t), SbxBin: sbx}, tc.agent, tc.template).Prepare(ctx, g)
			if err == nil || s != nil {
				t.Fatalf("prepared: %v, %v", s, err)
			}
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Errorf("prepare: %v, want %v", err, want)
				}
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(sbx), "sbx-calls.txt")); err == nil {
				t.Error("a sandbox was created")
			}
		})
	}

	for name, tc := range map[string]struct {
		change func(t *testing.T, l sbxLayout, r *Request)
		want   error
	}{
		"another agent":               {func(_ *testing.T, _ sbxLayout, r *Request) { r.Profile.Agent = AgentClaude }, ErrUnsupported},
		"another template":            {func(_ *testing.T, _ sbxLayout, r *Request) { r.Profile.SandboxImage = "acme/other:1" }, ErrUnsupported},
		"a container-use environment": {func(_ *testing.T, _ sbxLayout, r *Request) { r.Profile.ContainerUseEnvironment = "env" }, ErrUnsupported},
		"another sandbox":             {func(_ *testing.T, _ sbxLayout, r *Request) { r.Profile.Sandbox = SandboxContainer }, ErrUnsupported},
		"grants of its own": {func(_ *testing.T, l sbxLayout, r *Request) {
			g := l.grants([]string{ToolsAll}, 8081)
			r.Grants = &g
		}, ErrNotGranted},
		"VCS not granted":      {func(_ *testing.T, _ sbxLayout, r *Request) { r.Profile.VCSAccess = true }, ErrNotGranted},
		"a server not granted": {func(_ *testing.T, _ sbxLayout, r *Request) { r.Profile.AllowedTools = []string{"mcp__other"} }, ErrNotGranted},
		"a directory outside":  {func(_ *testing.T, l sbxLayout, r *Request) { r.Workspace = vcs.Directory(l.outside) }, ErrNotGranted},
		"the host server on a new port": {func(_ *testing.T, _ sbxLayout, r *Request) {
			r.Profile.MCP["tools"] = MCPEntry{URL: "http://127.0.0.1:9090/mcp"}
		}, ErrNotGranted},
		"the Dagger engine": {func(_ *testing.T, _ sbxLayout, r *Request) {
			r.Profile.Dagger = &Dagger{Engine: "tcp://127.0.0.1:1234", Version: "v0.20.5"}
		}, ErrNotGranted},
		"a mount's link pointed elsewhere": {func(t *testing.T, l sbxLayout, _ *Request) {
			link := filepath.Join(filepath.Dir(l.work), "link")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(l.outside, link); err != nil {
				t.Fatal(err)
			}
		}, ErrPolicyChanged},
	} {
		t.Run(name, func(t *testing.T) {
			l := newSbxLayout(t)
			linked := filepath.Join(filepath.Dir(l.work), "linked")
			if err := os.Mkdir(linked, 0o755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(filepath.Dir(l.work), "link")
			if err := os.Symlink(linked, link); err != nil {
				t.Fatal(err)
			}
			sbx := agenttest.Sbx(t, "RUN_DIR")
			bin := hostAgent(t, "codex", sbx, 0, "")
			g := l.grants([]string{ToolsAll}, 8080)
			g.Mounts = append(g.Mounts, Mount{Path: link, Access: ReadOnly})
			s, err := NewSbx(Runner{CodexBin: bin, SbxBin: sbx}, AgentCodex, "").Prepare(context.Background(), g)
			if err != nil {
				t.Fatal(err)
			}
			req := l.turn("http://127.0.0.1:8080/mcp")
			tc.change(t, l, &req)
			if _, err := s.Run(context.Background(), req); !errors.Is(err, tc.want) {
				t.Fatalf("run: %v, want %v", err, tc.want)
			}
			for _, left := range []string{filepath.Join(filepath.Dir(sbx), "sbx-calls.txt"), filepath.Join(l.session, "agent-args.txt")} {
				if _, err := os.Stat(left); err == nil {
					t.Errorf("%s exists after a refusal", filepath.Base(left))
				}
			}
		})
	}
}

// A turn that is cancelled leaves nothing behind: its host port's rule is
// removed, then its sandbox, and the session runs the next turn in a new
// sandbox.
func TestASandboxSessionCleansUpACancelledTurn(t *testing.T) {
	l := newSbxLayout(t)
	sbx := agenttest.Sbx(t, "RUN_DIR")
	engine := filepath.Dir(sbx)
	marker := filepath.Join(l.session, "stop")
	bin := hostAgent(t, "claude", sbx, 0, `if [ ! -f "`+marker+`" ]; then touch "`+marker+`"; sleep 30; fi
`+claudeResult)
	s, err := NewSbx(Runner{ClaudeBin: bin, SbxBin: sbx}, AgentClaude, "").Prepare(context.Background(), l.grants([]string{ToolsAll}, 8080))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Release(context.Background()) }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for ctx.Err() == nil {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	if res, err := s.Run(ctx, l.turn("http://127.0.0.1:8080/mcp")); err == nil {
		t.Fatalf("the cancelled turn: %+v", res)
	}
	if got, want := lines(t, filepath.Join(engine, "sbx-calls.txt")), []string{"create --quiet", "policy allow", "policy rm", "rm --force"}; !slices.Equal(got, want) {
		t.Errorf("sbx calls in order: %v, want %v", got, want)
	}
	if procs.SandboxName(l.session) != "" {
		t.Error("the sandbox's name is left behind")
	}
	if res, err := s.Run(context.Background(), l.turn("http://127.0.0.1:8080/mcp")); err != nil || res.IsError {
		t.Fatalf("the next turn: %+v, %v", res, err)
	}
	names := map[string]bool{}
	for _, line := range lines(t, filepath.Join(engine, "sbx-rm.txt")) {
		if line != "rm" && line != "--force" {
			names[line] = true
		}
	}
	if len(names) != 2 {
		t.Errorf("sandboxes removed: %v, want the two turns' own", names)
	}
}

// A sandbox policy admits the turn it describes and refuses one that is
// confined, or given a bind it was not prepared with.
func TestAdmitHoldsASandboxTurnToItsBinds(t *testing.T) {
	box := Policy{Sandbox: SandboxSbx, Mounts: []Mount{{Path: "/work", Access: ReadOnly}}, DeniedExecutables: []string{"git"},
		Binds: []Bind{{Source: "/work", Destination: "/work", Access: ReadOnly}}}
	turn := func() *Turn {
		return &Turn{Mounts: slices.Clone(box.Mounts), DeniedExecutables: []string{"git"}, Binds: slices.Clone(box.Binds)}
	}
	if err := (&held{policy: box}).admit(turn()); err != nil {
		t.Fatalf("the turn the policy describes: %v", err)
	}
	for name, change := range map[string]func(*Turn){
		"confined":            func(u *Turn) { u.Confinement = &Confinement{} },
		"a bind of its own":   func(u *Turn) { u.Binds = append(u.Binds, Bind{Source: "/etc", Destination: "/etc", Access: ReadOnly}) },
		"a mount bound wider": func(u *Turn) { u.Binds[0].Access = ReadWrite },
		"git not shadowed":    func(u *Turn) { u.DeniedExecutables = nil },
	} {
		u := turn()
		change(u)
		if err := (&held{policy: box}).admit(u); !errors.Is(err, ErrPolicyChanged) {
			t.Errorf("%s: %v, want ErrPolicyChanged", name, err)
		}
	}
}
