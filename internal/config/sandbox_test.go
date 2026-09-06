package config

import (
	"errors"
	"strings"
	"testing"
)

// sandbox resolves like model: the role's value if it set one, else the
// global one, else none.
func TestSandboxMerge(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
version = 1
[project]
repo = "acme/widgets"

[global]
sandbox = "claude"

[roles.developer]
sandbox = "container"
`))
	if err != nil {
		t.Fatal(err)
	}
	for role, want := range map[string]string{
		"developer":       SandboxContainer,
		"reviewer":        SandboxClaude,
		"product_manager": SandboxClaude,
	} {
		r, err := cfg.Role(role)
		if err != nil {
			t.Fatal(err)
		}
		if r.Sandbox != want {
			t.Errorf("roles.%s sandbox: got %q, want %q", role, r.Sandbox, want)
		}
	}
}

// With nothing configured anywhere every role runs unboxed, which is what
// bees has always done.
func TestSandboxDefaultsToNone(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range Roles {
		r, err := cfg.Role(role)
		if err != nil {
			t.Fatal(err)
		}
		if r.Sandbox != SandboxNone {
			t.Errorf("roles.%s sandbox: got %q, want %q", role, r.Sandbox, SandboxNone)
		}
	}
}

// Every mode of SandboxModes loads, under [global] and under a role: whether
// a mode can be run here is CheckSandbox's question, not the parser's, and a
// person must be able to write down the mode they want before it works.
func TestEverySandboxModeLoads(t *testing.T) {
	for _, mode := range SandboxModes {
		for _, scope := range []string{"global", "roles.developer"} {
			body := "version = 1\n[project]\nrepo = \"a/b\"\n[" + scope + "]\nsandbox = \"" + mode + "\"\n"
			cfg, err := Load(writeConfig(t, body))
			if err != nil {
				t.Fatalf("%s.sandbox = %q: %v", scope, mode, err)
			}
			dev, err := cfg.Role(RoleDeveloper)
			if err != nil {
				t.Fatal(err)
			}
			if dev.Sandbox != mode {
				t.Errorf("%s.sandbox = %q resolved to %q", scope, mode, dev.Sandbox)
			}
		}
	}
}

// An unknown mode is a load error naming the key and every mode it takes.
func TestUnknownSandboxModeIsALoadError(t *testing.T) {
	for scope, want := range map[string]string{
		"global":          "global.sandbox must be one of none, claude, container",
		"roles.developer": "roles.developer.sandbox must be one of none, claude, container",
	} {
		_, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n["+scope+"]\nsandbox = \"jail\"\n"))
		if err == nil {
			t.Fatalf("%s.sandbox = \"jail\": expected an error", scope)
		}
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// implemented narrows sandboxImplemented for the rest of the test, so a
// test about an unimplemented mode keeps its fixture whatever bees learns to
// run.
func implemented(t *testing.T, modes ...string) {
	t.Helper()
	old := sandboxImplemented
	t.Cleanup(func() { sandboxImplemented = old })
	sandboxImplemented = modes
}

// CheckSandbox is what `bees run` asks before its first poll: a role asking
// for a box bees cannot build stops the factory, naming the role and the
// modes there are, rather than letting that role run unboxed.
func TestCheckSandboxRefusesAModeThatIsNotImplemented(t *testing.T) {
	implemented(t, SandboxNone)
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[roles.developer]\nsandbox = \"container\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	err = cfg.CheckSandbox()
	if err == nil {
		t.Fatal("a container developer started without a container")
	}
	for _, want := range []string{"roles.developer", "container", "none"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// The check is about the sessions that will run, so a role that is out of the
// rotation is not one of them.
func TestCheckSandboxIgnoresADisabledRole(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[roles.qa]\nenabled = false\nsandbox = \"container\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.CheckSandbox(); err != nil {
		t.Errorf("a disabled role's sandbox stopped the factory: %v", err)
	}
}

// The default configuration starts.
func TestCheckSandboxPassesWithoutTheKey(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.CheckSandbox(); err != nil {
		t.Errorf("the default configuration does not start: %v", err)
	}
}

// A ResolvedRole built by hand — every test that runs a session makes one —
// has no mode set, and an empty mode is none rather than a refusal.
func TestCheckSandboxModeTakesAnEmptyMode(t *testing.T) {
	if err := CheckSandboxMode(""); err != nil {
		t.Errorf("an unset mode was refused: %v", err)
	}
	if err := CheckSandboxMode(SandboxNone); err != nil {
		t.Errorf("none was refused: %v", err)
	}
	if err := CheckSandboxMode(SandboxContainer); err != nil {
		t.Errorf("container was refused: %v", err)
	}
	if err := CheckSandboxMode(SandboxClaude); err == nil {
		t.Error("claude reported itself implemented")
	}
}

// `bees config show` prints the resolved mode per role.
func TestViewShowsTheResolvedSandbox(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
version = 1
[project]
repo = "a/b"

[global]
sandbox = "claude"

[roles.developer]
sandbox = "container"
`))
	if err != nil {
		t.Fatal(err)
	}
	v, err := cfg.View([]string{RoleDeveloper, RoleQA})
	if err != nil {
		t.Fatal(err)
	}
	if got := v.Roles[RoleDeveloper].Sandbox; got != SandboxContainer {
		t.Errorf("developer sandbox in the view: got %q, want %q", got, SandboxContainer)
	}
	if got := v.Roles[RoleQA].Sandbox; got != SandboxClaude {
		t.Errorf("qa sandbox in the view: got %q, want %q", got, SandboxClaude)
	}
}

