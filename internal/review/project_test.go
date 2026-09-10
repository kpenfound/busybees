package review

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestProjectDefaults(t *testing.T) {
	dir := t.TempDir()
	p, err := LoadProject(filepath.Join(dir, ProjectFile))
	if err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}
	if p.Loaded {
		t.Fatal("Loaded is true for a file that is not there")
	}
	if got := p.EnabledAngles(); !reflect.DeepEqual(got, BuiltinAngles) {
		t.Fatalf("angles %v, want every built-in %v", got, BuiltinAngles)
	}
	if got := sourceNames(p.EnabledSources()); !reflect.DeepEqual(got, BuiltinSources) {
		t.Fatalf("sources %v, want every built-in %v", got, BuiltinSources)
	}
	if p.Severity("nitpick") != "" {
		t.Fatalf("severity %q, want no override", p.Severity("nitpick"))
	}
	if got, want := p.Dir(), dir; got != want {
		t.Fatalf("dir %q, want %q", got, want)
	}
	// A repository with no context.toml at all is the same configuration.
	none, err := LoadProject(FindProject(dir))
	if err != nil {
		t.Fatal(err)
	}
	if none.Loaded || len(none.EnabledAngles()) != len(BuiltinAngles) {
		t.Fatalf("no context.toml: %+v", none)
	}
}

