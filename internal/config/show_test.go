package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// viewJSON loads a config, renders it through View and decodes the result the
// way `bees config show` prints it. XDG_CONFIG_HOME points at an empty
// directory: the views describe the file alone, not whatever defaults.toml
// the machine running the tests has.
func viewJSON(t *testing.T, body string) map[string]any {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := Load(writeConfig(t, body))
	if err != nil {
		t.Fatal(err)
	}
	v, err := cfg.View(Roles)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func roleOf(t *testing.T, out map[string]any, role string) map[string]any {
	t.Helper()
	r, ok := out["roles"].(map[string]any)[role].(map[string]any)
	if !ok {
		t.Fatalf("no role %q in %v", role, out["roles"])
	}
	return r
}

const showTOML = `version = 1
[project]
repo = "a/b"
default_branch = "main"
[roles.reviewer]
auto_merge = true
merge_method = "rebase"
checks_wait = "5s"
[roles.developer]
commit_flags = "-S"
max_size = "m"
model_by_size = { xs = "haiku" }
best_of_n_by_size = { l = 3 }
best_of_n_model = "opus"
best_of_n_prompt = "attempt"
assembler_model = "sonnet"
assembler_prompt = "assemble"
moe_experts_by_size = { xl = ["backend"] }
moe_experts = { backend = { prompt = "server side", model = "haiku" } }
moe_assembler_model = "opus"
moe_assembler_prompt = "merge"
[roles.product_manager]
min_issue_size = "m"
`

func TestViewIncludesRoleSpecificKeys(t *testing.T) {
	out := viewJSON(t, showTOML)

	rev := roleOf(t, out, RoleReviewer)
	want := map[string]any{
		"auto_merge":                true,
		"merge_method":              "rebase",
		"checks_wait":               "5s",
		"checks_poll_interval":      "2m0s",
		"checks_timeout":            "30m0s",
		"max_check_fix_rounds":      float64(DefaultMaxCheckFixRounds),
		"pre_review_checks":         true,
		"pre_review_checks_timeout": "10m0s",
	}
	for k, v := range want {
		if got := rev[k]; got != v {
			t.Errorf("reviewer %s: got %#v want %#v", k, got, v)
		}
	}
	dev := roleOf(t, out, RoleDeveloper)
	if got := dev["commit_flags"]; got != "-S" {
		t.Errorf("developer commit_flags: got %#v want %q", got, "-S")
	}
	if got := dev["max_size"]; got != "m" {
		t.Errorf("developer max_size: got %#v want %q", got, "m")
	}
	if got := dev["profiles_by_size"].(map[string]any)["xs"].(map[string]any)["model"]; got != "haiku" {
		t.Errorf("developer model_by_size: got %#v want %v", got, map[string]any{"xs": "haiku"})
	}
	if got := dev["best_of_n_by_size"]; !reflect.DeepEqual(got, map[string]any{"l": float64(3)}) {
		t.Errorf("developer best_of_n_by_size: got %#v want %v", got, map[string]any{"l": float64(3)})
	}
	for k, want := range map[string]string{"best_of_n_model": "opus", "best_of_n_prompt": "attempt", "assembler_model": "sonnet", "assembler_prompt": "assemble", "moe_assembler_model": "opus", "moe_assembler_prompt": "merge"} {
		if got := dev[k]; got != want {
			t.Errorf("developer %s: got %#v want %q", k, got, want)
		}
	}
	if want := (map[string]any{"xl": []any{"backend"}}); !reflect.DeepEqual(dev["moe_experts_by_size"], want) {
		t.Errorf("developer moe_experts_by_size: got %#v want %v", dev["moe_experts_by_size"], want)
	}
	if want := (map[string]any{"backend": map[string]any{"prompt": "server side", "model": "haiku"}}); !reflect.DeepEqual(dev["moe_experts"], want) {
		t.Errorf("developer moe_experts: got %#v want %v", dev["moe_experts"], want)
	}

	pm := roleOf(t, out, RoleProductManager)
	if got := pm["min_issue_size"]; got != "m" {
		t.Errorf("product manager min_issue_size: got %#v want %q", got, "m")
	}

	// Nobody else carries them.
	ownedBy := map[string]string{
		"commit_flags": RoleDeveloper, "max_size": RoleDeveloper,
		"best_of_n_by_size": RoleDeveloper, "best_of_n_model": RoleDeveloper,
		"best_of_n_prompt": RoleDeveloper, "assembler_model": RoleDeveloper,
		"assembler_prompt":    RoleDeveloper,
		"moe_experts_by_size": RoleDeveloper, "moe_experts": RoleDeveloper,
		"moe_assembler_model": RoleDeveloper, "moe_assembler_prompt": RoleDeveloper,
		"min_issue_size": RoleProductManager,
		"auto_merge":     RoleReviewer, "merge_method": RoleReviewer, "checks_wait": RoleReviewer,
		"checks_poll_interval": RoleReviewer, "checks_timeout": RoleReviewer,
		"max_check_fix_rounds": RoleReviewer, "pre_review_checks": RoleReviewer,
		"pre_review_checks_timeout": RoleReviewer,
	}
	for _, r := range Roles {
		for k, owner := range ownedBy {
			if r == owner {
				continue
			}
			if _, ok := roleOf(t, out, r)[k]; ok {
				t.Errorf("role %s must not carry %s", r, k)
			}
		}
	}
}

// TestViewIncludesSkillsRefresh checks the global-only skills_refresh policy is
// printed under every role, both when set and when it falls back to the default.
func TestViewIncludesSkillsRefresh(t *testing.T) {
	out := viewJSON(t, showTOML+"[global]\nskills_refresh = \"always\"\n")
	for _, r := range Roles {
		if got := roleOf(t, out, r)["skills_refresh"]; got != "always" {
			t.Errorf("role %s skills_refresh: got %#v want %q", r, got, "always")
		}
	}
	out = viewJSON(t, showTOML)
	for _, r := range Roles {
		if got := roleOf(t, out, r)["skills_refresh"]; got != DefaultSkillsRefresh {
			t.Errorf("role %s skills_refresh: got %#v want %q", r, got, DefaultSkillsRefresh)
		}
	}
}

func TestViewUsesTOMLKeyNamesAndDurationStrings(t *testing.T) {
	out := viewJSON(t, showTOML)

	if got := roleOf(t, out, RoleQA)["timeout"]; got != "45m0s" {
		t.Errorf("qa timeout: got %#v want %q (a duration string, not nanoseconds)", got, "45m0s")
	}
	sched, _ := out["scheduler"].(map[string]any)
	for _, k := range []string{"poll_interval", "max_developers", "rate_limit_backoff", "triage_batch_size"} {
		if _, ok := sched[k]; !ok {
			t.Errorf("scheduler is missing %s: %v", k, sched)
		}
	}
	for _, k := range []string{"fallback", "max_turns", "allowed_tools", "disallowed_tools"} {
		if _, ok := roleOf(t, out, RoleQA)[k]; !ok {
			t.Errorf("role is missing %s", k)
		}
	}
	if got := out["filter"].(map[string]any)["require_label"]; got != true {
		t.Errorf("filter require_label: got %#v want true", got)
	}
	// No Go field names survive anywhere.
	data, _ := json.Marshal(out)
	for _, bad := range []string{"PollInterval", "MaxDevelopers", "FallbackModel", "MaxTurns", "RequireLabel", "DefaultBranch"} {
		if strings.Contains(string(data), `"`+bad+`"`) {
			t.Errorf("output still uses the Go field name %s", bad)
		}
	}
	// Empty collections are [] / {}, never null.
	dev := roleOf(t, out, RoleDeveloper)
	for _, k := range []string{"skills", "pi_packages", "allowed_tools", "disallowed_tools", "mcp", "env"} {
		if dev[k] == nil {
			t.Errorf("role %s renders as null, want an empty collection", k)
		}
	}
}

// TestViewTagsMirrorTOMLTags checks the structs printed as-is: every toml key
// has a json tag with the same name.
func TestViewTagsMirrorTOMLTags(t *testing.T) {
	for _, typ := range []reflect.Type{reflect.TypeOf(Project{}), reflect.TypeOf(Scheduler{}), reflect.TypeOf(MCPServer{})} {
		for i := range typ.NumField() {
			f := typ.Field(i)
			toml, ok := f.Tag.Lookup("toml")
			if !ok || toml == "-" {
				continue
			}
			if js := f.Tag.Get("json"); js != toml {
				t.Errorf("%s.%s: toml %q but json %q", typ.Name(), f.Name, toml, js)
			}
		}
	}
	// Filter is printed through FilterView, which must cover every key.
	view := reflect.TypeOf(FilterView{})
	for i := range reflect.TypeOf(Filter{}).NumField() {
		key := reflect.TypeOf(Filter{}).Field(i).Tag.Get("toml")
		found := false
		for j := range view.NumField() {
			if view.Field(j).Tag.Get("json") == key {
				found = true
			}
		}
		if !found {
			t.Errorf("FilterView has no field for filter.%s", key)
		}
	}
}

// TestViewCoversTemplateKeys walks every key the bees.toml template can set and
// checks `bees config show` prints it under that name.
func TestViewCoversTemplateKeys(t *testing.T) {
	// github.token references this variable; an unset reference is a load
	// error (see TestTemplateUncommented).
	t.Setenv("BEES_GITHUB_TOKEN", "ghp_example")
	text, err := RenderTOML(RenderOptions{Repo: "acme/widgets", Assignee: "@me", ExplicitRepo: true, ExplicitBranch: true})
	if err != nil {
		t.Fatal(err)
	}
	out := viewJSON(t, uncommentTemplate(text))

	// prompt_file is folded into the resolved prompt; version and the [global]
	// table itself have no key of their own in a role.
	skip := map[string]bool{"prompt_file": true, "version": true}

	section := ""
	for _, line := range strings.Split(uncommentTemplate(text), "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "" || strings.HasPrefix(line, "#"):
			continue
		case strings.HasPrefix(line, "["):
			section = strings.Trim(line, "[]")
			continue
		}
		key, _, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		// Keys inside a [x.env] / [x.mcp.name] / [x.moe_experts.name]
		// sub-table are values of the env / mcp / moe_experts key itself.
		sec := section
		if i := strings.LastIndex(sec, ".env"); i >= 0 && strings.HasSuffix(sec, ".env") {
			sec, key = sec[:i], "env"
		} else if i := strings.Index(sec, ".mcp."); i >= 0 {
			sec, key = sec[:i], "mcp"
		} else if i := strings.Index(sec, ".moe_experts."); i >= 0 {
			sec, key = sec[:i], "moe_experts"
		}
		if skip[key] || key == "profile" || key == "profile_by_size" {
			continue
		}
		switch {
		case strings.HasPrefix(sec, "profiles."):
			if _, ok := roleOf(t, out, RoleDeveloper)[key]; !ok {
				t.Errorf("profile setting %s missing", key)
			}
		case sec == "global":
			for _, r := range Roles {
				if _, ok := roleOf(t, out, r)[key]; !ok {
					t.Errorf("global.%s does not appear under roles.%s", key, r)
				}
			}
		case strings.HasPrefix(sec, "roles."):
			r := strings.TrimPrefix(sec, "roles.")
			if _, ok := roleOf(t, out, r)[key]; !ok {
				t.Errorf("roles.%s.%s does not appear in the output", r, key)
			}
		default:
			tbl, ok := out[sec].(map[string]any)
			if !ok {
				t.Fatalf("no %q table in the output", sec)
			}
			if _, ok := tbl[key]; !ok {
				t.Errorf("%s.%s does not appear in the output", sec, key)
			}
		}
	}
}

