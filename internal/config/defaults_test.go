package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func defaultsFixture(t *testing.T, project, defaults string) (string, string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	configDir := filepath.Join(xdg, "bees")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	defaultsPath := filepath.Join(configDir, DefaultsFile)
	if defaults != "" {
		if err := os.WriteFile(defaultsPath, []byte(defaults), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	projectPath := filepath.Join(t.TempDir(), "bees.toml")
	if err := os.WriteFile(projectPath, []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}
	return projectPath, defaultsPath
}

func TestLoadMergesUserDefaults(t *testing.T) {
	project, defaults := defaultsFixture(t, `version = 5
[profiles.shared]
model = "project-model"
[profiles.project]
agent = "codex"
[global]
profile_by_size = { s = "project" }
[roles.developer]
profile = "project"
profile_by_size = { l = "project" }
[roles.reviewer]
angle_profiles = { docs = "project" }
angles = { xs = ["docs"] }
`, `version = 5
[profiles.shared]
agent = "codex"
model = "user-model"
fallback = "user"
[profiles.user]
agent = "claude"
model = "sonnet"
[global]
profile = "user"
profile_by_size = { xs = "user", s = "user" }
[roles.developer]
profile = "shared"
profile_by_size = { m = "shared", l = "shared" }
[roles.reviewer]
brief_profile = "user"
judge_profile = "shared"
angle_profiles = { general = "shared", docs = "shared" }
angles = { xs = ["quick_general"], m = ["general", "docs"] }
`)

	cfg, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Profiles["shared"]; got.Agent != "" || got.Model != "project-model" || got.Fallback != "" {
		t.Fatalf("project profile did not replace user profile whole: %+v", got)
	}
	if got := cfg.Profiles["user"]; got.Agent != "claude" || got.Model != "sonnet" {
		t.Fatalf("user-only profile missing: %+v", got)
	}
	if cfg.Global.Profile != "user" || cfg.Global.ProfileBySize["xs"] != "user" || cfg.Global.ProfileBySize["s"] != "project" {
		t.Fatalf("global selectors not merged per key: %+v", cfg.Global)
	}
	dev := cfg.Roles[RoleDeveloper]
	if dev.Profile != "project" || dev.ProfileBySize["m"] != "shared" || dev.ProfileBySize["l"] != "project" {
		t.Fatalf("developer selectors not merged: %+v", dev)
	}
	reviewer := cfg.Roles[RoleReviewer]
	if reviewer.BriefProfile != "user" || reviewer.JudgeProfile != "shared" || reviewer.AngleProfiles["general"] != "shared" || reviewer.AngleProfiles["docs"] != "project" {
		t.Fatalf("review profile selectors not merged: %+v", reviewer)
	}
	if got := strings.Join(reviewer.Angles["xs"], ","); got != "docs" {
		t.Fatalf("project angle list did not replace whole list: %q", got)
	}
	if got := strings.Join(reviewer.Angles["m"], ","); got != "general,docs" {
		t.Fatalf("user angle map entry missing: %q", got)
	}
	for key, want := range map[string]string{
		"profiles.shared":                    project,
		"profiles.user":                      defaults,
		"global.profile_by_size.xs":          defaults,
		"global.profile_by_size.s":           project,
		"roles.reviewer.angles.xs":           project,
		"roles.reviewer.angles.m":            defaults,
		"roles.reviewer.brief_profile":       defaults,
		"roles.reviewer.angle_profiles.docs": project,
	} {
		if got := cfg.sources[key]; got != want {
			t.Errorf("source %s = %q, want %q (all: %#v)", key, got, want, cfg.sources)
		}
	}
}

func TestLoadProjectExplicitEmptySelectorWins(t *testing.T) {
	project, _ := defaultsFixture(t, "version = 5\n[global]\nprofile = \"\"\n", "version = 5\n[profiles.user]\n[global]\nprofile = \"user\"\n")
	cfg, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Global.Profile != "" {
		t.Fatalf("explicit empty project profile = %q, want empty", cfg.Global.Profile)
	}
}

func TestLoadUserDefaultsValidationAcrossFiles(t *testing.T) {
	t.Run("references cross both ways", func(t *testing.T) {
		project, _ := defaultsFixture(t, `version = 5
[profiles.project]
agent = "codex"
[roles.developer]
profile = "user"
`, `version = 5
[profiles.user]
fallback = "project"
[roles.qa]
profile = "project"
`)
		if _, err := Load(project); err != nil {
			t.Fatal(err)
		}
	})

	for _, tc := range []struct {
		name, project, defaults, badFile, key string
	}{
		{"project selector", "version = 5\n[global]\nprofile = \"missing\"\n", "version = 5\n", "project", "global.profile"},
		{"user selector", "version = 5\n", "version = 5\n[roles.qa]\nprofile = \"missing\"\n", "defaults", "roles.qa.profile"},
		{"project fallback", "version = 5\n[profiles.bad]\nfallback = \"missing\"\n", "version = 5\n", "project", "profiles.bad.fallback"},
		{"user fallback", "version = 5\n", "version = 5\n[profiles.bad]\nfallback = \"missing\"\n", "defaults", "profiles.bad.fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, defaults := defaultsFixture(t, tc.project, tc.defaults)
			_, err := Load(project)
			wantFile := project
			if tc.badFile == "defaults" {
				wantFile = defaults
			}
			if err == nil || !strings.Contains(err.Error(), wantFile+": "+tc.key) {
				t.Fatalf("Load() error = %v, want offending source %s and key %s", err, wantFile, tc.key)
			}
		})
	}

	t.Run("overridden user profile is still syntax checked", func(t *testing.T) {
		project, defaults := defaultsFixture(t, "version = 5\n[profiles.same]\nagent = \"codex\"\n", "version = 5\n[profiles.same]\nagent = \"wrong\"\n")
		_, err := Load(project)
		if err == nil || !strings.Contains(err.Error(), defaults+": profiles.same.agent") {
			t.Fatalf("Load() error = %v, want invalid user value named", err)
		}
	})
}

func TestLoadUserDefaultsRefusesOtherKeys(t *testing.T) {
	for _, tc := range []struct {
		name, body, key string
	}{
		{"project setting", "[project]\nrepo = \"a/b\"", "project"},
		{"global setting", "[global]\nprompt = \"shared\"", "global.prompt"},
		{"role setting", "[roles.developer]\nmax_turns = 10", "roles.developer.max_turns"},
		{"review setting in another role", "[roles.developer]\nbrief_profile = \"x\"", "roles.developer.brief_profile"},
		{"profile setting", "[profiles.x]\nunknown = true", "profiles.x.unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			project, defaults := defaultsFixture(t, "version = 5\n", "version = 5\n"+tc.body+"\n")
			_, err := Load(project)
			if err == nil || !strings.Contains(err.Error(), defaults) || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("Load() error = %v, want %s and %s", err, defaults, tc.key)
			}
		})
	}
}