func TestFindProject(t *testing.T) {
	root := t.TempDir()
	path := writeFile(t, root, ProjectFile, "")
	sub := filepath.Join(root, "internal", "pkg")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindProject(sub); got != path {
		t.Fatalf("FindProject from a subdirectory: %q, want %q", got, path)
	}
	// A directory that happens to be called context.toml is not one.
	if err := os.Mkdir(filepath.Join(root, "internal", ProjectFile), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := FindProject(sub); got != path {
		t.Fatalf("FindProject past a context.toml directory: %q, want %q", got, path)
	}
	if got := FindProject(t.TempDir()); got != "" {
		t.Fatalf("FindProject with no file: %q, want an empty path", got)
	}
}

func TestProjectOverrides(t *testing.T) {
	dir := t.TempDir()
	p, err := LoadProject(writeFile(t, dir, ProjectFile, `
style_sources = ["docs/style.md", "CONTRIBUTING.md"]

[angles]
style = false
side_effects = true

[categories]
nitpick = "off"
naming = "low"

[[context_sources]]
name = "callers"
enabled = false

[[context_sources]]
name = "adr"
files = ["docs/adr/*.md"]
`))
	if err != nil {
		t.Fatal(err)
	}
	if !p.Loaded {
		t.Fatal("Loaded is false for a file that is there")
	}
	want := []string{AngleAcceptance, AngleTests, AngleSideEffects}
	if got := p.EnabledAngles(); !reflect.DeepEqual(got, want) {
		t.Fatalf("angles %v, want %v", got, want)
	}
	wantSources := []string{SourceDiff, SourcePRBody, SourceLinkedIssues, SourceStyleFiles, "adr"}
	if got := sourceNames(p.EnabledSources()); !reflect.DeepEqual(got, wantSources) {
		t.Fatalf("sources %v, want %v", got, wantSources)
	}
	adr := p.EnabledSources()[4]
	if adr.Builtin() || !reflect.DeepEqual(adr.Files, []string{"docs/adr/*.md"}) {
		t.Fatalf("the project's own source: %+v", adr)
	}
	if p.Severity("nitpick") != SeverityOff || p.Severity("naming") != SeverityLow || p.Severity("style") != "" {
		t.Fatalf("categories: %v", p.Categories)
	}
	if !reflect.DeepEqual(p.StyleSources, []string{"docs/style.md", "CONTRIBUTING.md"}) {
		t.Fatalf("style sources: %v", p.StyleSources)
	}
}

// A built-in named with enabled = true, or not named at all, is gathered:
// the entry configures the source, it does not replace it.
func TestProjectBuiltinSourceStaysInPlace(t *testing.T) {
	p, err := ParseProject("[[context_sources]]\nname = \"callers\"\nenabled = true\n", filepath.Join(t.TempDir(), ProjectFile))
	if err != nil {
		t.Fatal(err)
	}
	if got := sourceNames(p.EnabledSources()); !reflect.DeepEqual(got, BuiltinSources) {
		t.Fatalf("sources %v, want every built-in in order %v", got, BuiltinSources)
	}
}

func TestProjectInvalid(t *testing.T) {
	for _, tc := range []struct {
		name string
		toml string
		want []string
	}{
		{"unknown key", "angels = []\n", []string{"unknown keys", "angels"}},
		{"unknown key in an entry", "[[context_sources]]\nname = \"diff\"\nenable = false\n", []string{"unknown keys", "context_sources.enable"}},
		{"wrong type", "[angles]\nstyle = \"off\"\n", []string{"style"}},
		{"unknown angle", "[angles]\nsecurity = false\n", []string{"angles.security: unknown angle (want one of acceptance_criteria, test_coverage, style, side_effects)"}},
		{"category severity", "[categories]\nnaming = \"nit\"\n", []string{"categories.naming \"nit\" must be one of off, info, low, medium, high"}},
		{"absolute style source", "style_sources = [\"/etc/style.md\"]\n", []string{"style_sources[0] \"/etc/style.md\" must be relative to the project directory"}},
		{"empty style source", "style_sources = [\"docs/style.md\", \"  \"]\n", []string{"style_sources[1] is empty"}},
		{"nameless source", "[[context_sources]]\nenabled = false\n", []string{"context_sources[0].name is required"}},
		{"unknown source", "[[context_sources]]\nname = \"dif\"\n", []string{"context_sources[0]: \"dif\" is not a built-in source (diff, pr_body, linked_issues, style_files, callers): give it files to gather, or fix the name"}},
		{"built-in with files", "[[context_sources]]\nname = \"diff\"\nfiles = [\"x.md\"]\n", []string{"\"diff\" is a built-in source and gathers no files"}},
		{"source twice", "[[context_sources]]\nname = \"diff\"\n\n[[context_sources]]\nname = \"diff\"\nenabled = false\n", []string{"context source \"diff\" is configured twice"}},
		{"absolute source file", "[[context_sources]]\nname = \"adr\"\nfiles = [\"/etc/adr.md\"]\n", []string{"context_sources[0].files[0] \"/etc/adr.md\" must be relative"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ProjectFile)
			_, err := LoadProject(writeFile(t, dir, ProjectFile, tc.toml))
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

// Every problem is reported, and in the same order every time: a map is
// walked by sorted key, not by whatever order the runtime hands back.
func TestProjectReportsEveryProblem(t *testing.T) {
	toml := "[angles]\nsecurity = false\nperformance = false\n\n[categories]\nnaming = \"nit\"\n"
	first, err := ParseProject(toml, filepath.Join(t.TempDir(), ProjectFile))
	if err == nil {
		t.Fatalf("loaded without an error: %+v", first)
	}
	for _, want := range []string{"angles.performance", "angles.security", "categories.naming"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	if a, b := strings.Index(err.Error(), "angles.performance"), strings.Index(err.Error(), "angles.security"); a > b {
		t.Fatalf("problems are not in a stable order: %q", err)
	}
	for range 20 {
		_, again := ParseProject(toml, filepath.Join(t.TempDir(), ProjectFile))
		if again == nil {
			t.Fatal("loaded without an error")
		}
		if a, b := strings.Split(again.Error(), "\n  - "), strings.Split(err.Error(), "\n  - "); !reflect.DeepEqual(a[1:], b[1:]) {
			t.Fatalf("problems come back in a different order: %q, first time %q", again, err)
		}
	}
}

func sourceNames(sources []Source) []string {
	names := make([]string, 0, len(sources))
	for _, s := range sources {
		names = append(names, s.Name)
	}
	return names
}
