package enforcertest_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/agent/agenttest/enforcertest"
	"github.com/kpenfound/busybees/core/vcs"
)

// layout is a pinned working directory, a writable session directory and a
// directory no grant names.
type layout struct{ work, session, outside string }

func newLayout(t *testing.T) layout {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	l := layout{work: filepath.Join(base, "work"), session: filepath.Join(base, "session"), outside: filepath.Join(base, "outside")}
	for _, dir := range []string{l.work, l.session, l.outside} {
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(l.work, "readme.txt"), filepath.Join(l.outside, "secret.txt")} {
		if err := os.WriteFile(f, []byte("text\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return l
}

func (l layout) grants() agent.Grants {
	return agent.Grants{
		Env:    []string{"PATH"},
		Tools:  []string{"Read", "mcp__tools"},
		Mounts: []agent.Mount{{Path: l.work, Access: agent.ReadOnly}, {Path: l.session, Access: agent.ReadWrite}},
	}
}

func (l layout) request() agent.Request {
	return agent.Request{Name: "review", Workspace: vcs.Directory(l.work), SessionDir: l.session, Profile: agent.Profile{Name: "reviewer"}}
}

// A fake turn of every kind is held to its grants with no process started:
// it reads nothing outside its mounts, writes nothing into a read-only
// working directory, runs no git and has no tool it was not granted.
func TestAFakeTurnIsHeldToItsGrants(t *testing.T) {
	for _, kind := range []string{agent.SandboxNone, agent.SandboxClaude, agent.SandboxContainer} {
		t.Run(kind, func(t *testing.T) {
			l := newLayout(t)
			tried := map[string]error{}
			e := &enforcertest.Enforcer{Sandbox: kind, Image: "image", Agent: func(_ context.Context, turn *enforcertest.Turn) (*agent.Result, error) {
				_, tried["read the working directory"] = turn.ReadFile(filepath.Join(l.work, "readme.txt"))
				_, tried["read outside"] = turn.ReadFile(filepath.Join(l.outside, "secret.txt"))
				tried["write the working directory"] = turn.WriteFile(filepath.Join(l.work, "readme.txt"), []byte("changed\n"))
				tried["create in the working directory"] = turn.WriteFile(filepath.Join(l.work, "new.txt"), nil)
				tried["write the session directory"] = turn.WriteFile(filepath.Join(l.session, "notes.txt"), []byte("notes\n"))
				tried["write outside"] = turn.WriteFile(filepath.Join(l.outside, "new.txt"), nil)
				tried["run git"] = turn.Exec("git")
				tried["run git by its path"] = turn.Exec("/usr/bin/git")
				tried["run ls"] = turn.Exec("ls")
				tried["run a script of the working directory"] = turn.Exec(filepath.Join(l.work, "readme.txt"))
				tried["run a program outside"] = turn.Exec(filepath.Join(l.outside, "secret.txt"))
				tried["use Read"] = turn.UseTool("Read")
				tried["use Bash"] = turn.UseTool("Bash")
				tried["use the granted server"] = turn.UseTool("mcp__tools__done")
				tried["use another server"] = turn.UseTool("mcp__other__done")
				return &agent.Result{ResultText: "reviewed"}, nil
			}}
			s, err := e.Prepare(context.Background(), l.grants())
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Run(context.Background(), l.request())
			if err != nil || res.ResultText != "reviewed" || res.Name != "review" || res.Role != "reviewer" || res.SessionDir != l.session {
				t.Fatalf("run = %+v, %v", res, err)
			}
			// A container runs what its image holds, so a path outside its
			// mounts is the image's and not the host's.
			allowed := []string{"read the working directory", "write the session directory", "run ls", "run a script of the working directory", "use Read", "use the granted server"}
			if kind == agent.SandboxContainer {
				allowed = append(allowed, "run a program outside")
			}
			for what, err := range tried {
				switch {
				case slices.Contains(allowed, what) && err != nil:
					t.Errorf("%s: %v, want it allowed", what, err)
				case !slices.Contains(allowed, what) && !errors.Is(err, fs.ErrPermission):
					t.Errorf("%s: %v, want it refused", what, err)
				}
			}
			if len(tried) != 15 {
				t.Fatalf("the agent tried %d things", len(tried))
			}
			for path, want := range map[string]string{filepath.Join(l.work, "readme.txt"): "text\n", filepath.Join(l.session, "notes.txt"): "notes\n"} {
				if data, err := os.ReadFile(path); err != nil || string(data) != want {
					t.Errorf("%s = %q, %v, want %q", path, data, err, want)
				}
			}
			for _, p := range []string{filepath.Join(l.work, "new.txt"), filepath.Join(l.outside, "new.txt")} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("%s was created", p)
				}
			}

			// The request it ran is the one a real session would have: the
			// session's sandbox and grants, and a host turn confined.
			sessions := e.Sessions()
			if len(sessions) != 1 || len(sessions[0].Requests()) != 1 {
				t.Fatalf("sessions = %v", sessions)
			}
			ran := sessions[0].Requests()[0]
			if ran.Profile.Sandbox != kind || ran.Profile.Confine != (kind != agent.SandboxContainer) || ran.Grants == nil || !slices.Equal(ran.Grants.Mounts, l.grants().Mounts) {
				t.Errorf("the admitted request: profile %+v, grants %+v", ran.Profile, ran.Grants)
			}

			if sessions[0].Released() {
				t.Error("released before Release")
			}
			if err := s.Release(context.Background()); err != nil || !sessions[0].Released() {
				t.Fatalf("release: %v, released %v", err, sessions[0].Released())
			}
			if _, err := s.Run(context.Background(), l.request()); !errors.Is(err, agent.ErrReleased) {
				t.Errorf("run after release: %v, want ErrReleased", err)
			}
		})
	}
}

// The fake refuses the grants and the requests a real enforcer refuses.
func TestTheFakeRefusesWhatARealEnforcerRefuses(t *testing.T) {
	l := newLayout(t)
	ran := 0
	e := &enforcertest.Enforcer{Agent: func(context.Context, *enforcertest.Turn) (*agent.Result, error) {
		ran++
		return nil, nil
	}}
	g := l.grants()
	g.Env = append(g.Env, "GH_TOKEN")
	if _, err := e.Prepare(context.Background(), g); !errors.Is(err, agent.ErrNotGranted) {
		t.Errorf("a VCS variable without VCS: %v, want ErrNotGranted", err)
	}
	root := l.grants()
	root.Mounts = append(root.Mounts, agent.Mount{Path: "/", Access: agent.ReadOnly})
	if _, err := (&enforcertest.Enforcer{Sandbox: agent.SandboxContainer, Image: "image"}).Prepare(context.Background(), root); !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("the host's root for a container: %v, want ErrUnsupported", err)
	}
	if _, err := (&enforcertest.Enforcer{Sandbox: agent.SandboxContainer}).Prepare(context.Background(), l.grants()); !errors.Is(err, agent.ErrUnsupported) {
		t.Errorf("a container with no image: %v, want ErrUnsupported", err)
	}
	unsupported := errors.Join(agent.ErrUnsupported, errors.New("no confiner on this platform"))
	if s, err := (&enforcertest.Enforcer{PrepareErr: unsupported}).Prepare(context.Background(), l.grants()); !errors.Is(err, agent.ErrUnsupported) || s != nil {
		t.Errorf("a platform that cannot enforce: %v, %v", s, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.Prepare(cancelled, l.grants()); !errors.Is(err, context.Canceled) {
		t.Errorf("a cancelled context: %v", err)
	}

	s, err := e.Prepare(context.Background(), l.grants())
	if err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*agent.Request){
		"another sandbox":     func(r *agent.Request) { r.Profile.Sandbox = agent.SandboxClaude },
		"grants of its own":   func(r *agent.Request) { r.Grants = &agent.Grants{Tools: []string{agent.ToolsAll}} },
		"VCS not granted":     func(r *agent.Request) { r.Profile.VCSAccess = true },
		"a tool not granted":  func(r *agent.Request) { r.Profile.AllowedTools = []string{"Bash"} },
		"a directory outside": func(r *agent.Request) { r.Workspace = vcs.Directory(l.outside) },
	} {
		req := l.request()
		change(&req)
		if _, err := s.Run(context.Background(), req); !errors.Is(err, agent.ErrNotGranted) && !errors.Is(err, agent.ErrUnsupported) {
			t.Errorf("%s: %v, want a refusal", name, err)
		}
	}
	if ran != 0 {
		t.Fatalf("a refused request reached the agent %d times", ran)
	}
	// An agent that returns nothing still has a result.
	if res, err := s.Run(context.Background(), l.request()); err != nil || res == nil || ran != 1 {
		t.Fatalf("run = %+v, %v (ran %d)", res, err, ran)
	}
	failed := errors.New("the agent could not start")
	e.Agent = func(context.Context, *enforcertest.Turn) (*agent.Result, error) { return nil, failed }
	if _, err := s.Run(context.Background(), l.request()); !errors.Is(err, failed) {
		t.Errorf("the agent's error: %v", err)
	}
}

// allowAll is a confiner that enforces nothing, for a policy to be read from.
type allowAll struct{}

func (allowAll) Check(agent.Confinement) error                  { return nil }
func (allowAll) Start(cmd *exec.Cmd, _ agent.Confinement) error { return cmd.Start() }

// The fake's policy is the real one's without what a platform adds to it.
func TestTheFakePolicyIsTheRealOneWithoutItsPlatform(t *testing.T) {
	l := newLayout(t)
	docker := agenttest.Docker(t, "image", "SESSION_DIR")
	for kind, e := range map[string]agent.Enforcer{
		agent.SandboxNone:      agent.NewHostNone(agent.Runner{Confiner: allowAll{}, SystemPaths: []agent.Mount{}}),
		agent.SandboxClaude:    agent.NewHostClaude(agent.Runner{Confiner: allowAll{}, SystemPaths: []agent.Mount{}}),
		agent.SandboxContainer: agent.NewContainer(agent.Runner{DockerBin: docker}, "image"),
	} {
		real, err := e.Prepare(context.Background(), l.grants())
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		fake, err := (&enforcertest.Enforcer{Sandbox: kind, Image: "image"}).Prepare(context.Background(), l.grants())
		if err != nil {
			t.Fatalf("%s: %v", kind, err)
		}
		want := real.Policy()
		// The machine's own VCS executables and agents: the platform's part.
		want.System, want.Denied = nil, nil
		if got := fake.Policy(); !samePolicy(got, want) {
			t.Errorf("%s: the fake reports\n%+v\nand the real session\n%+v", kind, got, want)
		}
		_ = real.Release(context.Background())
	}
}

func samePolicy(a, b agent.Policy) bool {
	return a.Sandbox == b.Sandbox && a.Image == b.Image && a.VCS == b.VCS &&
		slices.Equal(a.Env, b.Env) && slices.Equal(a.Tools, b.Tools) && slices.Equal(a.MCPServers, b.MCPServers) &&
		slices.Equal(a.Mounts, b.Mounts) && slices.Equal(a.System, b.System) &&
		slices.Equal(a.DeniedExecutables, b.DeniedExecutables) && slices.Equal(a.Denied, b.Denied) && slices.Equal(a.Binds, b.Binds)
}
