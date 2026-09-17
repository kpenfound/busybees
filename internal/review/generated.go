package review

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	core "github.com/kpenfound/busybees/core/review"
)

// Generated files, and the diff read without them. A pull request that
// regenerates a protobuf binding or a vendored tree changes thousands of
// lines nobody wrote, and a review that reads them wastes its sessions on
// them and sizes the change by them. Before the distiller sees the diff,
// excludeGenerated takes the generated files' sections out of it, and
// what was taken out travels with the bundle (Bundle.Excluded) into the
// brief, where every session reads what was not reviewed, and Exclude
// (core/review's) drops a finding anchored there once the judge has
// merged the angles' findings.
//
// A file is generated when any of three things says so:
//
//   - a `// Code generated ... DO NOT EDIT.` line (generatedHeader) in its
//     head, on the new side, read from the checkout; for a deleted file,
//     on the old side, which the diff holds whole;
//   - the checkout's .gitattributes marks it linguist-generated, which git
//     answers (`git check-attr`) so the patterns mean what git says they
//     mean;
//   - a pattern in context.toml's `generated` list matches it
//     (matchGenerated).
//
// The first two need the checkout of the pull request's head. Without one,
// only the patterns are checked, and the review says so among the context
// it did not gather, so a brief that lists no generated files is read for
// what it is.

// ExcludedFile is one generated file taken out of the diff.
type ExcludedFile = core.ExcludedFile

// The reasons an ExcludedFile records, one per way of telling a file is
// generated.
const (
	// ReasonHeader is a generated header in the file's head.
	ReasonHeader = "generated header"
	// ReasonAttribute is linguist-generated in .gitattributes.
	ReasonAttribute = "linguist-generated in .gitattributes"
	// ReasonPattern is a pattern in context.toml's generated list; the
	// pattern is appended to it.
	ReasonPattern = "generated in context.toml"
)

// generatedHeader is the line that marks a generated file, as Go's own
// convention writes it. A file that has it in its head (generatedHeadLines)
// is generated whatever language it is written in.
var generatedHeader = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

// generatedHeadLines is how far into a file the header is looked for: far
// enough for a licence and an import block above it, not the whole file.
const generatedHeadLines = 64

// generatedAttribute is the .gitattributes attribute that marks a file
// generated, the one GitHub folds away in its own diff view.
const generatedAttribute = "linguist-generated"

// diffSection is one file's part of a unified diff: everything from its
// `diff --git` line to the next file's.
type diffSection struct {
	// text is the section as it was read, with its trailing newline.
	text string
	// oldPath and newPath are the file's paths on each side, "" for the
	// side it does not exist on.
	oldPath, newPath string
	// added and removed count the lines the hunks add and remove.
	added, removed int
	// removedLines are the removed lines' text, in order, up to
	// generatedHeadLines of them: the old side's head for a deleted file.
	removedLines []string
}

// path is the path that names the file: the new one, and the old one for
// a file the change deleted.
func (s *diffSection) path() string {
	if s.newPath != "" {
		return s.newPath
	}
	return s.oldPath
}

// excludeGenerated takes the generated files out of diff and returns what
// is left, with one ExcludedFile per file taken out, in diff order. checkout
// is the checkout of the pull request's head, or "" when there is none, in
// which case the header and .gitattributes checks are skipped and partial
// says so; patterns are context.toml's generated list. An error reading
// .gitattributes is returned in partial too: it costs that check and not the
// review.
func excludeGenerated(ctx context.Context, diff, checkout string, patterns []string) (filtered string, excluded []ExcludedFile, partial string) {
	sections := splitDiff(diff)
	reasons := make([]string, len(sections))
	for i, s := range sections {
		if s.path() == "" {
			continue
		}
		if pattern := matchGenerated(patterns, s.path()); pattern != "" {
			reasons[i] = ReasonPattern + ": " + pattern
		}
	}
	if checkout == "" {
		partial = "generated files: no checkout of the pull request's head, so only the generated patterns of context.toml were checked; .gitattributes and the files' heads were not"
	} else {
		var paths []string
		for i, s := range sections {
			if reasons[i] == "" && s.path() != "" {
				paths = append(paths, s.path())
			}
		}
		marked, err := generatedAttributes(ctx, checkout, paths)
		if err != nil {
			partial = fmt.Sprintf("generated files: %s of the checkout could not be read, so files it marks were not excluded: %v", ".gitattributes", err)
		}
		for i, s := range sections {
			switch {
			case reasons[i] != "" || s.path() == "":
			case marked[s.path()]:
				reasons[i] = ReasonAttribute
			case hasGeneratedHeader(checkout, &sections[i]):
				reasons[i] = ReasonHeader
			}
		}
	}
	var out strings.Builder
	for i, s := range sections {
		if reasons[i] == "" {
			out.WriteString(s.text)
			continue
		}
		excluded = append(excluded, ExcludedFile{Path: s.path(), Added: s.added, Removed: s.removed, Reason: reasons[i]})
	}
	return out.String(), excluded, partial
}

