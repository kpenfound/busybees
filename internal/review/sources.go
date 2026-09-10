package review

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/text"
)

// StyleFiles are the style documents the style_files source looks for in
// every repository, on top of the ones the project names in style_sources.
// They are the files a project writes its own rules in, whether for a person
// or for an agent.
var StyleFiles = []string{"AGENTS.md", "CLAUDE.md", "CONTRIBUTING.md", "STYLE.md", ".editorconfig"}

// Bounds on the sources that would otherwise follow a pull request into
// work nobody asked for: a body quoting fifty issue numbers, a rename
// touching every symbol in a package, a symbol as common as "New".
const (
	maxLinkedIssues  = 20
	maxCallerSymbols = 20
	maxCallerLines   = 40
)

// diffSource gathers the pull request's diff.
type diffSource struct{}

func (diffSource) Name() string { return SourceDiff }

func (diffSource) Collect(ctx context.Context, in *Input) ([]Item, error) {
	diff, err := in.Diff(ctx)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(diff) == "" {
		in.Skip("the diff of %s is empty", in.Ref)
		return nil, nil
	}
	return []Item{{Source: SourceDiff, Name: in.Ref.String(), Content: diff}}, nil
}

// prBodySource gathers the pull request's title and body, then its
// conversation: every comment and every submitted review, oldest first and
// whoever wrote them.
type prBodySource struct{}

func (prBodySource) Name() string { return SourcePRBody }

func (prBodySource) Collect(ctx context.Context, in *Input) ([]Item, error) {
	pr := in.PR
	var body strings.Builder
	fmt.Fprintf(&body, "%s\n\n", pr.Title)
	fmt.Fprintf(&body, "%s by %s, %s into %s\n", strings.ToLower(pr.State), pr.Author.Login, pr.HeadRefName, pr.BaseRefName)
	if text := strings.TrimSpace(pr.Body); text != "" {
		fmt.Fprintf(&body, "\n%s\n", text)
	}
	items := []Item{{Source: SourcePRBody, Name: in.Ref.String(), Content: body.String()}}

	comments, err := in.Client.CommentsSince(ctx, in.Ref.Number, time.Time{})
	if err != nil {
		return nil, err
	}
	reviews, err := in.Client.ReviewsSince(ctx, in.Ref.Number, time.Time{})
	if err != nil {
		return nil, err
	}
	conversation := append(comments, reviews...)
	sort.SliceStable(conversation, func(i, j int) bool {
		return conversation[i].CreatedAt.Before(conversation[j].CreatedAt)
	})
	for _, a := range conversation {
		name := fmt.Sprintf("%s by %s", a.Kind, a.Author)
		content := strings.TrimSpace(a.Body)
		if a.State != "" {
			content = strings.TrimSpace(a.State + "\n\n" + content)
		}
		if content == "" {
			continue
		}
		items = append(items, Item{Source: SourcePRBody, Name: name, Content: content})
	}
	return items, nil
}

// linkedIssuesSource gathers the issues the pull request closes, then the
// others its body mentions.
type linkedIssuesSource struct{}

func (linkedIssuesSource) Name() string { return SourceLinkedIssues }

func (linkedIssuesSource) Collect(ctx context.Context, in *Input) ([]Item, error) {
	numbers := linkedIssues(in.PR)
	if len(numbers) > maxLinkedIssues {
		in.Skip("%s mentions %d issues: the last %d were not read", in.Ref, len(numbers), len(numbers)-maxLinkedIssues)
		numbers = numbers[:maxLinkedIssues]
	}
	var items []Item
	for _, n := range numbers {
		issue, err := in.Client.GetIssue(ctx, n)
		if err != nil {
			// A "#12" in a body is a reference to anything at all: a pull
			// request, a comment on another repository, a heading in a
			// code block. One that is not an issue is not a failure.
			in.Skip("#%d is mentioned by %s and is not an issue that could be read", n, in.Ref)
			continue
		}
		var body strings.Builder
		fmt.Fprintf(&body, "#%d %s (%s)\n", issue.Number, issue.Title, strings.ToLower(issue.State))
		if text := strings.TrimSpace(issue.Body); text != "" {
			fmt.Fprintf(&body, "\n%s\n", text)
		}
		for _, c := range issue.Comments {
			if text := strings.TrimSpace(c.Body); text != "" {
				fmt.Fprintf(&body, "\ncomment by %s:\n\n%s\n", c.Author.Login, text)
			}
		}
		items = append(items, Item{Source: SourceLinkedIssues, Name: fmt.Sprintf("#%d", n), Content: body.String()})
	}
	return items, nil
}

