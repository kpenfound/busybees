package eval

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/review"
)

func localConfig(t *testing.T, text string) *config.Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bees.toml")
	cfg, err := config.Parse("version = 5\n[project]\nrepo = \"acme/widgets\"\n"+text, path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func globalConfig(t *testing.T, text string) *review.Config {
	t.Helper()
	// LoadConfig layers defaults.toml below the file; keep the machine's
	// own out of the test.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	if text != "" {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := review.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// reviewHome points XDG_CONFIG_HOME at a directory of its own and returns
// the bees directory in it, so the files a test writes there are the ones
// the loaders read, and the machine's own are out of the way.
func reviewHome(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "bees")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeDefaults(t *testing.T, home, text string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(home, config.DefaultsFile), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// defaultsConfig writes a defaults.toml for SelectProfile to read, and
// returns the global configuration the eval command loads when there is no
// config.toml: the defaults file layered below a file that is not there.
func defaultsConfig(t *testing.T, text string) *review.Config {
	t.Helper()
	writeDefaults(t, reviewHome(t), text)
	cfg, err := review.LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

const localProfiles = `
[global]
profile = "deep"
[roles.qa]
profile = "cheap"
[roles.reviewer]
brief_profile = "cheap"
[profiles.deep]
agent = "claude"
model = "opus"
effort = "high"
[profiles.cheap]
agent = "claude"
model = "haiku"
[profiles.boxed]
sandbox = "container"
`

// defaultsTables is a defaults.toml with the selection localProfiles makes
// in bees.toml, for the tests that take their selection from the defaults
// file alone.
const defaultsTables = `version = 5
[profiles.deep]
agent = "claude"
model = "opus"
effort = "high"
[profiles.cheap]
agent = "claude"
model = "haiku"
[global]
profile = "deep"
profile_by_size = { xs = "cheap" }
[roles.qa]
profile = "cheap"
[roles.reviewer]
brief_profile = "cheap"
`

func TestSelectProfile(t *testing.T) {
	local := localConfig(t, localProfiles)
	global := globalConfig(t, "provider = \"codex\"\nmodel = \"gpt-5\"\n[profiles.fromglobal]\nagent = \"codex\"\nmodel = \"o3\"\n")

	// bees.toml's own selection, role overrides and all.
	s, err := SelectProfile("", local, global)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "deep" || s.Source != local.Path || s.String() != "deep (bees.toml)" ||
		s.Roles["developer"] != "claude opus" || s.Roles["qa"] != "claude haiku" {
		t.Fatalf("local selection: %+v", s)
	}
	cfg := parseSelection(t, s)
	if reviewer, _ := cfg.Role(config.RoleReviewer); reviewer.BriefProfile == nil || reviewer.BriefProfile.Model != "haiku" {
		t.Fatalf("the reviewer's brief profile was not carried: %+v", reviewer.BriefProfile)
	}
	if dev, _ := cfg.Role(config.RoleDeveloper); dev.Effort != "high" {
		t.Fatalf("the developer's profile: %+v", dev)
	}

	// --profile runs every role on the named profile, bees.toml first.
	s, err = SelectProfile("cheap", local, global)
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range config.Roles {
		if s.Roles[role] != "claude haiku" {
			t.Fatalf("--profile cheap: %s runs %q", role, s.Roles[role])
		}
	}

	// Then the global config.toml's.
	s, err = SelectProfile("fromglobal", local, global)
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != global.Path || s.Roles["qa"] != "codex o3" || s.String() != "fromglobal (config.toml)" {
		t.Fatalf("--profile fromglobal: %+v", s)
	}

	// Without a bees.toml, config.toml's provider and model.
	s, err = SelectProfile("", nil, global)
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != global.Path || s.Roles["developer"] != "codex gpt-5" {
		t.Fatalf("global selection: %+v", s)
	}

	// Without either, the built-in profile.
	s, err = SelectProfile("", nil, globalConfig(t, ""))
	if err != nil {
		t.Fatal(err)
	}
	if s.Source != BuiltIn || s.Name != "default" || s.Roles["developer"] != "claude opus" {
		t.Fatalf("built-in selection: %+v", s)
	}
}

// A profile the defaults file alone defines is found, and the selection the
// defaults file makes is taken when there is no bees.toml and no
// config.toml.
func TestSelectProfileFromDefaults(t *testing.T) {
	global := defaultsConfig(t, defaultsTables)
	if global.Loaded {
		t.Fatal("config.toml existed in the test's own configuration directory")
	}

	// --profile runs every role on the profile, named as the file it came
	// from.
	s, err := SelectProfile("cheap", nil, global)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "cheap" || s.Source != config.DefaultDefaultsPath() || s.String() != "cheap (defaults.toml)" ||
		s.Roles["developer"] != "claude haiku" {
		t.Fatalf("--profile cheap from defaults.toml: %+v", s)
	}

	// Without a bees.toml and a config.toml, the selection is the tables
	// the defaults file makes, the reviewer's included.
	s, err = SelectProfile("", nil, global)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "deep" || s.Source != config.DefaultDefaultsPath() || s.String() != "deep (defaults.toml)" ||
		s.Roles["developer"] != "claude opus" || s.Roles["qa"] != "claude haiku" {
		t.Fatalf("selection from defaults.toml: %+v", s)
	}
	cfg := parseSelection(t, s)
	if reviewer, _ := cfg.Role(config.RoleReviewer); reviewer.BriefProfile == nil || reviewer.BriefProfile.Model != "haiku" {
		t.Fatalf("the reviewer's brief profile was not carried: %+v", reviewer.BriefProfile)
	}
	if dev, _ := cfg.Role(config.RoleDeveloper); dev.Effort != "high" || dev.ForSize("xs").Model != "haiku" {
		t.Fatalf("the developer's profile: %+v", dev)
	}
}

// The order the selection is taken in does not change: a config.toml that
// exists is the selection whatever tables the defaults file below it makes,
// and a bees.toml is nearer still.
func TestDefaultsSelectionIsTheLastFallback(t *testing.T) {
	home := reviewHome(t)
	writeDefaults(t, home, defaultsTables)
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("provider = \"codex\"\nmodel = \"gpt-5\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	global, err := review.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	s, err := SelectProfile("", nil, global)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "default" || s.Source != path || s.Roles["developer"] != "codex gpt-5" {
		t.Fatalf("config.toml's selection: %+v", s)
	}

	local := localConfig(t, localProfiles)
	s, err = SelectProfile("", local, global)
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "deep" || s.Source != local.Path || s.Roles["developer"] != "claude opus" {
		t.Fatalf("bees.toml's selection: %+v", s)
	}
}

func parseSelection(t *testing.T, s Selection) *config.Config {
	t.Helper()
	cfg, err := config.Parse(s.configText(settings{Repo: "bees-eval/x", StateDir: "state", Workspaces: "wt", PassInterval: "1s"}), filepath.Join(t.TempDir(), "bees.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestSelectProfileRefuses(t *testing.T) {
	local := localConfig(t, localProfiles)
	if _, err := SelectProfile("nope", local, globalConfig(t, "")); err == nil ||
		!strings.Contains(err.Error(), `--profile "nope": no such profile in `+local.Path) {
		t.Fatalf("an unknown profile: %v", err)
	}
	// A container session has no route to the eval's GitHub.
	if _, err := SelectProfile("boxed", local, nil); err == nil || !strings.Contains(err.Error(), `sandbox "container"`) {
		t.Fatalf("a container profile: %v", err)
	}
	// Nor has one a fallback puts there.
	withFallback := localConfig(t, localProfiles+"[profiles.fallsback]\nfallback = \"boxed\"\n")
	if _, err := SelectProfile("fallsback", withFallback, nil); err == nil || !strings.Contains(err.Error(), `sandbox "container"`) {
		t.Fatalf("a fallback to a container profile: %v", err)
	}
	// Nor has the reviewer's judge session on one.
	judged := localConfig(t, strings.Replace(localProfiles, `brief_profile = "cheap"`, `judge_profile = "boxed"`, 1))
	if _, err := SelectProfile("", judged, nil); err == nil || !strings.Contains(err.Error(), `reviewer role's profile uses sandbox "container"`) {
		t.Fatalf("a container judge profile: %v", err)
	}
	// The defaults file's selection goes through the same checks: a
	// container profile it selects is refused too.
	global := defaultsConfig(t, `version = 5
[profiles.boxed]
sandbox = "container"
[global]
profile = "boxed"
`)
	if _, err := SelectProfile("", nil, global); err == nil || !strings.Contains(err.Error(), `sandbox "container"`) {
		t.Fatalf("a container profile the defaults file selects: %v", err)
	}
}

// The eval's bees.toml carries the profiles and nothing else of the file
// they came from.
func TestConfigTextKeepsOnlyTheProfiles(t *testing.T) {
	local := localConfig(t, localProfiles+"[scheduler]\nmax_developers = 7\n[filter]\nassignee = \"@me\"\n")
	s, err := SelectProfile("", local, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := parseSelection(t, s)
	if cfg.Scheduler.MaxDevelopers == 7 || cfg.Filter.Assignee != "" || cfg.Project.Repo != "bees-eval/x" || cfg.Project.DefaultBranch != DefaultBranch {
		t.Fatalf("eval config: %+v", cfg)
	}
	policy := cfg.Merge()
	if policy.ChecksWait.Seconds() != 1 || policy.ChecksPollInterval.Seconds() != 1 || policy.AutoMerge {
		t.Fatalf("merge policy: %+v", policy)
	}
}

// A reviewer case's bees.toml caps the review loop at one round, and no
// other case's does: the loop's next stage after "changes requested" is a
// developer session, which a per-role reviewer run must not start.
func TestConfigTextGivesAReviewerCaseOneReviewRound(t *testing.T) {
	s, err := SelectProfile("", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	set := settings{Repo: "bees-eval/x", StateDir: "state", Workspaces: "wt", PassInterval: "1s"}
	for _, tc := range []struct {
		role string
		want int
	}{
		{config.RoleReviewer, 1},
		{config.RoleDeveloper, config.DefaultReviewRounds},
		{"", config.DefaultReviewRounds},
	} {
		set.Role = tc.role
		text := s.configText(set)
		cfg, err := config.Parse(text, filepath.Join(t.TempDir(), "bees.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Scheduler.MaxReviewRounds != tc.want {
			t.Errorf("role %q: max_review_rounds %d, want %d:\n%s", tc.role, cfg.Scheduler.MaxReviewRounds, tc.want, text)
		}
	}
}
