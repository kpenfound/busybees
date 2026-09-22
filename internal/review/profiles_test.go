package review

import (
	"context"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// reviewerSection is the reviewer section of a bees.toml, profiles and all,
// the way #789 wrote it with fallback naming a profile.
const reviewerSection = `
[profiles.default]
agent = "claude"
model = "opus"
fallback = "spare"
effort = "high"
sandbox = "none"

[profiles.review_fast]
agent = "claude"
model = "opus"
fallback = "spare"
effort = "medium"
sandbox = "none"

[profiles.spare]
agent = "codex"
model = "gpt-5"
effort = "low"

[profiles.judge]
agent = "opencode"
model = "ollama/qwen"
`

const reviewerSelection = `brief_profile = "default"
judge_profile = "judge"
angle_profiles = { quick_general = "review_fast", docs = "review_fast", test_coverage = "default" }
`

// chainOf is an agent and its fallbacks, one "provider/model/effort" each.
func chainOf(a *CLIAgent) []string {
	var out []string
	for ; a != nil; a = a.Fallback {
		out = append(out, a.Provider+"/"+a.Model+"/"+a.Effort)
	}
	return out
}

// chainOfRole is the same for a bees.toml role as the factory's reviewer
// runs its brief and angle sessions.
func chainOfRole(r config.ResolvedRole) []string {
	out := []string{r.Agent + "/" + r.Model + "/" + r.Effort}
	for _, f := range r.Fallbacks() {
		out = append(out, f.Agent+"/"+f.Model+"/"+f.Effort)
	}
	return out
}

func mustParseConfig(t *testing.T, text string) *Config {
	t.Helper()
	cfg, err := ParseConfig(text, filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A bees.toml reviewer setup moved into config.toml, its [profiles.*] tables
// as they are and its three selectors taken out of [roles.reviewer] to the
// top level, gives every step the agent, model, effort and fallback chain
// the factory gives it. The same setup in defaults.toml, below a config.toml
// that is not there, gives every step the same chains.
func TestBeesTomlReviewerProfilesMovedToConfigToml(t *testing.T) {
	bees, err := config.Parse("version = 5\n"+reviewerSection+"\n[roles.reviewer]\n"+reviewerSelection, filepath.Join(t.TempDir(), "bees.toml"))
	if err != nil {
		t.Fatal(err)
	}
	role, err := bees.Role(config.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	cfg := mustParseConfig(t, reviewerSelection+reviewerSection)

	brief := NewDistiller(cfg, "").Agent.(*CLIAgent)
	if got, want := chainOf(brief), chainOfRole(role.ForBrief()); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("brief = %q, bees.toml runs %q", got, want)
	}
	angles := NewAngles(cfg, "")
	for angle := range cfg.AngleProfiles {
		agent, _ := angles.agentFor(angle)
		if got, want := chainOf(agent.(*CLIAgent)), chainOfRole(role.ForAngle(angle)); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s = %q, bees.toml runs %q", angle, got, want)
		}
	}
	// The chain carries what each profile says, not the first one's.
	if got := chainOf(brief); strings.Join(got, " ") != "claude/opus/high codex/gpt-5/low" {
		t.Errorf("brief chain = %q", got)
	}
	// judge_profile loads and changes nothing: the judge is not a session.
	if cfg.JudgeProfile != "judge" {
		t.Errorf("judge_profile = %q", cfg.JudgeProfile)
	}
	if angles.Provider != DefaultProvider || angles.Model != DefaultModel {
		t.Errorf("an angle no profile names runs as %s/%s, want the defaults", angles.Provider, angles.Model)
	}

	dir := configHome(t)
	writeDefaults(t, dir, "version = 5\n"+reviewerSection+"[roles.reviewer]\n"+reviewerSelection)
	fromDefaults, err := LoadConfig(filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if fromDefaults.Loaded {
		t.Fatal("Loaded is true with no config.toml")
	}
	defaultsBrief := NewDistiller(fromDefaults, "").Agent.(*CLIAgent)
	if got, want := chainOf(defaultsBrief), chainOf(brief); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("brief from defaults.toml = %q, config.toml runs %q", got, want)
	}
	defaultsAngles := NewAngles(fromDefaults, "")
	for angle := range cfg.AngleProfiles {
		agent, _ := defaultsAngles.agentFor(angle)
		if got, want := chainOf(agent.(*CLIAgent)), chainOfRole(role.ForAngle(angle)); strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s = %q, bees.toml runs %q", angle, got, want)
		}
	}
	if fromDefaults.BriefProfile != cfg.BriefProfile || !reflect.DeepEqual(fromDefaults.AngleProfiles, cfg.AngleProfiles) ||
		!reflect.DeepEqual(fromDefaults.Profiles, cfg.Profiles) {
		t.Errorf("defaults.toml projected %+v, config.toml holds %+v", fromDefaults, cfg)
	}
}

// A step with a profile runs as the profile, whatever the flat keys say for
// it; a step without one keeps the flat keys.
func TestAProfileWinsOverTheFlatKeys(t *testing.T) {
	cfg := mustParseConfig(t, `provider = "claude"
model = "opus"
brief_model = "sonnet"
brief_profile = "code"
angle_profiles = { docs = "code" }

[angle_models]
docs = "haiku"
general = "haiku"

[profiles.code]
agent = "codex"
model = "gpt-5"
effort = "max"
`)
	if got := chainOf(NewDistiller(cfg, "").Agent.(*CLIAgent)); strings.Join(got, " ") != "codex/gpt-5/max" {
		t.Errorf("brief = %q, want the profile over brief_model", got)
	}
	angles := NewAngles(cfg, "")
	for angle, want := range map[string]string{AngleDocs: "codex/gpt-5/max", AngleGeneral: "claude/haiku/", AngleSideEffects: "claude/opus/"} {
		agent, model := angles.agentFor(angle)
		if got := strings.Join(chainOf(agent.(*CLIAgent)), " "); got != want {
			t.Errorf("%s = %q, want %q", angle, got, want)
		}
		if got := angles.providerFor(angle) + "/" + model; !strings.HasPrefix(want, got) {
			t.Errorf("%s is recorded as %q, want the agent that runs, %q", angle, got, want)
		}
	}
}

// An angle on a profile runs as that profile through the real command line,
// its fallback given to claude as --fallback-model, and its session is
// resumed as the profile's agent, not the file's provider.
func TestAnAngleProfileRunsAndResumesAsItsAgent(t *testing.T) {
	cfg := mustParseConfig(t, `provider = "codex"
angle_profiles = { docs = "deep" }

[profiles.deep]
model = "opus"
effort = "high"
sandbox = "container"
fallback = "light"

[profiles.light]
model = "sonnet"
`)
	bin, record := fakeCLI(t, claudeAnswer)
	angles := NewAngles(cfg, t.TempDir())
	angles.Checkout = nil
	angles.Agents[AngleDocs].ClaudeBin = bin
	runs, err := angles.Run(context.Background(), t.TempDir(), onlyAngle(t, AngleDocs), testBrief(), testDiff)
	if err != nil {
		t.Fatal(err)
	}
	got := args(t, record)
	for _, want := range []string{"\n--model\nopus\n", "\n--effort\nhigh\n", "\n--fallback-model\nsonnet\n", "\n--permission-prompts\nnone\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("the docs session was not run with %q:\n%s", strings.TrimSpace(want), got)
		}
	}
	if len(runs) != 1 || runs[0].Provider != config.AgentClaude || runs[0].Model != "opus" {
		t.Fatalf("runs = %+v, want the docs run recorded as claude/opus", runs)
	}
	if _, err := angles.Resume(context.Background(), runs[0], "why?"); err != nil {
		t.Fatalf("resume on the profile's agent: %v", err)
	}
	if got := args(t, record); !strings.Contains(got, "\n--resume\nsess-1\n") {
		t.Errorf("the docs session was not resumed:\n%s", got)
	}
}

