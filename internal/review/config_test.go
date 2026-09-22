package review

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// configHome points XDG_CONFIG_HOME at a directory of its own and returns
// the bees directory in it, so the defaults.toml a test writes is the one
// LoadConfig reads, and the machine's own is out of the way.
func configHome(t *testing.T) string {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	dir := filepath.Join(xdg, "bees")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func writeDefaults(t *testing.T, dir, body string) string {
	t.Helper()
	return writeFile(t, dir, config.DefaultsFile, body)
}

// defaults.toml with the reviewer setup the per-step tests layer config.toml
// over: a profile for the brief and for two angles, one for the judge, the
// angles of two sizes, a base profile and a global one.
const defaultsReviewer = `version = 5
[profiles.user_brief]
agent = "claude"
model = "opus"
effort = "high"
fallback = "user_spare"

[profiles.user_spare]
agent = "codex"
model = "gpt-5"
effort = "low"

[profiles.user_docs]
agent = "claude"
model = "sonnet"

[global]
profile = "user_docs"

[roles.reviewer]
profile = "user_brief"
brief_profile = "user_brief"
judge_profile = "user_docs"
angle_profiles = { docs = "user_docs", general = "user_docs" }
angles = { xs = ["docs"], m = ["general", "docs"] }
`

func TestConfigDefaults(t *testing.T) {
	configHome(t)
	dir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(dir, ConfigFile))
	if err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}
	if cfg.Loaded {
		t.Fatal("Loaded is true for a file that is not there")
	}
	if cfg.Provider != "claude" || cfg.Model != "opus" || cfg.Output != OutputAsk {
		t.Fatalf("defaults: %+v", cfg)
	}
	if got, want := cfg.ResolvedNotesPath(), filepath.Join(dir, "reviewer-notes.md"); got != want {
		t.Fatalf("notes path %q, want %q", got, want)
	}
	if got, want := cfg.ResolvedStoragePath(), filepath.Join(dir, "reviews"); got != want {
		t.Fatalf("storage path %q, want %q", got, want)
	}
	if cfg.GitHub.ResolvedToken() != "" || cfg.GitHub.RedactedToken() != "" {
		t.Fatalf("no token configured, got %q", cfg.GitHub.RedactedToken())
	}
}