// sandbox_image resolves like sandbox: the role's value if it set one, else
// the global one, else nothing; `bees config show` prints it per role.
func TestSandboxImageMerges(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
version = 1
[project]
repo = "a/b"

[global]
sandbox_image = "ghcr.io/acme/bees:latest"

[roles.qa]
sandbox_image = "ghcr.io/acme/widgets-qa:latest"
`))
	if err != nil {
		t.Fatal(err)
	}
	v, err := cfg.View([]string{RoleDeveloper, RoleQA})
	if err != nil {
		t.Fatal(err)
	}
	for role, want := range map[string]string{RoleDeveloper: "ghcr.io/acme/bees:latest", RoleQA: "ghcr.io/acme/widgets-qa:latest"} {
		r, err := cfg.Role(role)
		if err != nil {
			t.Fatal(err)
		}
		if r.SandboxImage != want {
			t.Errorf("roles.%s sandbox_image: got %q, want %q", role, r.SandboxImage, want)
		}
		if got := v.Roles[role].SandboxImage; got != want {
			t.Errorf("roles.%s sandbox_image in the view: got %q, want %q", role, got, want)
		}
	}
	plain, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	dev, _ := plain.Role(RoleDeveloper)
	if dev.SandboxImage != "" {
		t.Errorf("an unset sandbox_image resolved to %q", dev.SandboxImage)
	}
}

// An image reference with a space in it is a command line, not an image.
func TestSandboxImageWithSpacesIsALoadError(t *testing.T) {
	_, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[roles.developer]\nsandbox_image = \"docker run x\"\n"))
	if err == nil || !strings.Contains(err.Error(), "roles.developer.sandbox_image") {
		t.Fatalf("expected a load error naming the key, got %v", err)
	}
}

// fakeMachine describes a machine to the container checks for the rest of
// the test: which programs are on its PATH, its environment, and what the
// engine answers. Every engine call is recorded.
type fakeMachine struct {
	onPath []string
	env    map[string]string
	engine func(args ...string) ([]byte, error)
	calls  []string
}

func machine(t *testing.T, m *fakeMachine) *fakeMachine {
	t.Helper()
	oldLook, oldEnv, oldEngine := lookPath, getenv, engineCommand
	t.Cleanup(func() { lookPath, getenv, engineCommand = oldLook, oldEnv, oldEngine })
	lookPath = func(name string) (string, error) {
		for _, p := range m.onPath {
			if p == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
	getenv = func(name string) string { return m.env[name] }
	engineCommand = func(args ...string) ([]byte, error) {
		m.calls = append(m.calls, strings.Join(args, " "))
		if m.engine == nil {
			return []byte("ok"), nil
		}
		return m.engine(args...)
	}
	return m
}

// A container role needs three things from its configuration, and each
// refusal names the one that is missing and where to set it: the image, a
// GitHub credential for gh and pushes inside the container, and a credential
// for the agent. Each can come from more than one place.
func TestCheckSandboxContainer(t *testing.T) {
	bot := GitHub{Login: "bot", Token: "ghp_x"}
	cases := []struct {
		name    string
		role    ResolvedRole
		gh      GitHub
		hostEnv map[string]string
		wantErr string
	}{
		{"none asks nothing", ResolvedRole{Sandbox: SandboxNone}, GitHub{}, nil, ""},
		{"unset asks nothing", ResolvedRole{}, GitHub{}, nil, ""},
		{"claude asks nothing", ResolvedRole{Sandbox: SandboxClaude}, GitHub{}, nil, ""},
		{"no image", ResolvedRole{Sandbox: SandboxContainer}, bot, map[string]string{"ANTHROPIC_API_KEY": "k"}, "sandbox_image"},
		{"no github", ResolvedRole{Sandbox: SandboxContainer, SandboxImage: "img"}, GitHub{}, map[string]string{"ANTHROPIC_API_KEY": "k"}, "[github]"},
		{"GH_TOKEN in the role env instead", ResolvedRole{Sandbox: SandboxContainer, SandboxImage: "img", Env: map[string]string{"GH_TOKEN": "t"}}, GitHub{}, map[string]string{"ANTHROPIC_API_KEY": "k"}, ""},
		{"no agent credential", ResolvedRole{Sandbox: SandboxContainer, SandboxImage: "img"}, bot, nil, "ANTHROPIC_API_KEY or CLAUDE_CODE_OAUTH_TOKEN"},
		{"oauth token on the host", ResolvedRole{Sandbox: SandboxContainer, SandboxImage: "img"}, bot, map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "o"}, ""},
		{"api key in the role env", ResolvedRole{Sandbox: SandboxContainer, SandboxImage: "img", Env: map[string]string{"ANTHROPIC_API_KEY": "k"}}, bot, nil, ""},
		{"codex names its own", ResolvedRole{Sandbox: SandboxContainer, SandboxImage: "img", Agent: AgentCodex}, bot, map[string]string{"ANTHROPIC_API_KEY": "k"}, "OPENAI_API_KEY or CODEX_API_KEY"},
		{"codex with its key", ResolvedRole{Sandbox: SandboxContainer, SandboxImage: "img", Agent: AgentCodex}, bot, map[string]string{"OPENAI_API_KEY": "k"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			machine(t, &fakeMachine{env: tc.hostEnv})
			err := CheckSandboxContainer(tc.role, tc.gh)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// The engine question is asked of the machine: docker on PATH, a daemon
// that answers, the image present. Each refusal names what to do, and the
// image is never pulled by bees. No other mode touches the engine.
func TestCheckSandboxEngine(t *testing.T) {
	failing := func(sub string) func(args ...string) ([]byte, error) {
		return func(args ...string) ([]byte, error) {
			if args[0] == sub {
				return []byte("Cannot connect\nto the daemon"), errors.New("exit status 1")
			}
			return []byte("ok"), nil
		}
	}
	cases := []struct {
		name    string
		mode    string
		onPath  []string
		engine  func(args ...string) ([]byte, error)
		wantErr []string
		calls   int
	}{
		{"all present", SandboxContainer, []string{"docker"}, nil, nil, 2},
		{"no docker", SandboxContainer, nil, nil, []string{"docker on PATH"}, 0},
		{"daemon down", SandboxContainer, []string{"docker"}, failing("info"), []string{"daemon does not answer", "Cannot connect to the daemon"}, 1},
		{"image missing", SandboxContainer, []string{"docker"}, failing("image"), []string{"ghcr.io/acme/bees:1", "docker pull ghcr.io/acme/bees:1"}, 2},
		{"none asks nothing", SandboxNone, nil, nil, nil, 0},
		{"claude asks nothing", SandboxClaude, nil, nil, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := machine(t, &fakeMachine{onPath: tc.onPath, engine: tc.engine})
			err := CheckSandboxEngine(tc.mode, "ghcr.io/acme/bees:1")
			if len(m.calls) != tc.calls {
				t.Errorf("engine calls: got %v, want %d", m.calls, tc.calls)
			}
			if tc.wantErr == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("accepted")
			}
			for _, want := range tc.wantErr {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
		})
	}
}

// `bees run` asks the container questions per enabled role and names the
// role: a configured container developer starts on a machine with docker,
// the image and the credentials, and the same configuration is refused
// naming roles.developer when the daemon is down.
func TestCheckSandboxRunsAContainerRole(t *testing.T) {
	cfg, err := Load(writeConfig(t, `
version = 1
[project]
repo = "a/b"
[github]
login = "bot"
token = "ghp_x"
[roles.developer]
sandbox = "container"
sandbox_image = "ghcr.io/acme/bees:1"
`))
	if err != nil {
		t.Fatal(err)
	}
	m := machine(t, &fakeMachine{onPath: []string{"docker"}, env: map[string]string{"ANTHROPIC_API_KEY": "k"}})
	if err := cfg.CheckSandbox(); err != nil {
		t.Fatalf("a container developer did not start: %v", err)
	}
	if want := []string{"info --format {{.ServerVersion}}", "image inspect --format {{.Id}} ghcr.io/acme/bees:1"}; strings.Join(m.calls, "|") != strings.Join(want, "|") {
		t.Errorf("engine calls: got %v, want %v", m.calls, want)
	}
	m.engine = func(args ...string) ([]byte, error) { return nil, errors.New("exit status 1") }
	err = cfg.CheckSandbox()
	if err == nil {
		t.Fatal("a container developer started with the daemon down")
	}
	if !strings.Contains(err.Error(), "roles.developer") {
		t.Errorf("error %q does not name the role", err)
	}
	m.env = nil
	m.engine = nil
	err = cfg.CheckSandbox()
	if err == nil || !strings.Contains(err.Error(), "ANTHROPIC_API_KEY") {
		t.Errorf("a container developer without an agent credential was not refused by name: %v", err)
	}
}
