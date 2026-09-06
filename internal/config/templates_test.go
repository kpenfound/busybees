package config

import (
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
)

// templateTable is decision 4 of #421: what each template resolves to. A
// template in Templates() without a row here fails TestTemplatesResolve, and
// so does a row without a template.
var templateTable = map[string]struct {
	productManager, projectManager, developer, reviewer, qa bool
	autoMerge, featureProposals, reviewAssignedPRs          bool
}{
	"contributor":  {false, true, true, true, false, false, true, false},
	"issue-driven": {true, true, true, true, true, false, true, false},
	"slop-factory": {true, true, true, true, true, true, false, false},
	"reviewer":     {false, false, false, true, false, false, true, true},
	"planner":      {true, true, false, false, false, false, true, false},
}

func TestTemplatesOrder(t *testing.T) {
	want := []string{"contributor", "issue-driven", "slop-factory", "reviewer", "planner"}
	if got := TemplateNames(); !slices.Equal(got, want) {
		t.Fatalf("Templates() order: got %v, want %v", got, want)
	}
	names := map[string]bool{}
	for _, tpl := range Templates() {
		if names[tpl.Name] {
			t.Errorf("template %q registered twice", tpl.Name)
		}
		names[tpl.Name] = true
		if tpl.Summary == "" || tpl.When == "" {
			t.Errorf("template %q has no summary or no When paragraph", tpl.Name)
		}
		if strings.Contains(tpl.Summary, "\n") {
			t.Errorf("template %q: the summary is one line for `bees templates list`:\n%s", tpl.Name, tpl.Summary)
		}
	}
	if _, err := TemplateByName("nope"); err == nil || err.Error() != `unknown template "nope" (want one of contributor, issue-driven, slop-factory, reviewer, planner)` {
		t.Errorf("unknown template: got %v", err)
	}
	if tpl, err := TemplateByName("planner"); err != nil || tpl.Name != "planner" {
		t.Errorf("TemplateByName(planner) = %+v, %v", tpl, err)
	}
}

// TestTemplatesResolve loads every template's rendered file and checks the
// resolved config against the table: the effect, not the file text.
func TestTemplatesResolve(t *testing.T) {
	tpls := Templates()
	if len(tpls) != len(templateTable) {
		t.Fatalf("%d templates, %d rows in templateTable: every template needs a row", len(tpls), len(templateTable))
	}
	for _, tpl := range tpls {
		t.Run(tpl.Name, func(t *testing.T) {
			want, ok := templateTable[tpl.Name]
			if !ok {
				t.Fatalf("no row in templateTable for template %q", tpl.Name)
			}
			text, err := RenderTOML(RenderOptions{Repo: "acme/widgets", ExplicitRepo: true, ExplicitBranch: true, Template: &tpl})
			if err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(writeConfig(t, text))
			if err != nil {
				t.Fatalf("template %q does not load: %v\n%s", tpl.Name, err, text)
			}
			enabled := func(role string) bool {
				r, err := cfg.Role(role)
				if err != nil {
					t.Fatal(err)
				}
				return r.Enabled
			}
			got := templateTable[""]
			got.productManager = enabled(RoleProductManager)
			got.projectManager = enabled(RoleProjectManager)
			got.developer = enabled(RoleDeveloper)
			got.reviewer = enabled(RoleReviewer)
			got.qa = enabled(RoleQA)
			got.autoMerge = cfg.Merge().AutoMerge
			got.featureProposals = cfg.Scheduler.Proposals()
			got.reviewAssignedPRs = cfg.Scheduler.ReviewAssignedPRs
			if got != want {
				t.Errorf("template %q resolves to\n%+v\nwant\n%+v", tpl.Name, got, want)
			}
		})
	}
}

