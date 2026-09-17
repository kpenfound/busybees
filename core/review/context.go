package review

import (
	"fmt"
	"regexp"
	"strings"
)

// Reference is caller-owned review identity. Its JSON representation is preserved
// in artifacts; core only uses its display text, link and opaque rule scope.
// Implementations used with ReadBrief/ReadArtifact must support JSON decoding.
type Reference interface {
	fmt.Stringer
	URL() string
	ReviewScope() string
}

// Item is one piece of context a source gathered.
type Item struct {
	// Source is the caller-supplied name of the source that produced the item.
	Source string `json:"source"`
	// Name says which piece it is, in whatever way the source names its
	// own: a path, a change reference, a symbol.
	Name string `json:"name"`
	// Content is the text, as it was read.
	Content string `json:"content"`
}

// ExcludedFile is one file the caller took out of the diff before the
// review saw it, a generated one, and what the change did to it: the
// number of lines it added and removed there, and why the file was
// excluded, in the caller's words. The brief lists them so every session
// knows what nobody reviewed, and the judge drops a finding anchored in one.
type ExcludedFile struct {
	Path    string `json:"path"`
	Added   int    `json:"added"`
	Removed int    `json:"removed"`
	Reason  string `json:"reason,omitempty"`
}

// String renders the file for a list: its path, its line counts and, when
// there is one, the reason.
func (f ExcludedFile) String() string {
	if f.Reason == "" {
		return fmt.Sprintf("%s (+%d -%d)", f.Path, f.Added, f.Removed)
	}
	return fmt.Sprintf("%s (+%d -%d; %s)", f.Path, f.Added, f.Removed, f.Reason)
}

// Bundle is caller-supplied context, in acquisition order, without truncation.
type Bundle[R Reference] struct {
	Ref     R        `json:"ref"`
	Title   string   `json:"title,omitempty"`
	Author  string   `json:"author,omitempty"`
	Items   []Item   `json:"items"`
	Skipped []string `json:"skipped,omitempty"`
	// Excluded are the generated files the caller took out of the diff,
	// which is the diff the items hold and the sessions read.
	Excluded []ExcludedFile `json:"excluded,omitempty"`
}

// Of returns the items one source contributed, in bundle order.
func (b *Bundle[R]) Of(source string) []Item {
	var out []Item
	for _, it := range b.Items {
		if it.Source == source {
			out = append(out, it)
		}
	}
	return out
}

// Sources lists the sources that contributed an item, in first-seen order
// without repeats, including when items from a source are interleaved.
func (b *Bundle[R]) Sources() []string {
	seen := make(map[string]struct{})
	var out []string
	for _, it := range b.Items {
		if _, ok := seen[it.Source]; ok {
			continue
		}
		seen[it.Source] = struct{}{}
		out = append(out, it.Source)
	}
	return out
}

// Text renders the bundle as the markdown a session reads: one section per
// item, its content in a fence long enough to hold it, and what was skipped
// at the end.
func (b *Bundle[R]) Text() string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Context for %s\n\n%s\n", b.Ref, b.Ref.URL())
	for _, it := range b.Items {
		fmt.Fprintf(&out, "\n## %s: %s\n\n%s\n", it.Source, it.Name, fenced(it.Content))
	}
	excludedFiles(&out, b.Excluded)
	if len(b.Skipped) > 0 {
		out.WriteString("\n## Not gathered\n\n")
		for _, s := range b.Skipped {
			fmt.Fprintf(&out, "- %s\n", s)
		}
	}
	return out.String()
}

// excludedFiles renders the generated files a diff was read without, one
// line each with its counts, and nothing when there were none.
func excludedFiles(out *strings.Builder, files []ExcludedFile) {
	if len(files) == 0 {
		return
	}
	out.WriteString("\n## Generated files not reviewed\n\nThese files are not in the diff: they are generated, and no session reviews them.\n\n")
	for _, f := range files {
		fmt.Fprintf(out, "- %s\n", f)
	}
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