// The Dagger keys, which the uncommented template leaves out because they
// need sbx, are shown per role under their own names.
func TestViewShowsTheDaggerKeys(t *testing.T) {
	out := viewJSON(t, "version = 1\n[project]\nrepo = \"a/b\"\n[global]\nsandbox = \"sbx\"\nsandbox_dagger_engine = \"unix:///s\"\nsandbox_dagger_version = \"v0.20.5\"\n")
	dev, _ := out["roles"].(map[string]any)[RoleDeveloper].(map[string]any)
	if dev["sandbox_dagger_engine"] != "unix:///s" || dev["sandbox_dagger_version"] != "v0.20.5" {
		t.Errorf("developer view: engine %v, version %v", dev["sandbox_dagger_engine"], dev["sandbox_dagger_version"])
	}
}

// defaultsViewFixture writes a project bees.toml and a defaults.toml under
// XDG_CONFIG_HOME and returns the printed view and the defaults path the
// output is expected to name.
func defaultsViewFixture(t *testing.T, project, defaults string) (map[string]any, string) {
	t.Helper()
	_, defaultsPath := defaultsFixture(t, project, defaults)
	cfg, err := Load(writeConfig(t, project))
	if err != nil {
		t.Fatal(err)
	}
	v, err := cfg.View(Roles)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
	return out, defaultsPath
}