func TestProfileErrors(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		want []string
	}{
		{"unknown brief profile", "brief_profile = \"fast\"\n", []string{`brief_profile: unknown profile "fast" (declare it under [profiles.fast])`}},
		{"unknown judge profile", "judge_profile = \"fast\"\n", []string{`judge_profile: unknown profile "fast"`}},
		{"unknown angle profile", "angle_profiles = { docs = \"fast\" }\n", []string{`angle_profiles.docs: unknown profile "fast"`}},
		{"empty angle profile", "angle_profiles = { docs = \"\" }\n", []string{`angle_profiles.docs: unknown profile ""`}},
		{"angle profile for no angle", "angle_profiles = { style = \"a\" }\n[profiles.a]\n", []string{"angle_profiles.style", `"style" is not an angle`}},
		{"unknown profile key", "[profiles.a]\nmodle = \"opus\"\n", []string{"unknown keys", "profiles.a.modle"}},
		{"effort", "[profiles.a]\neffort = \"huge\"\n", []string{"profiles.a.effort must be low, medium, high or max"}},
		{"agent", "[profiles.a]\nagent = \"gemini\"\n", []string{"profiles.a.agent must be one of"}},
		{"sandbox", "[profiles.a]\nsandbox = \"jail\"\n", []string{"profiles.a.sandbox must be one of"}},
		{"unknown fallback", "[profiles.a]\nfallback = \"b\"\n", []string{`profiles.a.fallback: unknown profile "b"`}},
		{"fallback cycle", "[profiles.a]\nfallback = \"b\"\n[profiles.b]\nfallback = \"a\"\n", []string{`comes back to "a"`}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ConfigFile)
			_, err := ParseConfig(tc.toml, path)
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
	// The judge is no session, so its profile may run any agent, as in
	// bees.toml; a profile nothing selects is only held to the table's checks.
	mustParseConfig(t, "judge_profile = \"a\"\n[profiles.a]\nagent = \"opencode\"\n[profiles.b]\nagent = \"opencode\"\n")
}

