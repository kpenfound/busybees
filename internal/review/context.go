package review

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/kpenfound/busybees/internal/github"
)

// Item is one piece of context a source gathered.
type Item struct {
	// Source is the context source that produced the item: a name from
	// BuiltinSources, or a project's own name for the files it gathers.
	Source string `json:"source"`
	// Name says which piece it is, in whatever way the source names its
	// own: a path, a pull request reference, a symbol.
	Name string `json:"name"`
	// Content is the text, as it was read.
	Content string `json:"content"`
}

// Bundle is the context a review gathered about one pull request, and the
// starting context of the distiller session that reads it (distill.go). It
// is raw: an item holds the text its source produced, neither summarised nor
// truncated, in the order Project.EnabledSources gathers them.
type Bundle struct {
	// Ref is the pull request the context is about, and PR what gh returned
	// for it.
	Ref Ref       `json:"ref"`
	PR  github.PR `json:"pr"`
	// Items are the pieces gathered, source by source.
	Items []Item `json:"items"`
	// Skipped lists the context a source asked for and did not get: an
	// issue reference that is not an issue, a style source matching no
	// file, a machine with no checkout of the repository. It is not a
	// failure — a review runs on the context there is — and it travels in
	// the bundle so the distiller can say what it did not see.
	Skipped []string `json:"skipped,omitempty"`
}

// Of returns the items one source contributed, in bundle order.
func (b *Bundle) Of(source string) []Item {
	var out []Item
	for _, it := range b.Items {
		if it.Source == source {
			out = append(out, it)
		}
	}
	return out
}

// Sources lists the sources that contributed an item, in bundle order and
// without repeats.
func (b *Bundle) Sources() []string {
	var out []string
	for _, it := range b.Items {
		if len(out) == 0 || out[len(out)-1] != it.Source {
			out = append(out, it.Source)
		}
	}
	return out
}

// Text renders the bundle as the markdown a session reads: one section per
// item, its content in a fence long enough to hold it, and what was skipped
// at the end.
func (b *Bundle) Text() string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Context for %s\n\n%s\n", b.Ref, b.Ref.URL())
	for _, it := range b.Items {
		fmt.Fprintf(&out, "\n## %s: %s\n\n%s\n", it.Source, it.Name, fenced(it.Content))
	}
	if len(b.Skipped) > 0 {
		out.WriteString("\n## Not gathered\n\n")
		for _, s := range b.Skipped {
			fmt.Fprintf(&out, "- %s\n", s)
		}
	}
	return out.String()
}

// backticks matches a run of three or more backticks, the fences a piece of
// context can already contain.
var backticks = regexp.MustCompile("`{3,}")

// fenced puts content in a code fence longer than any fence inside it, so a
// diff of a markdown file cannot end the block early.
func fenced(content string) string {
	fence := "```"
	for _, run := range backticks.FindAllString(content, -1) {
		if len(run) >= len(fence) {
			fence = strings.Repeat("`", len(run)+1)
		}
	}
	return fence + "\n" + strings.TrimRight(content, "\n") + "\n" + fence
}

// Collector gathers one context source. The built-ins (Builtins) are bound
// to the names in BuiltinSources; a source a project declares with files of
// its own needs none, it gathers those files.
type Collector interface {
	// Name is the name in context.toml this collector answers to.
	Name() string
	// Collect gathers the source, and records what it could not read with
	// Input.Skip instead of failing: an error is for a source that could
	// not run at all.
	Collect(ctx context.Context, in *Input) ([]Item, error)
}

// Input is what a Collector gathers from: the pull request, the client that
// reads GitHub, the project configuration, and the checkout the sources that
// read files read from.
type Input struct {
	// Ref is the pull request being gathered and PR what gh returned for it.
	Ref Ref
	PR  github.PR
	// Client reads GitHub, authenticated as the person running the review.
	Client *github.Client
	// Dir is the checkout of the repository under review, and "" on a
	// machine that has none: the sources that read files then gather
	// nothing. It need not be the top of that checkout, only somewhere
	// inside it. A source reading the files context.toml names resolves
	// them against Root; the callers source searches the whole repository,
	// starting from here.
	Dir string
	// Project is the repository's context.toml, never nil.
	Project *Project

	diff    string
	gotDiff bool
	diffErr error
	skipped []string
}

