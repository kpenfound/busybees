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

// CheckSandbox is what `bees run` asks before its first poll: a role asking
// for a box bees cannot build stops the factory, naming the role and the
// modes there are, rather than letting that role run unboxed.
func TestCheckSandboxRefusesAModeThatIsNotImplemented(t *testing.T) {
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
// has no mode set, and an empty mode is none rather than a refusal. none and
// claude are implemented; container is not.
func TestCheckSandboxModeTakesAnEmptyMode(t *testing.T) {
	for _, mode := range []string{"", SandboxNone, SandboxClaude} {
		if err := CheckSandboxMode(mode); err != nil {
			t.Errorf("mode %q was refused: %v", mode, err)
		}
	}
	if err := CheckSandboxMode(SandboxContainer); err == nil {
		t.Error("container reported itself implemented")
	}
}

// fakeHost describes a machine to CheckSandboxHost for the rest of the test:
// its operating system and which programs are on its PATH.
func fakeHost(t *testing.T, goos string, onPath ...string) {
	t.Helper()
	oldOS, oldLook := hostOS, lookPath
	t.Cleanup(func() { hostOS, lookPath = oldOS, oldLook })
	hostOS = goos
	lookPath = func(name string) (string, error) {
		for _, p := range onPath {
			if p == name {
				return "/usr/bin/" + name, nil
			}
		}
		return "", errors.New("not found")
	}
}

// Claude Code's sandbox is the system's own on macOS, needs bubblewrap and
// socat on Linux, and runs nowhere else. The other modes have no host
// question: none needs nothing, container is refused before this is asked.
func TestCheckSandboxHost(t *testing.T) {
	cases := []struct {
		name    string
		goos    string
		onPath  []string
		mode    string
		wantErr []string
	}{
		{"macos needs nothing", "darwin", nil, SandboxClaude, nil},
		{"linux with both tools", "linux", []string{"bwrap", "socat"}, SandboxClaude, nil},
		{"linux without socat", "linux", []string{"bwrap"}, SandboxClaude, []string{"socat"}},
		{"linux without either", "linux", nil, SandboxClaude, []string{"bwrap and socat"}},
		{"windows", "windows", []string{"bwrap", "socat"}, SandboxClaude, []string{"macOS and Linux only", "windows"}},
		{"none anywhere", "windows", nil, SandboxNone, nil},
		{"container is not this check's", "windows", nil, SandboxContainer, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fakeHost(t, tc.goos, tc.onPath...)
			err := CheckSandboxHost(tc.mode)
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

// `bees run` asks the host question per enabled role and names the role, so
// the error says which line of bees.toml to change or what to install. A
// Linux machine without bubblewrap cannot run a claude-boxed developer, and
// the same configuration starts on macOS.
func TestCheckSandboxAsksTheHost(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[roles.developer]\nsandbox = \"claude\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	fakeHost(t, "linux", "socat")
	err = cfg.CheckSandbox()
	if err == nil {
		t.Fatal("a claude developer started on a Linux machine without bubblewrap")
	}
	for _, want := range []string{"roles.developer", "bwrap"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	fakeHost(t, "darwin")
	if err := cfg.CheckSandbox(); err != nil {
		t.Errorf("a claude developer did not start on macOS: %v", err)
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

// The claude box is Claude Code's, so a codex role cannot ask for it: `bees
// run` refuses the configuration naming the role, and the runner refuses the
// session, rather than run codex with its own sandbox switched off.
func TestCheckSandboxAgent(t *testing.T) {
	if err := CheckSandboxAgent(SandboxClaude, AgentCodex); err == nil {
		t.Error("a codex role was given the claude sandbox")
	} else {
		for _, want := range []string{"claude", "codex"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not mention %q", err, want)
			}
		}
	}
	for _, tc := range []struct{ mode, agent string }{
		{SandboxClaude, AgentClaude}, {SandboxClaude, ""}, {SandboxNone, AgentCodex}, {"", AgentCodex}, {SandboxContainer, AgentCodex},
	} {
		if err := CheckSandboxAgent(tc.mode, tc.agent); err != nil {
			t.Errorf("sandbox %q with agent %q refused: %v", tc.mode, tc.agent, err)
		}
	}
	cfg, err := Load(writeConfig(t, "version = 1\n[project]\nrepo = \"a/b\"\n[global]\nsandbox = \"claude\"\n[roles.qa]\nagent = \"codex\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	fakeHost(t, "darwin")
	err = cfg.CheckSandbox()
	if err == nil {
		t.Fatal("a codex qa started in a claude box")
	}
	if !strings.Contains(err.Error(), "roles.qa") {
		t.Errorf("error %q does not name the role", err)
	}
}
