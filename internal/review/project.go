package review

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// The angles a review runs, each one a session of its own reading the
// review brief. A project turns one off, or back on, under [angles].
const (
	// AngleAcceptance checks the change against the acceptance criteria of
	// what it says it does.
	AngleAcceptance = "acceptance_criteria"
	// AngleTests checks the tests and the documentation the change owes.
	AngleTests = "test_coverage"
	// AngleStyle checks the change against the project's own style sources.
	AngleStyle = "style"
	// AngleSideEffects checks what the change breaks elsewhere.
	AngleSideEffects = "side_effects"
)

// BuiltinAngles lists the angles every review runs, in the order they are
// fanned out.
var BuiltinAngles = []string{AngleAcceptance, AngleTests, AngleStyle, AngleSideEffects}

// The context sources a review gathers for the distiller. A project turns
// one off, or adds one of its own, under [[context_sources]].
const (
	// SourceDiff is the pull request's diff.
	SourceDiff = "diff"
	// SourcePRBody is the pull request's title, body and conversation.
	SourcePRBody = "pr_body"
	// SourceLinkedIssues is the issues the pull request closes or mentions.
	SourceLinkedIssues = "linked_issues"
	// SourceStyleFiles is the project's style sources, the built-in ones
	// plus Project.StyleSources.
	SourceStyleFiles = "style_files"
	// SourceCallers is the callers of the symbols the diff changed.
	SourceCallers = "callers"
)

// BuiltinSources lists the context sources every review gathers, in the
// order they enter the context bundle.
var BuiltinSources = []string{SourceDiff, SourcePRBody, SourceLinkedIssues, SourceStyleFiles, SourceCallers}

// Severities a category can be pinned to, most severe last. SeverityOff
// drops the category's findings instead of ranking them.
const (
	SeverityOff    = "off"
	SeverityInfo   = "info"
	SeverityLow    = "low"
	SeverityMedium = "medium"
	SeverityHigh   = "high"
)

// Severities lists the accepted severity values, in the order they are
// printed.
var Severities = []string{SeverityOff, SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh}

// Project is the per-project configuration, context.toml, read from the
// repository under review. Every table is an override: a project that has no
// context.toml gets every angle, every built-in context source and no
// category rules.
type Project struct {
	// Path is the file this configuration was read from, absolute, and set
	// even when that file does not exist (Loaded says which). Style sources
	// and source files are relative to its directory.
	Path string `toml:"-"`
	// Loaded reports whether Path existed.
	Loaded bool `toml:"-"`

	// Angles turns an angle off (`style = false`) or back on. A key must
	// name one of BuiltinAngles; an angle the file does not mention runs.
	Angles map[string]bool `toml:"angles"`
	// StyleSources are the project's own style documents, as paths or globs
	// relative to Path's directory, gathered on top of the ones the style
	// source finds by itself.
	StyleSources []string `toml:"style_sources"`
	// Categories pins the severity of a finding category, one of
	// Severities, or drops the category with "off". Category names are the
	// angles' own, so any name is accepted; read a category's override
	// through Severity.
	Categories map[string]string `toml:"categories"`
	// Sources are the [[context_sources]] entries, each either a built-in
	// (a name from BuiltinSources, turned off with `enabled = false`) or a
	// source of the project's own, which gathers the files it names.
	Sources []Source `toml:"context_sources"`
}

// Source is one [[context_sources]] entry.
type Source struct {
	// Name is a name from BuiltinSources, or the project's own name for a
	// source of files.
	Name string `toml:"name"`
	// Enabled turns the source off without deleting the entry. It defaults
	// to true, so read it through On.
	Enabled *bool `toml:"enabled"`
	// Files are paths or globs, relative to the project directory, that a
	// source of the project's own gathers. A built-in takes none: it knows
	// what it reads.
	Files []string `toml:"files"`
}

// On reports whether the source is gathered.
func (s Source) On() bool { return s.Enabled == nil || *s.Enabled }

// Builtin reports whether the source is one bees implements itself.
func (s Source) Builtin() bool { return slices.Contains(BuiltinSources, s.Name) }

// FindProject looks for context.toml in dir and its parents, and returns ""
// when there is none.
func FindProject(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return ""
	}
	for {
		p := filepath.Join(abs, ProjectFile)
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return ""
		}
		abs = parent
	}
}