// Root is the directory the files named in context.toml are read from: the
// one context.toml lives in, which its paths are relative to, and the
// checkout when the project has no context.toml. It is "" when there is no
// checkout, which is what those sources check. The callers source does not
// use it: a repository-wide search is not relative to anything.
func (in *Input) Root() string {
	if in.Project != nil && in.Project.Loaded {
		return in.Project.Dir()
	}
	return in.Dir
}

// Diff is the pull request's diff, read once however many sources ask for
// it: the diff source is one, and the callers source reads the changed
// symbols out of it.
func (in *Input) Diff(ctx context.Context) (string, error) {
	if !in.gotDiff {
		in.diff, in.diffErr = in.Client.PRDiff(ctx, in.Ref.Number)
		in.gotDiff = true
	}
	return in.diff, in.diffErr
}

// Skip records context this source asked for and did not get. It reaches the
// person as Bundle.Skipped, and the review goes on without it.
func (in *Input) Skip(format string, args ...any) {
	in.skipped = append(in.skipped, fmt.Sprintf(format, args...))
}

// Builtins are the collectors bees implements itself, one for every name in
// BuiltinSources.
func Builtins() []Collector {
	return []Collector{diffSource{}, prBodySource{}, linkedIssuesSource{}, styleFilesSource{}, callersSource{}}
}

// Pipeline gathers the context sources a project declares, for any pull
// request on GitHub: it reads the repository through gh and the checkout it
// is given, and nothing a factory keeps.
type Pipeline struct {
	// Client reads GitHub. Its repository is the one under review.
	Client *github.Client
	// Project is the repository's context.toml, which says which sources
	// are gathered. Nil is a repository that has none: every built-in.
	Project *Project
	// Dir is the checkout of the repository under review, anywhere inside
	// it: the sources that read files read from there. "" is a machine with
	// no checkout, and CheckoutOf is how a caller gets one or the other.
	Dir string
	// Collectors are the collectors the names in context.toml bind to,
	// Builtins when it is nil. Replacing one changes what that built-in
	// source reads; a source the project declares with files of its own is
	// gathered without one.
	Collectors []Collector
}

// Gather reads the pull request and gathers every context source the project
// enables, in Project.EnabledSources order.
func (p *Pipeline) Gather(ctx context.Context, number int) (*Bundle, error) {
	project := p.Project
	if project == nil {
		project = &Project{}
	}
	ref := Ref{Repo: p.Client.Repo, Number: number}
	pr, err := p.Client.GetPR(ctx, number)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ref, err)
	}
	in := &Input{Ref: ref, PR: pr, Client: p.Client, Dir: p.Dir, Project: project}
	collectors := p.Collectors
	if collectors == nil {
		collectors = Builtins()
	}
	byName := make(map[string]Collector, len(collectors))
	for _, c := range collectors {
		byName[c.Name()] = c
	}
	bundle := &Bundle{Ref: ref, PR: pr}
	for _, s := range project.EnabledSources() {
		collector, ok := byName[s.Name]
		switch {
		case ok:
		case s.Builtin():
			return nil, fmt.Errorf("context source %q: no collector", s.Name)
		default:
			collector = fileSource{name: s.Name, files: s.Files}
		}
		items, err := collector.Collect(ctx, in)
		if err != nil {
			return nil, fmt.Errorf("context source %q: %w", s.Name, err)
		}
		bundle.Items = append(bundle.Items, items...)
	}
	bundle.Skipped = in.skipped
	return bundle, nil
}

// NewClient is the gh client a review reads ref's repository through, and
// posts to: authenticated the way cfg says, which with no token is the
// machine's own gh authentication.
func NewClient(ref Ref, cfg *Config) *github.Client {
	client := github.New(ref.Repo)
	if cfg != nil {
		client.Token = cfg.GitHub.ResolvedToken()
	}
	return client
}

// Open is the pipeline that gathers context for ref: gh authenticated the
// way cfg says, the context.toml found from dir, and dir itself as the
// checkout — but only when dir is a checkout of ref's own repository, so a
// review run from an unrelated clone reads no style rules rather than the
// wrong ones.
func Open(ctx context.Context, ref Ref, cfg *Config, dir string) (*Pipeline, error) {
	client := NewClient(ref, cfg)
	// With no checkout there is no context.toml to look for either: the
	// directory the command happens to have been run in is another
	// repository, and its configuration is not this review's.
	dir = CheckoutOf(ctx, ref.Repo, dir)
	project := &Project{}
	if dir != "" {
		found, err := LoadProject(FindProject(dir))
		if err != nil {
			return nil, err
		}
		project = found
	}
	return &Pipeline{Client: client, Project: project, Dir: dir}, nil
}