var issueMention = regexp.MustCompile(`#(\d+)`)

// linkedIssues are the issues a pull request closes, in the order its body
// closes them, then the others its body mentions. Its own number is not one
// of them.
func linkedIssues(pr github.PR) []int {
	var out []int
	seen := map[int]bool{pr.Number: true}
	add := func(n int) {
		if n > 0 && !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	for _, n := range pr.ClosingIssues() {
		add(n)
	}
	for _, m := range issueMention.FindAllStringSubmatch(pr.Body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil {
			continue
		}
		add(n)
	}
	return out
}

// styleFilesSource gathers the repository's style documents: the ones every
// project may have (StyleFiles) and the ones it names in style_sources.
type styleFilesSource struct{}

func (styleFilesSource) Name() string { return SourceStyleFiles }

func (styleFilesSource) Collect(ctx context.Context, in *Input) ([]Item, error) {
	root := in.Root()
	if root == "" {
		in.Skip("no checkout of %s: its style files were not read", in.Ref.Repo)
		return nil, nil
	}
	// A built-in name that no repository has is not worth reporting; a
	// style source the project asked for and that matches nothing is.
	g := &fileGatherer{in: in, source: SourceStyleFiles, root: root, read: map[string]bool{}}
	g.add(StyleFiles, false)
	g.add(in.Project.StyleSources, true)
	return g.items, nil
}

// fileSource gathers the files a source of the project's own names.
type fileSource struct {
	name  string
	files []string
}

func (f fileSource) Name() string { return f.name }

func (f fileSource) Collect(ctx context.Context, in *Input) ([]Item, error) {
	root := in.Root()
	if root == "" {
		in.Skip("no checkout of %s: the files of context source %q were not read", in.Ref.Repo, f.name)
		return nil, nil
	}
	g := &fileGatherer{in: in, source: f.name, root: root, read: map[string]bool{}}
	g.add(f.files, true)
	return g.items, nil
}

// fileGatherer reads the files a source's patterns match under root, one
// item each, in pattern order and reading no file twice however many
// patterns match it.
type fileGatherer struct {
	in     *Input
	source string
	root   string
	read   map[string]bool
	items  []Item
}

// add gathers the files patterns match. With report set, a pattern that
// matches nothing is recorded as skipped: the project asked for it by name,
// so an empty answer is context it expected and did not get.
func (g *fileGatherer) add(patterns []string, report bool) {
	for _, pattern := range patterns {
		matches, err := filepath.Glob(filepath.Join(g.root, pattern))
		if err != nil {
			g.in.Skip("%s: %q is not a valid pattern: %v", g.source, pattern, err)
			continue
		}
		sort.Strings(matches)
		found := 0
		for _, path := range matches {
			// The pattern comes out of the repository under review, which
			// is the thing being reviewed: a path that leaves the checkout
			// is not read, however it was spelled.
			rel, err := filepath.Rel(g.root, path)
			if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
				g.in.Skip("%s: %q leaves the checkout and was not read", g.source, pattern)
				break
			}
			if g.read[rel] {
				found++ // read for an earlier pattern, so this one matched
				continue
			}
			data, err := os.ReadFile(path)
			if err != nil {
				// A directory, or a file the person cannot read: either
				// way it is context that is not there.
				continue
			}
			g.read[rel] = true
			found++
			g.items = append(g.items, Item{Source: g.source, Name: rel, Content: string(data)})
		}
		if report && found == 0 {
			g.in.Skip("%s: %q matched no file in the checkout", g.source, pattern)
		}
	}
}

// callersSource gathers the lines that name a symbol the diff declares,
// outside the files the diff changed: what the change reaches that its own
// diff does not show.
type callersSource struct{}

func (callersSource) Name() string { return SourceCallers }