// LoadProject reads the project configuration at path. A file that is not
// there is not an error: it loads as the defaults, with Loaded false, which
// is what a repository with no context.toml gets. An empty path is that same
// repository, seen through FindProject.
func LoadProject(path string) (*Project, error) {
	if path == "" {
		return &Project{}, nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		return &Project{Path: abs}, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseProject(string(data), abs)
}

// ParseProject validates the text of a project configuration file as if it
// had been read from path, which is used for the Project's location and in
// error messages but is not itself read.
func ParseProject(text, path string) (*Project, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	p := Project{Path: abs, Loaded: true}
	md, err := toml.Decode(text, &p)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}
	if err := undecoded(md, abs); err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate checks the project configuration, reporting every problem it
// finds rather than the first.
func (p *Project) Validate() error {
	var errs []string
	for _, name := range sortedNames(p.Angles) {
		if !slices.Contains(BuiltinAngles, name) {
			errs = append(errs, fmt.Sprintf("angles.%s: unknown angle (want one of %s)", name, strings.Join(BuiltinAngles, ", ")))
		}
	}
	errs = append(errs, patternErrs("style_sources", p.StyleSources)...)
	for _, name := range sortedNames(p.Categories) {
		if !slices.Contains(Severities, p.Categories[name]) {
			errs = append(errs, fmt.Sprintf("categories.%s %q must be one of %s", name, p.Categories[name], strings.Join(Severities, ", ")))
		}
	}
	seen := map[string]bool{}
	for i, s := range p.Sources {
		at := fmt.Sprintf("context_sources[%d]", i)
		switch {
		case s.Name == "":
			errs = append(errs, at+".name is required: a built-in source ("+strings.Join(BuiltinSources, ", ")+"), or a name of your own for the files it gathers")
			continue
		case seen[s.Name]:
			errs = append(errs, fmt.Sprintf("%s: context source %q is configured twice", at, s.Name))
			continue
		}
		seen[s.Name] = true
		switch {
		case s.Builtin() && len(s.Files) > 0:
			errs = append(errs, fmt.Sprintf("%s: %q is a built-in source and gathers no files: drop files, or give the entry a name of its own", at, s.Name))
		case !s.Builtin() && len(s.Files) == 0:
			errs = append(errs, fmt.Sprintf("%s: %q is not a built-in source (%s): give it files to gather, or fix the name", at, s.Name, strings.Join(BuiltinSources, ", ")))
		}
		errs = append(errs, patternErrs(at+".files", s.Files)...)
	}
	return invalid(p.Path, errs)
}

// patternErrs checks a list of paths or globs. They are read out of the
// repository under review, which the reviewing machine has as a checkout and
// nothing more, so each one is relative to the project directory.
func patternErrs(key string, patterns []string) []string {
	var errs []string
	for i, pattern := range patterns {
		switch {
		case strings.TrimSpace(pattern) == "":
			errs = append(errs, fmt.Sprintf("%s[%d] is empty: give it a path or a glob, or drop the entry", key, i))
		case filepath.IsAbs(pattern):
			errs = append(errs, fmt.Sprintf("%s[%d] %q must be relative to the project directory", key, i, pattern))
		case !filepath.IsLocal(pattern):
			errs = append(errs, fmt.Sprintf("%s[%d] %q must stay inside the project directory", key, i, pattern))
		}
	}
	return errs
}

// Dir is the directory the project configuration lives in, which style
// sources and source files are relative to.
func (p *Project) Dir() string {
	if p.Path == "" {
		return "."
	}
	return filepath.Dir(p.Path)
}

// EnabledAngles lists the angles this project runs, in BuiltinAngles order.
func (p *Project) EnabledAngles() []string {
	angles := make([]string, 0, len(BuiltinAngles))
	for _, a := range BuiltinAngles {
		if on, ok := p.Angles[a]; ok && !on {
			continue
		}
		angles = append(angles, a)
	}
	return angles
}

// EnabledSources lists the context sources this project gathers: the
// built-ins it did not turn off, in BuiltinSources order, then its own, in
// the order the file declares them.
func (p *Project) EnabledSources() []Source {
	configured := map[string]Source{}
	for _, s := range p.Sources {
		configured[s.Name] = s
	}
	sources := make([]Source, 0, len(BuiltinSources)+len(p.Sources))
	for _, name := range BuiltinSources {
		s, ok := configured[name]
		if !ok {
			s = Source{Name: name}
		}
		if s.On() {
			sources = append(sources, s)
		}
	}
	for _, s := range p.Sources {
		if !s.Builtin() && s.On() {
			sources = append(sources, s)
		}
	}
	return sources
}

// Severity is the severity this project pins findings in category to, and ""
// when it pins none. A category pinned to SeverityOff is dropped.
func (p *Project) Severity(category string) string { return p.Categories[category] }

// sortedNames orders a map's keys, so a file with several bad ones reports
// them in the same order every time.
func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
