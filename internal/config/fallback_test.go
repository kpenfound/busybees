package config

import (
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
)

// fallback names another profile, and the chain it starts has to end: a name
// that is no profile, a profile naming itself and a longer cycle all fail to
// load with the key and what to change.
func TestFallbackValidation(t *testing.T) {
	for _, tc := range []struct{ name, body, want string }{
		{"unknown", "[profiles.a]\nfallback = \"missing\"\n", `profiles.a.fallback: unknown profile "missing" (declare it under [profiles.missing])`},
		{"self", "[profiles.a]\nfallback = \"a\"\n", "profiles.a.fallback: a profile cannot be its own fallback (name another profile, or remove the key)"},
		{"cycle of two", "[profiles.a]\nfallback = \"b\"\n[profiles.b]\nfallback = \"a\"\n", `profiles.a.fallback: fallback chain a -> b -> a comes back to "a" (end the chain at a profile without a fallback)`},
		{"cycle further down", "[profiles.a]\nfallback = \"b\"\n[profiles.b]\nfallback = \"c\"\n[profiles.c]\nfallback = \"b\"\n", `profiles.a.fallback: fallback chain a -> b -> c -> b comes back to "b"`},
		{"unknown further down", "[profiles.a]\nfallback = \"b\"\n[profiles.b]\nfallback = \"missing\"\n", `profiles.a.fallback: profiles.b.fallback: unknown profile "missing"`},
		{"self further down", "[profiles.a]\nfallback = \"b\"\n[profiles.b]\nfallback = \"b\"\n", `profiles.a.fallback: profiles.b.fallback: a profile cannot be its own fallback`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeConfig(t, "version = 5\n"+tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
	// The chain is walked from every profile, so a cycle is reported on each
	// profile in it and not on one that only leads into it twice.
	_, err := Load(writeConfig(t, "version = 5\n[profiles.a]\nfallback = \"b\"\n[profiles.b]\nfallback = \"a\"\n"))
	if err == nil || strings.Count(err.Error(), "comes back to") != 2 {
		t.Fatalf("cycle reported on every profile in it: %v", err)
	}
	// A chain that ends loads, however long.
	if _, err := Load(writeConfig(t, "version = 5\n[profiles.a]\nfallback = \"b\"\n[profiles.b]\nfallback = \"c\"\n[profiles.c]\n")); err != nil {
		t.Fatal(err)
	}
}

const fallbackChainTOML = `version = 5
[profiles.main]
agent = "claude"
model = "opus"
fallback = "mid"
effort = "high"
sandbox = "claude"
[profiles.mid]
agent = "codex"
model = "gpt"
fallback = "last"
effort = "low"
[profiles.last]
agent = "opencode"
model = "ollama/x"
[profiles.sized]
agent = "claude"
model = "haiku"
fallback = "last"
[profiles.code]
agent = "codex"
[profiles.review]
agent = "claude"
fallback = "code"
[roles.developer]
profile = "main"
profile_by_size = { xs = "sized" }
max_turns = 7
prompt = "developer prompt"
[roles.reviewer]
profile = "code"
brief_profile = "review"
judge_profile = "last"
`

// A role carries its fallback chain: the role as it runs on each profile
// the chain names, agent included, with every other setting of the role
// unchanged, and the chain of whatever profile a size or a phase selected.
func TestFallbackChainResolution(t *testing.T) {
	cfg, err := Load(writeConfig(t, fallbackChainTOML))
	if err != nil {
		t.Fatal(err)
	}
	r, err := cfg.Role(RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if r.Fallback != "mid" {
		t.Fatalf("fallback: %q", r.Fallback)
	}
	f := r.Fallbacks()
	if len(f) != 2 {
		t.Fatalf("chain: %+v", f)
	}
	if got := profileOf(f[0]); got != (AgentProfile{Agent: "codex", Model: "gpt", Fallback: "last", Effort: "low", Sandbox: "none"}) {
		t.Errorf("first fallback: %+v", got)
	}
	if got := profileOf(f[1]); got != (AgentProfile{Agent: "opencode", Model: "ollama/x", Sandbox: "none"}) {
		t.Errorf("second fallback: %+v", got)
	}
	for _, step := range f {
		if step.MaxTurns != 7 || step.Prompt != "developer prompt" || step.Name != RoleDeveloper {
			t.Errorf("a fallback lost the role's settings: %+v", step)
		}
	}
	// The size's profile starts its own chain.
	xs := r.ForSize("xs")
	if xs.Fallback != "last" || len(xs.Fallbacks()) != 1 || xs.Fallbacks()[0].Agent != AgentOpenCode {
		t.Errorf("sized chain: %q %+v", xs.Fallback, xs.Fallbacks())
	}
	// So does a phase's, and a profile without a fallback has no chain.
	rev, _ := cfg.Role(RoleReviewer)
	if b := rev.ForBrief(); b.Fallback != "code" || len(b.Fallbacks()) != 1 || b.Fallbacks()[0].Agent != AgentCodex {
		t.Errorf("brief chain: %q %+v", b.Fallback, b.Fallbacks())
	}
	if j := rev.ForJudge(); j.Fallback != "" || j.Fallbacks() != nil {
		t.Errorf("judge chain: %q %+v", j.Fallback, j.Fallbacks())
	}
	if (ResolvedRole{}).Fallbacks() != nil {
		t.Error("a bare role has a chain")
	}
	// bees config show names the fallback the way the file does.
	v, err := cfg.View([]string{RoleDeveloper})
	if err != nil {
		t.Fatal(err)
	}
	if v.Roles[RoleDeveloper].Fallback != "mid" || v.Roles[RoleDeveloper].ProfilesBySize["xs"].Fallback != "last" {
		t.Errorf("show: %+v", v.Roles[RoleDeveloper])
	}
}

// A chain the table would not have loaded still ends: a name that is no
// profile stops it, and a name already in it does too.
func TestFallbackChainEndsOnItsOwn(t *testing.T) {
	profiles := map[string]AgentProfile{"a": {Fallback: "b"}, "b": {Fallback: "a"}, "c": {Fallback: "missing"}}
	if got := fallbackChain(profiles, "a", "b"); !reflect.DeepEqual(got, []string{"b"}) {
		t.Errorf("cycle: %v", got)
	}
	if got := fallbackChain(profiles, "c", "missing"); got != nil {
		t.Errorf("unknown: %v", got)
	}
	if got := fallbackChain(profiles, "", "c"); !reflect.DeepEqual(got, []string{"c"}) {
		t.Errorf("from the implicit profile: %v", got)
	}
}

// Brief and angle profiles use the restricted capability declared by the
// shared agent backend, including every profile in the fallback chain.
func TestReviewPhaseProfilesAcceptRestrictedBackends(t *testing.T) {
	for _, backend := range []string{AgentOpenCode, AgentPi} {
		for _, selection := range []string{`brief_profile = "selected"`, `angle_profiles.docs = "selected"`} {
			t.Run(backend+"/"+selection, func(t *testing.T) {
				body := fmt.Sprintf("version = 5\n[profiles.selected]\nagent = %q\n[roles.reviewer]\n%s\n", backend, selection)
				if _, err := Load(writeConfig(t, body)); err != nil {
					t.Fatalf("restricted backend %q should be accepted: %v", backend, err)
				}
			})
		}
		// A selected profile may fall back to another supported restricted backend.
		body := fmt.Sprintf("version = 5\n[profiles.selected]\nagent = %q\nfallback = \"fallback\"\n[profiles.fallback]\nagent = %q\n[roles.reviewer]\nbrief_profile = \"selected\"\n", backend, backend)
		if _, err := Load(writeConfig(t, body)); err != nil {
			t.Errorf("restricted fallback %q should be accepted: %v", backend, err)
		}
	}
}

func TestReviewPhaseProfilesRejectUnsupportedBackend(t *testing.T) {
	_, err := Load(writeConfig(t, "version = 5\n[profiles.unsupported]\nagent = \"other\"\n[roles.reviewer]\nbrief_profile = \"unsupported\"\n"))
	if err == nil || !strings.Contains(err.Error(), `brief_profile`) || !strings.Contains(err.Error(), `restricted execution`) {
		t.Fatalf("unsupported reviewer backend should be rejected by restricted capability validation: %v", err)
	}
}

// Version 4 -> 5: a profile's fallback_model becomes a fallback profile of
// its own, <name>_fallback (the next free name when that is taken), the
// profile with that model and no fallback, and the migrated file behaves as
// it did: the same chain, one profile long. A codex or opencode profile whose
// fallback_model was empty had none and loses the key. Comments, an example
// inside a prompt and the rest of the file survive.
func TestFallbackProfileMigration(t *testing.T) {
	source := `version = 4
# top comment
[profiles.main]
agent = "claude"
model = "opus"
fallback_model = "sonnet" # keep me
effort = "high"
sandbox = "claude"
[profiles.plain]
model = "haiku"
[profiles.code]
agent = "codex"
model = "gpt"
fallback_model = ""
[profiles.main_fallback]
model = "taken"
[global]
profile = "main"
prompt = """
fallback_model = "example"
"""
`
	path := writeConfig(t, source)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MigratedFrom != 4 || !cfg.NeedsRewrite() {
		t.Fatalf("migrated from %d", cfg.MigratedFrom)
	}
	r, _ := cfg.Role(RoleDeveloper)
	if got := profileOf(r); got != (AgentProfile{Agent: "claude", Model: "opus", Fallback: "main_fallback_2", Effort: "high", Sandbox: "claude"}) {
		t.Errorf("main: %+v", got)
	}
	if f := r.Fallbacks(); len(f) != 1 || profileOf(f[0]) != (AgentProfile{Agent: "claude", Model: "sonnet", Effort: "high", Sandbox: "claude"}) {
		t.Errorf("chain: %+v", f)
	}
	for name, want := range map[string]AgentProfile{
		"plain":         {Agent: "claude", Model: "haiku", Sandbox: "none"},
		"code":          {Agent: "codex", Model: "gpt", Sandbox: "none"},
		"main_fallback": {Agent: "claude", Model: "taken", Sandbox: "none"},
	} {
		if got := cfg.Profiles[name].resolved(); got != want {
			t.Errorf("%s: %+v want %+v", name, got, want)
		}
	}
	if len(cfg.Profiles) != 5 {
		t.Errorf("profiles: %+v", cfg.Profiles)
	}
	if _, err := cfg.Rewrite(); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	text := string(b)
	for _, kept := range []string{"# top comment", "# Previous: fallback_model = \"sonnet\" # keep me", "\nfallback = \"main_fallback_2\"\n", "# Previous: fallback_model = \"\"\n", "\n[profiles.main_fallback_2]\nagent = \"claude\"\nmodel = \"sonnet\"\neffort = \"high\"\nsandbox = \"claude\"\n", "fallback_model = \"example\"\n\"\"\"", "version = 5\n"} {
		if !strings.Contains(text, kept) {
			t.Errorf("missing %q in:\n%s", kept, text)
		}
	}
	// The one fallback_model left is the example inside the prompt.
	if n := strings.Count(text, "\nfallback_model"); n != 1 {
		t.Errorf("%d fallback_model lines survived:\n%s", n, text)
	}
	again, err := Load(path)
	if err != nil || again.NeedsRewrite() {
		t.Fatalf("reload: %v", err)
	}
	if rerole, _ := again.Role(RoleDeveloper); !reflect.DeepEqual(r, rerole) {
		t.Fatal("rewrite changed resolution")
	}
	if twice, err := migrateFallbackProfiles(text); err != nil || twice != text {
		t.Fatalf("not idempotent: %v\n%s", err, twice)
	}
}

// The key is rewritten wherever TOML lets a profile be written: under its
// own header, as dotted keys, as an inline table under [profiles] and inside
// a profiles = { ... } inline table, which the new profile joins because no
// table header may extend an inline table.
func TestFallbackProfileMigrationShapes(t *testing.T) {
	for _, body := range []string{
		"[profiles.main]\nfallback_model = \"sonnet\" # inline comment\nmodel = \"opus\"\n",
		"profiles.main.fallback_model = \"sonnet\"\nprofiles.main.model = \"opus\"\n",
		"[profiles]\nmain = { model = \"opus\", fallback_model = \"sonnet\" }\n",
		"profiles = { main = { model = \"opus\", fallback_model = \"sonnet\" }, other = { agent = \"codex\" } }\n",
	} {
		t.Run(body, func(t *testing.T) {
			path := writeConfig(t, "version = 4\n"+body+"[global]\nprofile = \"main\"\n")
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			r, _ := cfg.Role(RoleQA)
			if r.Fallback != "main_fallback" || r.Model != "opus" {
				t.Fatalf("main: %+v", profileOf(r))
			}
			if f := r.Fallbacks(); len(f) != 1 || profileOf(f[0]) != (AgentProfile{Agent: "claude", Model: "sonnet", Sandbox: "none"}) {
				t.Fatalf("chain: %+v", f)
			}
			if _, err := cfg.Rewrite(); err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(path)
			if strings.Contains(body, "# inline comment") && !strings.Contains(string(b), "# inline comment") {
				t.Errorf("lost the comment:\n%s", b)
			}
			again, err := Load(path)
			if err != nil || again.NeedsRewrite() {
				t.Fatalf("reload: %v\n%s", err, b)
			}
			if rerole, _ := again.Role(RoleQA); !reflect.DeepEqual(r, rerole) {
				t.Fatalf("rewrite changed resolution:\n%s", b)
			}
			if twice, err := migrateFallbackProfiles(string(b)); err != nil || twice != string(b) {
				t.Fatalf("not idempotent: %v\n%s", err, twice)
			}
		})
	}
}

// A commented-out fallback_model, the template's commented default, is kept
// and told what replaced it, once.
func TestFallbackProfileMigrationTellsCommentedDefaults(t *testing.T) {
	cfg, err := Load(writeConfig(t, "version = 4\n#[profiles.default]\n#agent = \"claude\"\n#model = \"opus\"\n#fallback_model = \"sonnet\"\n#effort = \"\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := "#model = \"opus\"\n# fallback_model was replaced by fallback, which names another profile (version 5).\n#fallback_model = \"sonnet\"\n#effort = \"\"\n"
	if !strings.Contains(cfg.migrated, want) {
		t.Fatalf("migrated:\n%s", cfg.migrated)
	}
	if twice, err := migrateFallbackProfiles(cfg.migrated); err != nil || twice != cfg.migrated {
		t.Fatalf("not idempotent: %v\n%s", err, twice)
	}
}

// A file that has both keys on one profile is refused rather than guessed
// at: fallback_model is the one to remove.
func TestFallbackProfileMigrationRefusesBothKeys(t *testing.T) {
	_, err := Load(writeConfig(t, "version = 4\n[profiles.a]\nfallback_model = \"x\"\nfallback = \"b\"\n[profiles.b]\n"))
	if err == nil || !strings.Contains(err.Error(), "profiles.a: cannot mix fallback_model and fallback; remove fallback_model") {
		t.Fatalf("error: %v", err)
	}
}

// ValidateProfiles is bees.toml's check of [profiles] on a table of its own,
// for another file that describes sessions with profiles: the same problems,
// named by the same keys, and nothing about roles.
func TestValidateProfilesOnATableOfItsOwn(t *testing.T) {
	for _, tc := range []struct {
		name     string
		profiles map[string]AgentProfile
		want     string
	}{
		{"effort", map[string]AgentProfile{"a": {Effort: "huge"}}, "profiles.a.effort must be low, medium, high or max"},
		{"agent", map[string]AgentProfile{"a": {Agent: "gemini"}}, "profiles.a.agent must be one of"},
		{"sandbox", map[string]AgentProfile{"a": {Sandbox: "jail"}}, "profiles.a.sandbox must be one of"},
		{"unknown fallback", map[string]AgentProfile{"a": {Fallback: "b"}}, `profiles.a.fallback: unknown profile "b"`},
		{"cycle", map[string]AgentProfile{"a": {Fallback: "b"}, "b": {Fallback: "a"}}, `comes back to "a"`},
		{"empty name", map[string]AgentProfile{"": {}}, "profiles: profile name must not be empty"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := ValidateProfiles(tc.profiles)
			if len(errs) == 0 || !strings.Contains(strings.Join(errs, "\n"), tc.want) {
				t.Fatalf("want %q, got %q", tc.want, errs)
			}
		})
	}
	if errs := ValidateProfiles(map[string]AgentProfile{"a": {Agent: AgentCodex, Effort: "max", Sandbox: SandboxNone, Fallback: "b"}, "b": {}}); len(errs) != 0 {
		t.Fatalf("a valid table: %q", errs)
	}
}

// ProfileChain is the named profile and every profile its fallback chain
// runs through, each resolved.
func TestProfileChain(t *testing.T) {
	cfg, err := Load(writeConfig(t, fallbackChainTOML))
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, p := range ProfileChain(cfg.Profiles, "main") {
		got = append(got, p.Agent+"/"+p.Model+"/"+p.Effort+"/"+p.Sandbox)
	}
	want := []string{"claude/opus/high/claude", "codex/gpt/low/" + DefaultSandbox, "opencode/ollama/x//" + DefaultSandbox}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("chain = %q, want %q", got, want)
	}
	// Resolved: a claude profile that names no model gets the default one.
	if chain := ProfileChain(cfg.Profiles, "review"); len(chain) != 2 || chain[0].Model != DefaultModel || chain[1].Agent != AgentCodex {
		t.Fatalf("review chain = %+v", chain)
	}
	if chain := ProfileChain(cfg.Profiles, "missing"); chain != nil {
		t.Fatalf("a profile that is not there has no chain: %+v", chain)
	}
	// A cycle ends where validation would have refused it.
	cyclic := map[string]AgentProfile{"a": {Fallback: "b"}, "b": {Fallback: "a"}}
	if chain := ProfileChain(cyclic, "a"); len(chain) != 2 {
		t.Fatalf("cyclic chain = %+v", chain)
	}
}
