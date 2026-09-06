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