// splitDiff cuts a unified diff into one section per file. Text before the
// first `diff --git` line, which git writes none of, is a section of its
// own with no path, kept as it is.
func splitDiff(diff string) []diffSection {
	var sections []diffSection
	var cur *diffSection
	inHunk := false
	lines := strings.SplitAfter(diff, "\n")
	for _, line := range lines {
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "diff --git ") || cur == nil {
			sections = append(sections, diffSection{})
			cur = &sections[len(sections)-1]
			inHunk = false
			if strings.HasPrefix(line, "diff --git ") {
				cur.oldPath, cur.newPath = headerPaths(strings.TrimRight(line[len("diff --git "):], "\n"))
			}
		}
		cur.text += line
		trimmed := strings.TrimRight(line, "\n")
		switch {
		case strings.HasPrefix(trimmed, "@@"):
			inHunk = true
		case !inHunk && strings.HasPrefix(trimmed, "--- "):
			cur.oldPath = sidePath(trimmed[len("--- "):], "a/")
		case !inHunk && strings.HasPrefix(trimmed, "+++ "):
			cur.newPath = sidePath(trimmed[len("+++ "):], "b/")
		case inHunk && strings.HasPrefix(trimmed, "+"):
			cur.added++
		case inHunk && strings.HasPrefix(trimmed, "-"):
			cur.removed++
			if len(cur.removedLines) < generatedHeadLines {
				cur.removedLines = append(cur.removedLines, trimmed[1:])
			}
		}
	}
	return sections
}

// headerPaths reads the two paths out of a `diff --git a/old b/new` line.
// A path git quoted (core.quotePath: a character outside ASCII, a space
// escaped, a quote) is between double quotes with its escapes, and each
// side is quoted on its own (unquotePath). Unquoted, the paths are the
// same for a file that was not renamed, which is where the line splits; a
// renamed file's split is the last " b/", which a path with " b/" in it
// can put in the wrong place, and the "---"/"+++" lines that follow set
// the paths right when the section has hunks.
func headerPaths(rest string) (oldPath, newPath string) {
	rest = strings.TrimSpace(rest)
	var first, second string
	switch {
	case strings.HasPrefix(rest, `"`):
		quoted, err := strconv.QuotedPrefix(rest)
		if err != nil {
			return "", ""
		}
		first, second = unquotePath(quoted), unquotePath(strings.TrimSpace(rest[len(quoted):]))
	case strings.HasSuffix(rest, `"`):
		i := strings.LastIndex(rest, ` "`)
		if i < 0 {
			return "", ""
		}
		first, second = rest[:i], unquotePath(rest[i+1:])
	default:
		if mid := len(rest) / 2; len(rest)%2 == 1 && rest[mid] == ' ' && rest[:mid] == "a"+rest[mid+2:] {
			first, second = rest[:mid], rest[mid+1:]
			break
		}
		i := strings.LastIndex(rest, " b/")
		if i < 0 {
			return "", ""
		}
		first, second = rest[:i], rest[i+1:]
	}
	if !strings.HasPrefix(first, "a/") || !strings.HasPrefix(second, "b/") {
		return "", ""
	}
	return first[len("a/"):], second[len("b/"):]
}

// sidePath is the path on a "---" or "+++" line without its prefix, and ""
// for /dev/null, the side a file added or deleted does not exist on.
func sidePath(s, prefix string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "\t"); i >= 0 {
		s = s[:i]
	}
	if s == "/dev/null" {
		return ""
	}
	return strings.TrimPrefix(unquotePath(s), prefix)
}

// unquotePath is a path as git spells it with its quotes and escapes
// undone: git quotes a path with a character it escapes the way C does
// (`"gen/\303\251.pb.go"`), which is the syntax a Go string literal has.
// A quoted path Go cannot read is returned without its quotes and with its
// escapes as they were, and an unquoted one as it is.
func unquotePath(s string) string {
	if !strings.HasPrefix(s, `"`) {
		return s
	}
	if u, err := strconv.Unquote(s); err == nil {
		return u
	}
	return strings.Trim(s, `"`)
}