func TestLoadWithoutUserDefaults(t *testing.T) {
	project, defaults := defaultsFixture(t, "version = 5\n", "")
	cfg, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(defaults); !os.IsNotExist(err) {
		t.Fatalf("defaults fixture unexpectedly exists: %v", err)
	}
	if len(cfg.Profiles) != 0 || cfg.Global.Profile != "" {
		t.Fatalf("missing defaults changed config: profiles=%v global=%+v", cfg.Profiles, cfg.Global)
	}
}

func TestLoadReloadsChangedUserDefaults(t *testing.T) {
	project, defaults := defaultsFixture(t, "version = 5\n[global]\nprofile = \"p\"\n", "version = 5\n[profiles.p]\nmodel = \"first\"\n")
	first, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	if got := first.Profiles["p"].Model; got != "first" {
		t.Fatalf("first load model = %q", got)
	}
	if err := os.WriteFile(defaults, []byte("version = 5\n[profiles.p]\nmodel = \"second\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := Load(project)
	if err != nil {
		t.Fatal(err)
	}
	if got := second.Profiles["p"].Model; got != "second" {
		t.Fatalf("reloaded model = %q, want second", got)
	}
}

// LoadUserDefaults is the exported loader, the one `bees review`'s
// config.go layers below its config.toml.
func TestLoadUserDefaultsExported(t *testing.T) {
	t.Run("missing loads as nil", func(t *testing.T) {
		defaultsFixture(t, "version = 5\n", "")
		d, err := LoadUserDefaults()
		if err != nil {
			t.Fatal(err)
		}
		if d != nil {
			t.Fatalf("a missing defaults.toml loads as %+v, want nil", d)
		}
	})
	t.Run("present", func(t *testing.T) {
		defaultsFixture(t, "version = 5\n", `version = 5
[profiles.fast]
model = "sonnet"
[roles.reviewer]
brief_profile = "fast"
angles = { xs = ["docs"] }
`)
		d, err := LoadUserDefaults()
		if err != nil {
			t.Fatal(err)
		}
		if d.Profiles["fast"].Model != "sonnet" || d.Roles[RoleReviewer].BriefProfile != "fast" {
			t.Fatalf("LoadUserDefaults read %+v", d)
		}
		if got := strings.Join(d.Roles[RoleReviewer].Angles["xs"], ","); got != "docs" {
			t.Fatalf("angles = %q", got)
		}
	})
	t.Run("invalid names the file", func(t *testing.T) {
		_, defaults := defaultsFixture(t, "version = 5\n", "version = 5\n[global]\nprompt = \"x\"\n")
		if _, err := LoadUserDefaults(); err == nil || !strings.Contains(err.Error(), defaults) {
			t.Fatalf("LoadUserDefaults() error = %v, want the file named", err)
		}
	})
}

func TestUserDefaultsVersion(t *testing.T) {
	t.Run("newer", func(t *testing.T) {
		project, defaults := defaultsFixture(t, "version = 5\n", "version = 6\n")
		_, err := Load(project)
		if err == nil || !strings.Contains(err.Error(), defaults) || !strings.Contains(err.Error(), "newer than this bees understands") {
			t.Fatalf("Load() error = %v", err)
		}
	})
	t.Run("older has independent migrations", func(t *testing.T) {
		project, defaults := defaultsFixture(t, "version = 5\n", "version = 4\n")
		_, err := Load(project)
		if err == nil || !strings.Contains(err.Error(), defaults) || !strings.Contains(err.Error(), "no migration from defaults.toml version 4 to 5") {
			t.Fatalf("Load() error = %v", err)
		}
	})
}
