package review

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	core "github.com/kpenfound/busybees/core/review"
	"github.com/kpenfound/busybees/internal/github"
)

type Item = core.Item

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

// core preserves source ordering, labels, complete content and skipped reasons.
func (b *Bundle) core() *core.Bundle[Ref] {
	if b == nil {
		return nil
	}
	return &core.Bundle[Ref]{Ref: b.Ref, Title: b.PR.Title, Author: b.PR.Author.Login, Items: b.Items, Skipped: b.Skipped}
}
func (b *Bundle) Of(source string) []Item { return b.core().Of(source) }
func (b *Bundle) Sources() []string       { return b.core().Sources() }
func (b *Bundle) Text() string            { return b.core().Text() }

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
	// Checkout is the checkout of the pull request's head made for this
	// review under its artifact directory (checkout.go), and "" when none
	// was made: Diff reads the diff from it when there is one, and through
	// gh otherwise.
	Checkout string
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
// symbols out of it. It is read from Checkout when there is one
// (checkoutDiff), which no number of changed files is too many for, and
// through gh otherwise, or when the checkout's read failed: `gh pr diff`,
// which GitHub refuses for a pull request past its limit on changed
// files. An error is a diff that could be read neither way, and says
// what each way said.
func (in *Input) Diff(ctx context.Context) (string, error) {
	if in.gotDiff {
		return in.diff, in.diffErr
	}
	in.gotDiff = true
	var fromCheckout error
	if in.Checkout != "" {
		diff, note, err := checkoutDiff(ctx, in.Checkout)
		if err == nil {
			if note != "" {
				in.Skip("%s: %s", SourceDiff, note)
			}
			in.diff = diff
			return in.diff, nil
		}
		fromCheckout = err
	}
	diff, err := in.Client.PRDiff(ctx, in.Ref.Number)
	switch {
	case err == nil:
		in.diff = diff
	case fromCheckout != nil:
		in.diffErr = fmt.Errorf("the diff of %s could not be read from the checkout (%v) or through gh (%w)", in.Ref, fromCheckout, err)
	default:
		in.diffErr = fmt.Errorf("the diff of %s could not be read through gh: %w", in.Ref, err)
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
	// Checkout clones the pull request's head, with its merge base with
	// the base branch beside it, into the artifact directory Gather is given (checkout.go):
	// the diff source reads the diff from that clone, and the angles run in
	// it afterwards (Angles.Run). It is nil to attempt none, and the diff
	// is then read through gh.
	Checkout *Checkout
	// Log is where a checkout that was made, or could not be, is reported,
	// and nowhere when nil.
	Log io.Writer
}

// Gather reads the pull request and gathers every context source the project
// enables, in Project.EnabledSources order. artifact is the review's
// artifact directory, which Checkout clones the pull request's head under
// as CheckoutDir before the sources run, for the diff source to read and
// the angles to run in; with no Checkout, or "" for artifact, no clone is
// made and the diff is read through gh. A clone that could not be made is
// reported on Log and is not an error: the diff is read through gh, and
// the review goes on with what it gathered.
func (p *Pipeline) Gather(ctx context.Context, number int, artifact string) (*Bundle, error) {
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
	in.Checkout = p.checkout(ctx, ref, pr.BaseRefName, artifact)
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

// checkout is the clone of ref's head Checkout makes under artifact, and
// "" when there is no Checkout, no artifact, or the clone could not be
// made, which is reported on Log with what the diff is read through
// instead. A clone that is there already, from an earlier attempt for the
// same review, is used as it is.
func (p *Pipeline) checkout(ctx context.Context, ref Ref, base, artifact string) string {
	if p.Checkout == nil || artifact == "" {
		return ""
	}
	dir := filepath.Join(artifact, CheckoutDir)
	if st, err := os.Stat(dir); err == nil && st.IsDir() {
		return dir
	}
	if err := p.Checkout.Run(ctx, ref, base, dir); err != nil {
		p.logf("could not check out %s in a container: %v; the diff is read through gh", ref, err)
		return ""
	}
	p.logf("checked out %s under %s", ref, dir)
	return dir
}

// logf writes one line to Log.
func (p *Pipeline) logf(format string, args ...any) {
	if p.Log == nil {
		return
	}
	_, _ = fmt.Fprintf(p.Log, format+"\n", args...)
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