// hasGeneratedHeader reports whether the file s is about has generatedHeader
// in its head: read from the checkout on the new side, and from the
// removed lines the diff holds for a file the change deleted. Only a
// regular file is read: the path comes out of the pull request under
// review, and a symlink it adds can point anywhere on the machine, a FIFO
// would block the read, and neither is a generated file.
func hasGeneratedHeader(checkout string, s *diffSection) bool {
	if s.newPath == "" {
		return headerIn(s.removedLines)
	}
	name := filepath.Join(checkout, filepath.FromSlash(s.newPath))
	if st, err := os.Lstat(name); err != nil || !st.Mode().IsRegular() {
		return false
	}
	f, err := os.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	buf := make([]byte, 16*1024)
	n, _ := f.Read(buf)
	lines := strings.Split(string(buf[:n]), "\n")
	if len(lines) > generatedHeadLines {
		lines = lines[:generatedHeadLines]
	}
	return headerIn(lines)
}

// headerIn reports whether one of lines is the generated header.
func headerIn(lines []string) bool {
	for _, line := range lines {
		if generatedHeader.MatchString(strings.TrimRight(line, "\r")) {
			return true
		}
	}
	return false
}

// generatedAttributes asks git which of paths the checkout's attributes
// mark generated: linguist-generated set, or set to true. It runs git
// once, with every path on its standard input, and an error is git failing
// to answer at all, which a directory that is not a checkout does.
func generatedAttributes(ctx context.Context, checkout string, paths []string) (map[string]bool, error) {
	marked := map[string]bool{}
	if len(paths) == 0 {
		return marked, nil
	}
	cmd := exec.CommandContext(ctx, "git", "check-attr", "-z", "--stdin", generatedAttribute)
	cmd.Dir = checkout
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\x00") + "\x00")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("git check-attr: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	// -z answers path, attribute, value, each NUL-terminated.
	fields := strings.Split(stdout.String(), "\x00")
	for i := 0; i+2 < len(fields); i += 3 {
		switch fields[i+2] {
		case "set", "true":
			marked[fields[i]] = true
		}
	}
	return marked, nil
}

// matchGenerated is the first of patterns that matches p, and "" when none
// does. A pattern is a path or a glob relative to the repository root, with
// `*`, `?` and `[...]` matching within one path segment and `**` matching
// any number of segments; a pattern with no `/` in it matches a file's name
// in any directory; and a pattern that matches a directory matches every
// file under it.
func matchGenerated(patterns []string, p string) string {
	p = strings.TrimPrefix(path.Clean(p), "./")
	for _, pattern := range patterns {
		pattern = strings.TrimSuffix(strings.TrimPrefix(pattern, "./"), "/")
		if pattern == "" {
			continue
		}
		if !strings.Contains(pattern, "/") {
			if ok, _ := path.Match(pattern, path.Base(p)); ok {
				return pattern
			}
		}
		for q := p; q != "." && q != "/" && q != ""; q = path.Dir(q) {
			if matchSegments(strings.Split(pattern, "/"), strings.Split(q, "/")) {
				return pattern
			}
		}
	}
	return ""
}

// matchSegments matches a pattern's segments against a path's, `**`
// standing for any number of them.
func matchSegments(pattern, segments []string) bool {
	for len(pattern) > 0 {
		if pattern[0] == "**" {
			for i := 0; i <= len(segments); i++ {
				if matchSegments(pattern[1:], segments[i:]) {
					return true
				}
			}
			return false
		}
		if len(segments) == 0 {
			return false
		}
		if ok, err := path.Match(pattern[0], segments[0]); err != nil || !ok {
			return false
		}
		pattern, segments = pattern[1:], segments[1:]
	}
	return len(segments) == 0
}

// generatedErrs checks context.toml's generated list: each entry a path
// or glob relative to the repository root, staying inside the repository,
// that path.Match can read. The list is not checked by patternErrs
// because its entries are relative to the repository root, not to the
// directory context.toml is in, and the messages say which.
func generatedErrs(patterns []string) []string {
	var errs []string
	for i, pattern := range patterns {
		switch {
		case strings.TrimSpace(pattern) == "":
			errs = append(errs, fmt.Sprintf("generated[%d] is empty: give it a path or a glob, or drop the entry", i))
		case filepath.IsAbs(pattern):
			errs = append(errs, fmt.Sprintf("generated[%d] %q must be relative to the repository root", i, pattern))
		case !filepath.IsLocal(pattern):
			errs = append(errs, fmt.Sprintf("generated[%d] %q must stay inside the repository", i, pattern))
		default:
			if _, err := path.Match(pattern, ""); err != nil {
				errs = append(errs, fmt.Sprintf("generated[%d] %q is not a valid pattern: %v", i, pattern, err))
			}
		}
	}
	return errs
}
