package config

import (
	"slices"
	"strings"
	"testing"
)

// A profile with agent = "pi" loads with no model of bees' choosing, as
// codex's and opencode's do, and runs with sandbox none or container.
func TestPiProfileLoads(t *testing.T) {
	cfg, err := Load(writeConfig(t, `version = 4
[profiles.pi]
agent = "pi"
[profiles.boxed]
agent = "pi"
model = "openrouter/qwen"
effort = "high"
sandbox = "container"
[roles.developer]
profile = "pi"
sandbox_image = "ghcr.io/acme/pi:1"
[roles.qa]
profile = "boxed"
`))
	if err != nil {
		t.Fatal(err)
	}
	dev, _ := cfg.Role(RoleDeveloper)
	if dev.Agent != AgentPi || dev.Model != "" || dev.FallbackModel != "" || dev.Sandbox != SandboxNone {
		t.Errorf("developer: %+v", dev.AgentProfile())
	}
	qa, _ := cfg.Role(RoleQA)
	if qa.Agent != AgentPi || qa.Model != "openrouter/qwen" || qa.Effort != "high" || qa.Sandbox != SandboxContainer {
		t.Errorf("qa: %+v", qa.AgentProfile())
	}
}

// Pi has no sandbox of its own, so a profile asking for Claude Code's with
// agent = "pi" is refused at load, naming the key, however the profile came
// to be: declared, or migrated from a version 1 file's role settings.
func TestPiRefusesClaudeSandboxAtLoad(t *testing.T) {
	for body, key := range map[string]string{
		"version = 4\n[profiles.remote]\nagent = \"pi\"\nsandbox = \"claude\"\n":                               "profiles.remote.sandbox",
		"version = 4\n[profiles.default]\nagent = \"pi\"\nsandbox = \"claude\"\n":                              "profiles.default.sandbox",
		"version = 1\n[project]\nrepo = \"a/b\"\n[global]\nsandbox = \"claude\"\n[roles.qa]\nagent = \"pi\"\n": ".sandbox",
	} {
		_, err := Load(writeConfig(t, body))
		if err == nil {
			t.Errorf("%q loaded", body)
			continue
		}
		for _, want := range []string{key, `"claude"`, `"pi"`, "none or container"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%q: error %q does not mention %s", body, err, want)
			}
		}
	}
	// The same sandbox on another agent still loads, and so does pi on a
	// profile no role uses once it asks for a mode pi runs in.
	for _, body := range []string{
		"version = 4\n[profiles.remote]\nagent = \"codex\"\nsandbox = \"claude\"\n",
		"version = 4\n[profiles.remote]\nagent = \"pi\"\nsandbox = \"none\"\n",
		"version = 4\n[profiles.remote]\nagent = \"pi\"\n",
	} {
		if _, err := Load(writeConfig(t, body)); err != nil {
			t.Errorf("%q: %v", body, err)
		}
	}
	// `bees run` and the session runner refuse it too.
	if err := CheckSandboxAgent(SandboxClaude, AgentPi); err == nil || !strings.Contains(err.Error(), `"pi"`) {
		t.Errorf("CheckSandboxAgent(claude, pi) = %v", err)
	}
	for _, mode := range []string{"", SandboxNone, SandboxContainer} {
		if err := CheckSandboxAgent(mode, AgentPi); err != nil {
			t.Errorf("CheckSandboxAgent(%q, pi) = %v", mode, err)
		}
	}
}

// pi_packages is unioned like skills: global first, then the role's, each
// source once; empty when neither sets it.
func TestPiPackagesMerge(t *testing.T) {
	cfg, err := Load(writeConfig(t, `version = 4
[global]
pi_packages = ["npm:a", "git:github.com/acme/b@v1"]
[roles.developer]
pi_packages = ["npm:c", "npm:a"]
`))
	if err != nil {
		t.Fatal(err)
	}
	dev, _ := cfg.Role(RoleDeveloper)
	if want := []string{"npm:a", "git:github.com/acme/b@v1", "npm:c"}; !slices.Equal(dev.PiPackages, want) {
		t.Errorf("developer pi_packages %q, want %q", dev.PiPackages, want)
	}
	qa, _ := cfg.Role(RoleQA)
	if want := []string{"npm:a", "git:github.com/acme/b@v1"}; !slices.Equal(qa.PiPackages, want) {
		t.Errorf("qa pi_packages %q, want %q", qa.PiPackages, want)
	}
	cfg, err = Load(writeConfig(t, "version = 4\n"))
	if err != nil {
		t.Fatal(err)
	}
	if dev, _ := cfg.Role(RoleDeveloper); len(dev.PiPackages) != 0 {
		t.Errorf("pi_packages with none configured: %q", dev.PiPackages)
	}
}

// A pi_packages entry pi could not be handed as `-e <source>`, or one naming
// the adapter every pi session loads already, is refused at load naming the
// scope's key.
func TestPiPackagesInvalid(t *testing.T) {
	for _, tc := range []struct{ scope, value, want string }{
		{"global", `""`, "empty"},
		{"global", `" "`, "empty"},
		{"roles.developer", `"--no-extensions"`, "flag"},
		{"global", `"npm:pi-mcp-adapter"`, PiMCPAdapter},
		{"roles.qa", `"npm:pi-mcp-adapter@2.34.0"`, PiMCPAdapter},
		{"global", `"git:github.com/nicobailon/pi-mcp-adapter@v2"`, PiMCPAdapter},
		{"global", `"https://github.com/nicobailon/pi-mcp-adapter.git"`, PiMCPAdapter},
		{"global", `"/opt/pi-mcp-adapter"`, PiMCPAdapter},
	} {
		_, err := Load(writeConfig(t, "version = 4\n["+tc.scope+"]\npi_packages = ["+tc.value+"]\n"))
		if err == nil || !strings.Contains(err.Error(), tc.scope+".pi_packages") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s.pi_packages = [%s]: %v", tc.scope, tc.value, err)
		}
	}
	for _, ok := range []string{`"npm:@acme/pi-tools@1.2.3"`, `"npm:pi-mcp-adapter-extras"`, `"./tools/pi-ext"`} {
		if _, err := Load(writeConfig(t, "version = 4\n[global]\npi_packages = ["+ok+"]\n")); err != nil {
			t.Errorf("pi_packages = [%s]: %v", ok, err)
		}
	}
}

// The review pipeline's brief and angle sessions run claude or codex only,
// so a pi profile there is refused like an opencode one; the judge, an
// ordinary session, can be pi.
func TestPiReviewProfiles(t *testing.T) {
	_, err := Load(writeConfig(t, "version = 4\n[profiles.pi]\nagent = \"pi\"\n[roles.reviewer]\nbrief_profile = \"pi\"\n"))
	if err == nil || !strings.Contains(err.Error(), "roles.reviewer.brief_profile") {
		t.Errorf("a pi brief profile: %v", err)
	}
	if _, err := Load(writeConfig(t, "version = 4\n[profiles.pi]\nagent = \"pi\"\n[roles.reviewer]\njudge_profile = \"pi\"\n")); err != nil {
		t.Errorf("a pi judge profile: %v", err)
	}
}