func (callersSource) Collect(ctx context.Context, in *Input) ([]Item, error) {
	if in.Dir == "" {
		in.Skip("no checkout of %s: callers of the changed symbols were not looked for", in.Ref.Repo)
		return nil, nil
	}
	// `git grep` searches the directory it runs in and below, and names the
	// files it found relative to it. Who calls a changed symbol is a
	// question about the whole repository, and the answers have to line up
	// with the diff's paths, which are relative to the repository root: so
	// search from the top of the checkout, wherever the review was run from
	// and wherever context.toml sits.
	top, _, err := gitRun(ctx, in.Dir, "rev-parse", "--show-toplevel")
	if err != nil {
		in.Skip("%s is not a git checkout: callers of the changed symbols were not looked for", in.Dir)
		return nil, nil
	}
	top = strings.TrimSpace(top)
	diff, err := in.Diff(ctx)
	if err != nil {
		return nil, err
	}
	symbols := changedSymbols(diff)
	if len(symbols) > maxCallerSymbols {
		in.Skip("%s declares %d symbols: callers of the last %d were not looked for", in.Ref, len(symbols), len(symbols)-maxCallerSymbols)
		symbols = symbols[:maxCallerSymbols]
	}
	changed := changedFiles(diff)
	var items []Item
	for _, symbol := range symbols {
		out, code, err := gitRun(ctx, top, "grep", "-n", "--fixed-strings", "--word-regexp", "--", symbol)
		if code == 1 {
			continue // git grep says "no match" with a status, not an error
		}
		if err != nil {
			return nil, err
		}
		lines := callerLines(out, changed)
		if len(lines) == 0 {
			continue
		}
		if len(lines) > maxCallerLines {
			lines = append(lines[:maxCallerLines], fmt.Sprintf("... and %s naming %s", text.Count(len(lines)-maxCallerLines, "more line"), symbol))
		}
		items = append(items, Item{Source: SourceCallers, Name: symbol, Content: strings.Join(lines, "\n") + "\n"})
	}
	return items, nil
}

// callerLines keeps the `git grep` matches that are outside the files the
// diff changed: those files are in the bundle already, whole.
func callerLines(out string, changed map[string]bool) []string {
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		path, _, ok := strings.Cut(line, ":")
		if line == "" || !ok || changed[path] {
			continue
		}
		lines = append(lines, line)
	}
	return lines
}

// declarations are the shapes a line that declares a symbol takes, across
// the languages a pull request is likely to be written in. They are a
// heuristic over the diff's own text: a symbol they miss costs the review
// one source, and one they invent finds no callers.
var declarations = []*regexp.Regexp{
	regexp.MustCompile(`^func\s+(?:\([^)]*\)\s*)?([A-Za-z_]\w*)`),
	regexp.MustCompile(`^(?:export\s+)?(?:async\s+)?(?:def|function)\s+([A-Za-z_]\w*)`),
	regexp.MustCompile(`^(?:export\s+)?(?:public\s+|pub\s+)?(?:default\s+)?(?:abstract\s+)?(?:type|class|interface|struct|enum|trait|fn)\s+([A-Za-z_]\w*)`),
	regexp.MustCompile(`^(?:export\s+)?(?:const|let|var)\s+([A-Za-z_]\w*)\s*[:=]`),
}

// changedSymbols are the symbols the diff's added and removed lines
// declare, in the order they first appear.
func changedSymbols(diff string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(diff, "\n") {
		if len(line) < 2 || (line[0] != '+' && line[0] != '-') {
			continue
		}
		body := strings.TrimSpace(line[1:])
		for _, re := range declarations {
			m := re.FindStringSubmatch(body)
			if m == nil || seen[m[1]] {
				continue
			}
			seen[m[1]] = true
			out = append(out, m[1])
			break
		}
	}
	return out
}

var diffFile = regexp.MustCompile(`(?m)^\+\+\+ b/(.+)$`)

// changedFiles are the paths the diff writes to.
func changedFiles(diff string) map[string]bool {
	out := map[string]bool{}
	for _, m := range diffFile.FindAllStringSubmatch(diff, -1) {
		if path := strings.TrimSpace(m[1]); path != "" && path != "/dev/null" {
			out[path] = true
		}
	}
	return out
}

// gitRun runs git in dir and returns its output together with its exit
// status. The status is what tells `git grep`'s "nothing matched" (1) from a
// search that could not run at all, which workspace.Git — used everywhere a
// non-zero status is only ever a failure — has no room for.
func gitRun(ctx context.Context, dir string, args ...string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		code = exit.ExitCode()
	}
	if err != nil {
		err = fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), code, err
}
