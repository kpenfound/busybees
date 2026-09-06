package config

import (
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
// has no mode set, and an empty mode is none rather than a refusal.
func TestCheckSandboxModeTakesAnEmptyMode(t *testing.T) {
	if err := CheckSandboxMode(""); err != nil {
		t.Errorf("an unset mode was refused: %v", err)
	}
	if err := CheckSandboxMode(SandboxNone); err != nil {
		t.Errorf("none was refused: %v", err)
	}
	for _, mode := range []string{SandboxClaude, SandboxContainer} {
		if err := CheckSandboxMode(mode); err == nil {
			t.Errorf("%s reported itself implemented", mode)
		}
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