// The agent a session's profile runs is checked against the providers the
// shared restricted execution supports, the whole fallback chain with it.
// The table's rows cannot reach this check — ValidateProfiles rejects an
// agent outside the four before it — so it is driven on a configuration
// built by hand, the way a factory agent added without restricted support
// would reach it.
func TestASessionsProfilesMustRunSupportedProviders(t *testing.T) {
	cfg := &Config{Profiles: map[string]config.AgentProfile{
		"a": {Agent: "gemini", Model: "gemini-pro"},
		"b": {Agent: "claude", Fallback: "c"},
		"c": {Agent: "gemini"},
	}}
	for _, agent := range SupportedProviders {
		cfg.Profiles["ok"] = config.AgentProfile{Agent: agent}
		if errs := cfg.checkProfile("brief_profile", "ok", true); len(errs) != 0 {
			t.Errorf("a profile on %s was refused: %v", agent, errs)
			delete(cfg.Profiles, "ok")
		}
	}
	errs := cfg.checkProfile("brief_profile", "a", true)
	if len(errs) != 1 || !strings.Contains(errs[0], `brief_profile: review sessions run as one of claude, codex, opencode, pi, and profile "a" runs "gemini"`) {
		t.Fatalf("errors = %v, want the profile's agent named", errs)
	}
	errs = cfg.checkProfile("angle_profiles.docs", "b", true)
	if len(errs) != 1 || !strings.Contains(errs[0], `its fallback profile "c" runs "gemini"`) {
		t.Fatalf("errors = %v, want the fallback profile named", errs)
	}
	if errs := cfg.checkProfile("judge_profile", "b", false); len(errs) != 0 {
		t.Errorf("the judge's profile is no session's: %v", errs)
	}
}
