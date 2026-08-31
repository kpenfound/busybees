package config

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// viewJSON loads a config, renders it through View and decodes the result the
// way `bees config show` prints it.
func viewJSON(t *testing.T, body string) map[string]any {
	t.Helper()
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
	if got := dev["model_by_size"]; !reflect.DeepEqual(got, map[string]any{"xs": "haiku"}) {
		t.Errorf("developer model_by_size: got %#v want %v", got, map[string]any{"xs": "haiku"})
	}

	// Nobody else carries them.
	devOnly := map[string]bool{"commit_flags": true, "max_size": true, "model_by_size": true}
	own := []string{"commit_flags", "max_size", "model_by_size", "auto_merge", "merge_method", "checks_wait",
		"checks_poll_interval", "checks_timeout", "max_check_fix_rounds", "pre_review_checks",
		"pre_review_checks_timeout"}
	for _, r := range Roles {
		for _, k := range own {
			if r == RoleReviewer && !devOnly[k] {
				continue
			}
			if r == RoleDeveloper && devOnly[k] {
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
	for _, k := range []string{"fallback_model", "max_turns", "allowed_tools", "disallowed_tools"} {
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
	for _, k := range []string{"skills", "allowed_tools", "disallowed_tools", "mcp", "env"} {
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
	text, err := Template(TemplateData{Repo: "acme/widgets", Assignee: "@me", ExplicitRepo: true, ExplicitBranch: true})
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
		// Keys inside a [x.env] / [x.mcp.name] sub-table are values of the
		// env / mcp key itself.
		sec := section
		if i := strings.LastIndex(sec, ".env"); i >= 0 && strings.HasSuffix(sec, ".env") {
			sec, key = sec[:i], "env"
		} else if i := strings.Index(sec, ".mcp."); i >= 0 {
			sec, key = sec[:i], "mcp"
		}
		if skip[key] {
			continue
		}
		switch {
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