// TestViewMarksUserDefaultsToml walks every section defaults.toml can
// contribute to and checks the view names the file beside each value it
// supplied, per entry for the map selectors, and nothing beside values the
// project set.
func TestViewMarksUserDefaultsToml(t *testing.T) {
	project := `version = 5
[profiles.project]
agent = "codex"
[profiles.replaced]
model = "project-model"
[global]
profile_by_size = { s = "project" }
[roles.developer]
profile_by_size = { l = "project" }
[roles.reviewer]
angle_profiles = { docs = "project" }
angles = { xs = ["docs"] }
`
	defaults := `version = 5
[profiles.shared]
agent = "claude"
model = "sonnet"
[profiles.other]
agent = "codex"
[profiles.replaced]
model = "user-model"
[global]
profile = "shared"
profile_by_size = { xs = "shared", s = "shared" }
[roles.developer]
profile = "other"
profile_by_size = { m = "shared", l = "shared" }
[roles.reviewer]
brief_profile = "shared"
judge_profile = "shared"
angle_profiles = { general = "shared", docs = "shared" }
angles = { xs = ["quick_general"], m = ["general", "docs"] }
`
	out, defaultsPath := defaultsViewFixture(t, project, defaults)

	got, ok := out["profile_sources"].(map[string]any)
	if !ok {
		t.Fatalf("no profile_sources in %v", out)
	}
	if want := map[string]any{"shared": defaultsPath, "other": defaultsPath}; !reflect.DeepEqual(got, want) {
		t.Errorf("profile_sources = %v, want %v", got, want)
	}

	dev := roleOf(t, out, RoleDeveloper)
	src, ok := dev["profile_sources"].(map[string]any)
	if !ok {
		t.Fatalf("developer carries no profile_sources: %v", dev)
	}
	want := map[string]any{
		"profile":         defaultsPath,
		"profile_by_size": map[string]any{"m": defaultsPath},
	}
	if !reflect.DeepEqual(src, want) {
		t.Errorf("developer profile_sources = %v, want %v", src, want)
	}

	rev := roleOf(t, out, RoleReviewer)
	src, ok = rev["profile_sources"].(map[string]any)
	if !ok {
		t.Fatalf("reviewer carries no profile_sources: %v", rev)
	}
	want = map[string]any{
		"profile":        defaultsPath,
		"brief_profile":  defaultsPath,
		"judge_profile":  defaultsPath,
		"angle_profiles": map[string]any{"general": defaultsPath},
		"angles":         map[string]any{"m": defaultsPath},
	}
	if !reflect.DeepEqual(src, want) {
		t.Errorf("reviewer profile_sources = %v, want %v", src, want)
	}

	// A role defaults.toml reaches only through [global] is marked there.
	qa := roleOf(t, out, RoleQA)
	src, ok = qa["profile_sources"].(map[string]any)
	if !ok {
		t.Fatalf("qa carries no profile_sources: %v", qa)
	}
	if want := (map[string]any{"profile": defaultsPath}); !reflect.DeepEqual(src, want) {
		t.Errorf("qa profile_sources = %v, want %v", src, want)
	}
}

// TestViewWithoutUserDefaultsHasNoSources checks a config loaded without a
// defaults.toml prints no source marking anywhere: built-in defaults and the
// project file keep showing the way they always have.
func TestViewWithoutUserDefaultsHasNoSources(t *testing.T) {
	out, _ := defaultsViewFixture(t, "version = 5\n[project]\nrepo = \"a/b\"\n", "")
	if _, ok := out["profile_sources"]; ok {
		t.Errorf("profile_sources present without a defaults.toml: %v", out["profile_sources"])
	}
	for _, r := range Roles {
		if _, ok := roleOf(t, out, r)["profile_sources"]; ok {
			t.Errorf("role %s carries profile_sources without a defaults.toml", r)
		}
	}
}