// TestTemplateSettingsAreActive checks the file, not the resolved config:
// every template sets exactly the eight keys, each one is an active line in
// the rendered file, and nothing else in the file differs from the default
// render apart from the header.
func TestTemplateSettingsAreActive(t *testing.T) {
	base, err := RenderTOML(RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	baseLines := strings.Split(base, "\n")
	keys := TemplateKeys()
	if len(keys) != 8 {
		t.Fatalf("TemplateKeys() has %d keys, want 8: %v", len(keys), keys)
	}
	for _, tpl := range Templates() {
		t.Run(tpl.Name, func(t *testing.T) {
			got := slices.Sorted(maps.Keys(tpl.Settings))
			if want := slices.Sorted(slices.Values(keys)); !slices.Equal(got, want) {
				t.Fatalf("template %q sets %v, want exactly %v", tpl.Name, got, want)
			}
			text, err := RenderTOML(RenderOptions{Template: &tpl})
			if err != nil {
				t.Fatal(err)
			}
			body, ok := strings.CutPrefix(text, tpl.header())
			if !ok {
				t.Fatalf("template %q: the render does not start with its header:\n%s", tpl.Name, text)
			}
			lines := strings.Split(body, "\n")
			if len(lines) != len(baseLines) {
				t.Fatalf("template %q renders %d lines, the default %d", tpl.Name, len(lines), len(baseLines))
			}
			section := ""
			changed := map[string]bool{}
			for i, line := range lines {
				if strings.HasPrefix(line, "[") {
					section = strings.Trim(line, "[]")
				}
				if line == baseLines[i] {
					continue
				}
				leaf, _, _ := strings.Cut(line, " = ")
				key := section + "." + leaf
				value, sets := tpl.Settings[key]
				if !sets {
					t.Errorf("template %q changed a line it does not set:\n-%s\n+%s", tpl.Name, baseLines[i], line)
					continue
				}
				if want := leaf + " = " + value; line != want {
					t.Errorf("template %q: line %d is %q, want %q", tpl.Name, i+1, line, want)
				}
				if want := "#" + leaf + " = "; !strings.HasPrefix(baseLines[i], want) {
					t.Errorf("template %q: line %d replaced %q, which is not the commented default of %s", tpl.Name, i+1, baseLines[i], key)
				}
				changed[key] = true
			}
			for _, key := range keys {
				if !changed[key] {
					t.Errorf("template %q sets %s, but no line in the render is active for it", tpl.Name, key)
				}
			}
		})
	}
}

// The default render, with no template selected, keeps every one of the eight
// lines commented out: bees.example.toml is the byte-for-byte guard
// (TestExampleTOMLInSync), this one says which line moved.
func TestDefaultRenderLeavesTemplateKeysCommented(t *testing.T) {
	text, err := RenderTOML(RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(text, "# Template:") {
		t.Errorf("the default render carries a template header:\n%s", text[:200])
	}
	for line, n := range map[string]int{
		"\n#enabled = true\n":              5,
		"\n#auto_merge = false\n":          1,
		"\n#feature_proposals = true\n":    1,
		"\n#review_assigned_prs = false\n": 1,
	} {
		if got := strings.Count(text, line); got != n {
			t.Errorf("default render has %d of %q, want %d", got, strings.TrimSpace(line), n)
		}
	}
	for _, active := range []string{"\nenabled = ", "\nauto_merge = ", "\nfeature_proposals = ", "\nreview_assigned_prs = "} {
		if strings.Contains(text, active) {
			t.Errorf("default render has an active %q line", strings.TrimSpace(active))
		}
	}
}

// TestTemplateHeader pins the comment block `bees templates show` and `bees
// init --template` write above the file: the name, the When paragraph as
// comment lines wrapped at the width the rest of the file uses, one blank
// line, then the file.
func TestTemplateHeader(t *testing.T) {
	for _, tpl := range Templates() {
		text, err := RenderTOML(RenderOptions{Template: &tpl})
		if err != nil {
			t.Fatal(err)
		}
		header, rest, ok := strings.Cut(text, "\n\n")
		if !ok || !strings.HasPrefix(rest, "# bees.toml") {
			t.Fatalf("template %q: no blank line between the header and the file:\n%s", tpl.Name, text[:300])
		}
		lines := strings.Split(header, "\n")
		if lines[0] != "# Template: "+tpl.Name || lines[1] != "#" {
			t.Errorf("template %q: header starts %q, %q", tpl.Name, lines[0], lines[1])
		}
		var words []string
		for _, line := range lines[2:] {
			if !strings.HasPrefix(line, "# ") {
				t.Errorf("template %q: header line %q is not a comment", tpl.Name, line)
			}
			if len(line) > commentWidth {
				t.Errorf("template %q: header line is %d wide, the file wraps at %d:\n%s", tpl.Name, len(line), commentWidth, line)
			}
			words = append(words, strings.Fields(line[2:])...)
		}
		if got := strings.Join(words, " "); got != strings.Join(strings.Fields(tpl.When), " ") {
			t.Errorf("template %q: the header does not carry the When paragraph:\n%s", tpl.Name, header)
		}
	}
	// Decision 8 of #421: assignment-triggered review needs filter.assignee,
	// which no template can set, and the reviewer template says how.
	reviewer, err := TemplateByName("reviewer")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"filter.assignee", "bees init --template reviewer --assignee"} {
		if !strings.Contains(reviewer.When, want) {
			t.Errorf("reviewer template's When paragraph does not mention %q:\n%s", want, reviewer.When)
		}
	}
	if _, set := reviewer.Settings["filter.assignee"]; set {
		t.Error("the reviewer template sets filter.assignee")
	}
}

func TestWrapWords(t *testing.T) {
	for _, tc := range []struct {
		text  string
		width int
		want  []string
	}{
		{"", 10, nil},
		{"one two three four", 9, []string{"one two", "three", "four"}},
		{"one two three four", 13, []string{"one two three", "four"}},
		{"  spaced\n  out ", 20, []string{"spaced out"}},
		{"unbreakable-word next", 5, []string{"unbreakable-word", "next"}},
	} {
		if got := wrapWords(tc.text, tc.width); !slices.Equal(got, tc.want) {
			t.Errorf("wrapWords(%q, %d) = %q, want %q", tc.text, tc.width, got, tc.want)
		}
	}
}

// Every template's file loads, and the example file is the default: a
// template must never be what bees.example.toml is generated from.
func TestExampleTOMLHasNoTemplateHeader(t *testing.T) {
	data, err := os.ReadFile(exampleTOML)
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(string(data), "# Template:") {
		t.Error("bees.example.toml was generated with a template selected")
	}
}

// A template that sets only some of the keys (none in the registry does, but
// the func is what `setting` promises) activates those and leaves the others
// as the commented default.
func TestSettingLeavesUnsetKeysCommented(t *testing.T) {
	base, err := RenderTOML(RenderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	partial := Template{Name: "partial", When: "Only QA is off.", Settings: map[string]string{"roles.qa.enabled": "false"}}
	text, err := RenderTOML(RenderOptions{Template: &partial})
	if err != nil {
		t.Fatal(err)
	}
	body := strings.TrimPrefix(text, partial.header())
	if strings.Count(body, "\n#enabled = true\n") != 4 || strings.Count(body, "\nenabled = false\n") != 1 {
		t.Errorf("partial template did not activate exactly roles.qa.enabled:\n%s", body)
	}
	if strings.Count(base, "\n") != strings.Count(body, "\n") || !strings.Contains(body, "\n#auto_merge = false\n") || !strings.Contains(body, "\n#feature_proposals = true\n") {
		t.Errorf("partial template changed a line it does not set")
	}
	empty := Template{Name: "empty", When: "Sets nothing."}
	text, err = RenderTOML(RenderOptions{Template: &empty})
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimPrefix(text, empty.header()) != base {
		t.Errorf("a template with no settings renders something other than the default below its header")
	}
}

// loadTemplate renders a template's bees.toml and loads it, which is the
// config of a project set up with `bees templates show <name> > bees.toml`.
func loadTemplate(t *testing.T, tpl Template) *Config {
	t.Helper()
	text, err := RenderTOML(RenderOptions{Repo: "acme/widgets", ExplicitRepo: true, ExplicitBranch: true, Template: &tpl})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(writeConfig(t, text))
	if err != nil {
		t.Fatalf("template %q does not load: %v", tpl.Name, err)
	}
	return cfg
}

// flip returns tpl with one setting set to the other boolean, which is the
// config of a project that runs the template's shape apart from that setting.
func flip(t *testing.T, tpl Template, key string) Template {
	t.Helper()
	value, ok := tpl.Settings[key]
	if !ok {
		t.Fatalf("template %q does not set %s", tpl.Name, key)
	}
	settings := maps.Clone(tpl.Settings)
	if value == "true" {
		settings[key] = "false"
	} else {
		settings[key] = "true"
	}
	return Template{Name: tpl.Name + "-" + key, When: tpl.When, Settings: settings}
}

// Every key a template may set is read by Compare. A ninth key added to
// TemplateKeys without a resolver would be compared against nothing, and
// every config would silently match on it.
func TestEveryTemplateKeyResolves(t *testing.T) {
	if got, want := slices.Sorted(maps.Keys(resolved)), slices.Sorted(slices.Values(TemplateKeys())); !slices.Equal(got, want) {
		t.Errorf("resolved covers %v, TemplateKeys() is %v", got, want)
	}
}

// A config rendered from a template matches that template. Table-driven over
// Templates(), so a sixth template is covered without a new test.
func TestCompareMatchesTheTemplateItWasRenderedFrom(t *testing.T) {
	for _, tpl := range Templates() {
		t.Run(tpl.Name, func(t *testing.T) {
			if diffs := tpl.Compare(loadTemplate(t, tpl)); len(diffs) != 0 {
				t.Errorf("a config rendered from %q differs from it: %+v", tpl.Name, diffs)
			}
		})
	}
}

// The example the issue leads with: an issue-driven factory compared to
// slop-factory reports that auto-merge is off and that a feature a bee
// proposes waits for a person, and nothing else. The differences come out in
// the order the file writes the keys, which is TemplateKeys order.
func TestCompareReportsTheSettingsAndNothingElse(t *testing.T) {
	issueDriven, err := TemplateByName("issue-driven")
	if err != nil {
		t.Fatal(err)
	}
	slop, err := TemplateByName("slop-factory")
	if err != nil {
		t.Fatal(err)
	}
	want := []Difference{
		{Key: "scheduler.feature_proposals", Config: "true", Template: "false"},
		{Key: "roles.reviewer.auto_merge", Config: "false", Template: "true"},
	}
	if got := slop.Compare(loadTemplate(t, issueDriven)); !slices.Equal(got, want) {
		t.Errorf("issue-driven vs slop-factory:\ngot  %+v\nwant %+v", got, want)
	}
}

// One setting at a time: a config that runs a template's shape apart from one
// key is reported as differing on that key and nothing else, for all eight
// keys of every template.
func TestCompareReportsOneSettingAtATime(t *testing.T) {
	for _, tpl := range Templates() {
		for _, key := range TemplateKeys() {
			t.Run(tpl.Name+"/"+key, func(t *testing.T) {
				changed := flip(t, tpl, key)
				cfg := loadTemplate(t, changed)
				want := []Difference{{Key: key, Config: changed.Settings[key], Template: tpl.Settings[key]}}
				if got := tpl.Compare(cfg); !slices.Equal(got, want) {
					t.Errorf("got %+v, want %+v", got, want)
				}
			})
		}
	}
}

// Decision 2 of #423: the comparison is on the resolved values. A config that
// sets none of the eight keys runs on their defaults, which is what
// issue-driven writes explicitly, so the two are equal.
func TestCompareReadsDefaultsNotFileText(t *testing.T) {
	text := "version = 1\n\n[project]\nrepo = \"acme/widgets\"\ndefault_branch = \"main\"\n"
	cfg, err := Load(writeConfig(t, text))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(text, "enabled") || strings.Contains(text, "auto_merge") {
		t.Fatalf("the fixture sets a templated key, so it proves nothing:\n%s", text)
	}
	tpl, err := TemplateByName("issue-driven")
	if err != nil {
		t.Fatal(err)
	}
	if tpl.Settings["roles.qa.enabled"] != "true" {
		t.Fatalf("issue-driven no longer sets roles.qa.enabled = true, so this fixture proves nothing")
	}
	if diffs := tpl.Compare(cfg); len(diffs) != 0 {
		t.Errorf("a config that sets nothing differs from issue-driven: %+v", diffs)
	}
}

// Closest picks the template a config differs from least, and reports those
// differences.
func TestClosest(t *testing.T) {
	for _, tpl := range Templates() {
		cfg := loadTemplate(t, tpl)
		got, diffs := Closest(cfg)
		if got.Name != tpl.Name || len(diffs) != 0 {
			t.Errorf("a config rendered from %q is closest to %q with %d differences", tpl.Name, got.Name, len(diffs))
		}
	}
	// One setting away from slop-factory and further from everything else.
	tpl, err := TemplateByName("slop-factory")
	if err != nil {
		t.Fatal(err)
	}
	cfg := loadTemplate(t, flip(t, tpl, "roles.qa.enabled"))
	got, diffs := Closest(cfg)
	want := []Difference{{Key: "roles.qa.enabled", Config: "false", Template: "true"}}
	if got.Name != "slop-factory" || !slices.Equal(diffs, want) {
		t.Errorf("Closest = %q, %+v; want slop-factory, %+v", got.Name, diffs, want)
	}
}

// Decision 4 of #423: ties go to the earlier template in Templates(). This
// config enables the product manager (contributor does not) and disables QA
// (issue-driven does not), and agrees with both on the other six settings.
func TestClosestTieGoesToTheEarlierTemplate(t *testing.T) {
	contributor, err := TemplateByName("contributor")
	if err != nil {
		t.Fatal(err)
	}
	cfg := loadTemplate(t, flip(t, contributor, "roles.product_manager.enabled"))
	issueDriven, err := TemplateByName("issue-driven")
	if err != nil {
		t.Fatal(err)
	}
	if a, b := len(contributor.Compare(cfg)), len(issueDriven.Compare(cfg)); a != 1 || b != 1 {
		t.Fatalf("the fixture is %d from contributor and %d from issue-driven, not a tie", a, b)
	}
	if got, diffs := Closest(cfg); got.Name != "contributor" {
		t.Errorf("Closest = %q, %+v; want contributor, the earlier of the two in Templates()", got.Name, diffs)
	}
}