// An empty file is the same as no file, and every key left out of a file
// that does set some keeps its default.
func TestConfigPartialFileKeepsDefaults(t *testing.T) {
	configHome(t)
	dir := t.TempDir()
	cfg, err := LoadConfig(writeFile(t, dir, ConfigFile, "output = \"report\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Loaded {
		t.Fatal("Loaded is false for a file that is there")
	}
	if cfg.Output != OutputReport {
		t.Fatalf("output %q", cfg.Output)
	}
	if cfg.Provider != DefaultProvider || cfg.Model != DefaultModel || cfg.NotesPath != DefaultNotesPath || cfg.StoragePath != DefaultStoragePath {
		t.Fatalf("the rest is not defaulted: %+v", cfg)
	}
	if cfg.Angles != nil || cfg.AngleModels != nil || cfg.BriefModel != "" || cfg.JudgeModel != "" {
		t.Fatalf("a file with no per-step keys overrides nothing: %+v", cfg)
	}
}

// The per-size angles and per-step models have the shape of bees.toml's
// roles.reviewer.
func TestConfigReviewerShape(t *testing.T) {
	configHome(t)
	cfg, err := LoadConfig(writeFile(t, t.TempDir(), ConfigFile, `
model = "opus"
brief_model = "sonnet"
judge_model = "haiku"

[angles]
xs = ["quick_general"]
xl = ["general", "side_effects"]

[angle_models]
docs = "haiku"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Model != "opus" || cfg.BriefModel != "sonnet" || cfg.JudgeModel != "haiku" {
		t.Fatalf("models: %+v", cfg)
	}
	want := map[string][]string{"xs": {AngleQuickGeneral}, "xl": {AngleGeneral, AngleSideEffects}}
	if !reflect.DeepEqual(cfg.Angles, want) {
		t.Fatalf("angles = %v, want %v", cfg.Angles, want)
	}
	if !reflect.DeepEqual(cfg.AngleModels, map[string]string{AngleDocs: "haiku"}) {
		t.Fatalf("angle_models = %v", cfg.AngleModels)
	}
}

func TestConfigOverrides(t *testing.T) {
	configHome(t)
	dir := t.TempDir()
	t.Setenv("REVIEW_TOKEN", "ghp_secret")
	cfg, err := LoadConfig(writeFile(t, dir, ConfigFile, `
provider = "codex"
model = "gpt-5"
notes_path = "notes/reviewer-notes.md"
storage_path = "/var/lib/bees/reviews"
output = "comment"

[github]
token = "$REVIEW_TOKEN"
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "codex" || cfg.Model != "gpt-5" || cfg.Output != OutputComment {
		t.Fatalf("overrides: %+v", cfg)
	}
	if got, want := cfg.ResolvedNotesPath(), filepath.Join(dir, "notes", "reviewer-notes.md"); got != want {
		t.Fatalf("relative notes path %q, want %q", got, want)
	}
	if got := cfg.ResolvedStoragePath(); got != "/var/lib/bees/reviews" {
		t.Fatalf("absolute storage path %q", got)
	}
	if got := cfg.GitHub.ResolvedToken(); got != "ghp_secret" {
		t.Fatalf("token %q, want the expanded variable", got)
	}
	// Whitespace around the reference is the shape a token written on its
	// own line takes; it is not part of the credential.
	padded := GitHub{Token: "  $REVIEW_TOKEN  "}
	if got := padded.ResolvedToken(); got != "ghp_secret" {
		t.Fatalf("padded token %q, want the expanded variable alone", got)
	}
	if got := (GitHub{Token: "ghp_literal"}).RedactedToken(); got != "(set)" {
		t.Fatalf("redacted literal token %q: the value itself is never printed", got)
	}
	if got := cfg.GitHub.RedactedToken(); got != "$REVIEW_TOKEN" {
		t.Fatalf("redacted token %q: a $VAR reference is printed as written", got)
	}
}

func TestConfigInvalid(t *testing.T) {
	// A literal token spelled with no $ still has to be there, so the empty
	// case is only reachable through a variable that is not set.
	t.Setenv("REVIEW_UNSET_TOKEN", "")
	configHome(t)
	for _, tc := range []struct {
		name string
		toml string
		want []string
	}{
		{"unknown key", "provder = \"claude\"\n", []string{"unknown keys", "provder"}},
		{"unknown key in a table", "[github]\ntokn = \"x\"\n", []string{"unknown keys", "github.tokn"}},
		{"tui table is gone", "[tui]\ncolor = false\n", []string{"unknown keys", "tui"}},
		{"provider", "provider = \"gemini\"\n", []string{"provider \"gemini\" must be one of claude, codex"}},
		{"output", "output = \"merge\"\n", []string{"output \"merge\" must be one of ask, approve, comment, reject, report, discard"}},
		{"token variable", "[github]\ntoken = \"$REVIEW_UNSET_TOKEN\"\n", []string{"github.token reads $REVIEW_UNSET_TOKEN, which is not set"}},
		{"unknown per-step key", "angle_model = \"haiku\"\n", []string{"unknown keys", "angle_model"}},
		{"unknown size", "[angles]\nxxl = [\"general\"]\n", []string{"angles.xxl", "\"xxl\" is not a size"}},
		{"unknown angle in a size", "[angles]\nxs = [\"quick_general\", \"style\"]\n", []string{"angles.xs", "\"style\" is not an angle"}},
		{"empty size", "[angles]\nm = []\n", []string{"angles.m must name at least one angle"}},
		{"unknown angle model", "[angle_models]\nstyle = \"haiku\"\n", []string{"angle_models.style", "\"style\" is not an angle"}},
		{"empty angle model", "[angle_models]\ndocs = \"\"\n", []string{"angle_models.docs must name a model"}},
		{"pi under the claude sandbox", "[profiles.p]\nagent = \"pi\"\nsandbox = \"claude\"\n", []string{"profiles.p.sandbox", "\"pi\"", "none or container"}},
		{"token expands to nothing", "[github]\ntoken = \"$REVIEW_UNSET_TOKEN$REVIEW_UNSET_TOKEN\"\n", []string{"expands to nothing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ConfigFile)
			_, err := LoadConfig(writeFile(t, filepath.Dir(path), ConfigFile, tc.toml))
			if err == nil {
				t.Fatal("loaded without an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name the file", err)
			}
		})
	}
}

// Every problem in a file is reported, not only the first: fixing one and
// loading again to find the next is the loop this avoids.
func TestConfigReportsEveryProblem(t *testing.T) {
	_, err := ParseConfig("provider = \"gemini\"\noutput = \"merge\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err == nil {
		t.Fatal("loaded without an error")
	}
	if !strings.Contains(err.Error(), "provider") || !strings.Contains(err.Error(), "output") {
		t.Fatalf("error names one problem only: %q", err)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, want := DefaultConfigPath(), filepath.Join("/xdg", "bees", ConfigFile); got != want {
		t.Fatalf("XDG_CONFIG_HOME: %q, want %q", got, want)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	// An unset XDG_CONFIG_HOME falls back to ~/.config, and so does a
	// relative one: the spec says a relative value is to be ignored, and
	// resolving it against the working directory would put a person's
	// configuration wherever they happened to run bees from.
	for _, xdg := range []string{"", "relative/config"} {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		if got, want := DefaultConfigPath(), filepath.Join(home, ".config", "bees", ConfigFile); got != want {
			t.Fatalf("XDG_CONFIG_HOME %q: default path %q, want %q", xdg, got, want)
		}
	}
	// LoadConfig with no path reads that file, and the home directory of a
	// test has none, so it is the defaults.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Loaded || cfg.Path != DefaultConfigPath() {
		t.Fatalf("LoadConfig(\"\") read %q (loaded %v), want the default path", cfg.Path, cfg.Loaded)
	}
}

func TestConfigPathExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	cfg, err := ParseConfig("notes_path = \"~/notes.md\"\nstorage_path = \"~\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.ResolvedNotesPath(), filepath.Join(home, "notes.md"); got != want {
		t.Fatalf("notes path %q, want %q", got, want)
	}
	if got := cfg.ResolvedStoragePath(); got != home {
		t.Fatalf("storage path %q, want %q", got, home)
	}
}

// With no config.toml, a review runs on the reviewer settings defaults.toml
// holds, the same tables a bees.toml's [roles.reviewer] does: its profiles,
// its angles, its per-step profiles, and the base its roles.reviewer.profile
// selects ahead of global.profile.
func TestLoadConfigTakesTheUserDefaults(t *testing.T) {
	dir := configHome(t)
	writeDefaults(t, dir, defaultsReviewer)
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Loaded {
		t.Fatal("Loaded is true with no config.toml")
	}
	if cfg.Provider != "claude" || cfg.Model != "opus" {
		t.Fatalf("base = %s/%s, want the agent and model of roles.reviewer.profile", cfg.Provider, cfg.Model)
	}
	if got := strings.Join(chainOf(NewDistiller(cfg, "").Agent.(*CLIAgent)), " "); got != "claude/opus/high codex/gpt-5/low" {
		t.Errorf("brief = %q, want brief_profile's chain", got)
	}
	angles := NewAngles(cfg, "")
	for angle, want := range map[string]string{AngleDocs: "claude/sonnet/", AngleGeneral: "claude/sonnet/"} {
		agent, _ := angles.agentFor(angle)
		if got := strings.Join(chainOf(agent.(*CLIAgent)), " "); got != want {
			t.Errorf("%s = %q, want %q", angle, got, want)
		}
	}
	wantAngles := map[string][]string{"xs": {AngleDocs}, "m": {AngleGeneral, AngleDocs}}
	if !reflect.DeepEqual(cfg.Angles, wantAngles) {
		t.Errorf("angles = %v, want %v", cfg.Angles, wantAngles)
	}
	if cfg.BriefProfile != "user_brief" || cfg.JudgeProfile != "user_docs" {
		t.Errorf("brief/judge profile = %q/%q", cfg.BriefProfile, cfg.JudgeProfile)
	}
	if !reflect.DeepEqual(cfg.AngleProfiles, map[string]string{AngleDocs: "user_docs", AngleGeneral: "user_docs"}) {
		t.Errorf("angle_profiles = %v", cfg.AngleProfiles)
	}
	if len(cfg.Profiles) != 3 {
		t.Errorf("profiles = %d entries, want the defaults file's three", len(cfg.Profiles))
	}
}

// The nearer file wins per step, whatever form it uses: a step config.toml
// configures at all — a profile or a step model for the brief and the
// angles, the flat provider or model for the base — takes nothing from
// defaults.toml. The angles merge per size and the profiles per name
// whatever else is set.
func TestConfigTomlBeatsDefaultsPerStep(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config string
		check  func(t *testing.T, cfg *Config)
	}{
		{
			name:   "no config.toml runs the defaults throughout",
			config: "",
			check: func(t *testing.T, cfg *Config) {
				if got, want := strings.Join(chainOf(NewDistiller(cfg, "").Agent.(*CLIAgent)), " "), "claude/opus/high codex/gpt-5/low"; got != want {
					t.Errorf("brief = %q, want %q", got, want)
				}
				assertAngle(t, cfg, AngleDocs, "claude/sonnet/")
				assertAngle(t, cfg, AngleGeneral, "claude/sonnet/")
				if cfg.Provider != "claude" || cfg.Model != "opus" {
					t.Errorf("base = %s/%s, want roles.reviewer.profile's", cfg.Provider, cfg.Model)
				}
			},
		},
		{
			name: "flat keys block the base, and only the base",
			config: `provider = "codex"
model = "gpt-5"
`,
			check: func(t *testing.T, cfg *Config) {
				if cfg.Provider != "codex" || cfg.Model != "gpt-5" {
					t.Errorf("base = %s/%s, want the flat keys", cfg.Provider, cfg.Model)
				}
				if got, want := strings.Join(chainOf(NewDistiller(cfg, "").Agent.(*CLIAgent)), " "), "claude/opus/high codex/gpt-5/low"; got != want {
					t.Errorf("brief = %q, want the defaults profile", got)
				}
				assertAngle(t, cfg, AngleDocs, "claude/sonnet/")
			},
		},
		{
			name:   "a step model blocks the brief's profile, and only that",
			config: "brief_model = \"haiku\"\n",
			check: func(t *testing.T, cfg *Config) {
				if cfg.BriefProfile != "" {
					t.Errorf("brief_profile = %q, want none: the step model took the step", cfg.BriefProfile)
				}
				if got, want := strings.Join(chainOf(NewDistiller(cfg, "").Agent.(*CLIAgent)), " "), "claude/haiku/"; got != want {
					t.Errorf("brief = %q, want the step model", got)
				}
				assertAngle(t, cfg, AngleDocs, "claude/sonnet/")
				if cfg.Provider != "claude" || cfg.Model != "opus" {
					t.Errorf("base = %s/%s, want roles.reviewer.profile's", cfg.Provider, cfg.Model)
				}
			},
		},
		{
			name: "the brief's profile in config.toml wins over the defaults one",
			config: `brief_profile = "config_brief"
[profiles.config_brief]
agent = "codex"
model = "o3"
`,
			check: func(t *testing.T, cfg *Config) {
				if got, want := strings.Join(chainOf(NewDistiller(cfg, "").Agent.(*CLIAgent)), " "), "codex/o3/"; got != want {
					t.Errorf("brief = %q, want %q", got, want)
				}
				assertAngle(t, cfg, AngleDocs, "claude/sonnet/")
			},
		},
		{
			name: "an angle model blocks that angle's profile, and only that angle's",
			config: `[angle_models]
docs = "haiku"
`,
			check: func(t *testing.T, cfg *Config) {
				assertAngle(t, cfg, AngleDocs, "claude/haiku/")
				assertAngle(t, cfg, AngleGeneral, "claude/sonnet/")
				if got := cfg.AngleProfiles[AngleGeneral]; got != "user_docs" {
					t.Errorf("angle_profiles.general = %q, want the defaults one", got)
				}
				if _, blocked := cfg.AngleProfiles[AngleDocs]; blocked {
					t.Errorf("angle_profiles.docs is set, want none: the step model took the step")
				}
			},
		},
		{
			name: "the angles merge per size",
			config: `[angles]
xs = ["general"]
`,
			check: func(t *testing.T, cfg *Config) {
				want := map[string][]string{"xs": {AngleGeneral}, "m": {AngleGeneral, AngleDocs}}
				if !reflect.DeepEqual(cfg.Angles, want) {
					t.Errorf("angles = %v, want %v", cfg.Angles, want)
				}
			},
		},
		{
			name: "a same-named profile replaces the defaults one whole",
			config: `[profiles.user_brief]
agent = "codex"
model = "config-model"
`,
			check: func(t *testing.T, cfg *Config) {
				if got, want := strings.Join(chainOf(NewDistiller(cfg, "").Agent.(*CLIAgent)), " "), "codex/config-model/"; got != want {
					t.Errorf("brief = %q, want the config.toml profile alone, its fallback gone with it", got)
				}
			},
		},
		{
			name:   "the judge's profile in config.toml wins",
			config: "judge_profile = \"j\"\n[profiles.j]\n",
			check: func(t *testing.T, cfg *Config) {
				if cfg.JudgeProfile != "j" {
					t.Errorf("judge_profile = %q", cfg.JudgeProfile)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := configHome(t)
			writeDefaults(t, dir, defaultsReviewer)
			var cfg *Config
			var err error
			if tc.config == "" {
				cfg, err = LoadConfig(filepath.Join(t.TempDir(), ConfigFile))
			} else {
				cfg, err = LoadConfig(writeFile(t, t.TempDir(), ConfigFile, tc.config))
			}
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, cfg)
		})
	}
}

// assertAngle is the agent the angle's session runs as, one "provider/model/effort" per chain link.
func assertAngle(t *testing.T, cfg *Config, angle, want string) {
	t.Helper()
	angles := NewAngles(cfg, "")
	agent, _ := angles.agentFor(angle)
	if got := strings.Join(chainOf(agent.(*CLIAgent)), " "); got != want {
		t.Errorf("%s = %q, want %q", angle, got, want)
	}
}

// The base comes from global.profile when roles.reviewer.profile names none.
func TestDefaultsBaseFallsBackToTheGlobalProfile(t *testing.T) {
	dir := configHome(t)
	writeDefaults(t, dir, `version = 5
[profiles.global_p]
agent = "codex"
model = "gpt-5"
[global]
profile = "global_p"
[roles.reviewer]
brief_profile = "global_p"
`)
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "codex" || cfg.Model != "gpt-5" {
		t.Fatalf("base = %s/%s, want global.profile's agent and model", cfg.Provider, cfg.Model)
	}
}

// A missing defaults.toml changes nothing, with and without a config.toml.
func TestLoadConfigWithoutDefaults(t *testing.T) {
	configHome(t)
	cfg, err := LoadConfig(filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Loaded || cfg.Provider != DefaultProvider || cfg.Model != DefaultModel || len(cfg.Profiles) != 0 ||
		cfg.BriefProfile != "" || cfg.JudgeProfile != "" || len(cfg.AngleProfiles) != 0 || len(cfg.Angles) != 0 {
		t.Fatalf("no defaults.toml is not the built-in defaults: %+v", cfg)
	}
	cfg, err = LoadConfig(writeFile(t, t.TempDir(), ConfigFile, "output = \"report\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != DefaultProvider || cfg.Model != DefaultModel || len(cfg.Profiles) != 0 {
		t.Fatalf("no defaults.toml changed a config.toml that sets none of its keys: %+v", cfg)
	}
}

func TestDefaultsErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		defaults string
		config   string
		want     []string
		notWant  string
	}{
		{
			name:     "unknown brief profile",
			defaults: "version = 5\n[roles.reviewer]\nbrief_profile = \"ghost\"\n",
			want:     []string{`defaults.toml: brief_profile: unknown profile "ghost" (declare it under [profiles.ghost])`},
		},
		{
			name:     "unknown base profile",
			defaults: "version = 5\n[roles.reviewer]\nprofile = \"ghost\"\n",
			want:     []string{`defaults.toml: roles.reviewer.profile: unknown profile "ghost"`},
		},
		{
			name:     "base profile on an agent reviews cannot run",
			defaults: "version = 5\n[profiles.oc]\nagent = \"opencode\"\n[roles.reviewer]\nprofile = \"oc\"\n",
			want:     []string{"defaults.toml: provider \"opencode\" must be one of claude, codex"},
		},
		{
			name:     "bad value in a defaults profile",
			defaults: "version = 5\n[profiles.bad]\neffort = \"huge\"\n",
			want:     []string{"defaults.toml: profiles.bad.effort must be low, medium, high or max"},
		},
		{
			name:     "refused key",
			defaults: "version = 5\n[roles.developer]\nmax_turns = 4\n",
			want:     []string{"roles.developer.max_turns is not allowed"},
		},
		{
			name:     "an error of config.toml's own is not attributed to the defaults file",
			defaults: defaultsReviewer,
			config:   "provider = \"gemini\"\n",
			want:     []string{"provider \"gemini\" must be one of claude, codex"},
			notWant:  "defaults.toml",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := configHome(t)
			writeDefaults(t, dir, tc.defaults)
			// The config.toml to read is not there unless the case writes
			// one; the defaults file is read from the home, never as the
			// config.
			path := filepath.Join(t.TempDir(), ConfigFile)
			if tc.config != "" {
				writeFile(t, filepath.Dir(path), ConfigFile, tc.config)
			}
			_, err := LoadConfig(path)
			if err == nil {
				t.Fatal("loaded without an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
			if tc.notWant != "" && strings.Contains(err.Error(), tc.notWant) {
				t.Fatalf("error %q names %q", err, tc.notWant)
			}
		})
	}
}

// TestSupportedProvidersMatchesWhatCommandImplements pins the provider list
// Config.Validate() and CLIAgent.command()'s error both read from. Widening
// config.Agents (a factory session backend) must not, by itself, widen this
// list: that only happens when someone adds the matching case to command()
// in agent.go.
func TestSupportedProvidersMatchesWhatCommandImplements(t *testing.T) {
	want := []string{"claude", "codex"}
	if !slices.Equal(SupportedProviders, want) {
		t.Fatalf("SupportedProviders = %v, want %v", SupportedProviders, want)
	}
}
