package review

import (
	"regexp"
	"strconv"
	"strings"
)

// The diff a person reads beside a finding. NewDiffView turns a pull
// request's unified diff into files, each file into its hunks and each hunk
// into its lines, in the order the diff has them, and marks on it the lines
// one finding is about. It is data for a screen to draw, and draws nothing
// itself.
//
// A finding's lines are marked only when the diff has every one of them on
// the finding's side (Anchors.Has), which is the test Compose posts a
// comment on its lines by. A finding the diff does not have marks nothing,
// and the view says so (DiffView.InDiff) rather than marking the nearest
// lines: the review would fold that finding into its summary, not put it on
// a line.

// Kinds of line in a hunk.
const (
	// LineAdded is a line the change added: it has a line on the new side
	// only.
	LineAdded = "added"
	// LineRemoved is a line the change removed: it has a line on the old
	// side only.
	LineRemoved = "removed"
	// LineContext is a line the change left alone: it has a line on both
	// sides.
	LineContext = "context"
)

// DiffView is a unified diff sectioned by file, then by hunk, with one
// finding's lines marked.
type DiffView struct {
	// Files are the files of the diff, in its order.
	Files []FileSection
	// InDiff reports whether the diff has the finding's lines: its file,
	// and every line of its range on its side. Only then is anything
	// marked. It is false with no finding, for a finding about the change
	// as a whole, for one with a file and no lines, and for one with any
	// line of its range outside the diff, and then nothing is marked.
	InDiff bool
	// MarkedFile and MarkedHunk are the indexes, in Files and in that
	// file's Hunks, of the hunk holding the marked lines, when InDiff: a
	// range the diff has is inside one hunk (Anchors).
	MarkedFile, MarkedHunk int
}

// FileSection is one file of a diff.
type FileSection struct {
	// Path is where GitHub shows the file's lines: its new path, or its
	// old one when the change deleted it. It is the path a finding names.
	Path string
	// OldPath is the file's path before the change, which differs from
	// Path when the change renamed it.
	OldPath string
	// Hunks are the file's hunks, in the diff's order. A file the diff has
	// no lines of (a binary file, a mode change, a rename with no edit)
	// has none.
	Hunks []Hunk
}

// Hunk is one hunk of a file.
type Hunk struct {
	// Header is the hunk's header line as the diff has it:
	// "@@ -10,6 +10,7 @@ func Gather() {".
	Header string
	// OldStart and NewStart are the first line of the hunk on each side.
	OldStart, NewStart int
	// Lines are the hunk's lines. A "\ No newline at end of file" marker
	// is not one of them.
	Lines []DiffLine
}

// DiffLine is one line of a hunk.
type DiffLine struct {
	// Kind is LineAdded, LineRemoved or LineContext.
	Kind string
	// Old and New are the line's numbers on the old and the new side, 0 on
	// the side it is not on.
	Old, New int
	// Text is the line without the diff's leading marker.
	Text string
	// Marked reports whether the finding is about this line: the line is
	// on the finding's side and its number there is in the finding's
	// range. On the new side, a removed line between two marked ones is
	// not marked, and on the old side an added one is not.
	Marked bool
	// Suggestion is the finding's suggestion, the text that would replace
	// the marked lines, on the last of them. It is only ever on a finding
	// on the new side, the only side GitHub can apply a suggestion to.
	Suggestion string
}

// On is the line's number on a side, SideNew or SideOld, and 0 when the
// line is not on it.
func (l DiffLine) On(side string) int {
	switch side {
	case SideNew:
		return l.New
	case SideOld:
		return l.Old
	}
	return 0
}

// NewDiffView reads a unified diff, as `gh pr diff` prints it, into a view
// with f's lines marked, and f's suggestion on the last of them when f is
// on the new side. f may be nil, and then nothing is marked.
func NewDiffView(diff string, f *Finding) *DiffView {
	v := &DiffView{}
	walkDiff(diff, diffWalker{
		file: func(oldPath, path string) {
			v.Files = append(v.Files, FileSection{Path: path, OldPath: oldPath})
		},
		hunk: func(header string, oldStart, newStart int) {
			file := &v.Files[len(v.Files)-1]
			file.Hunks = append(file.Hunks, Hunk{Header: header, OldStart: oldStart, NewStart: newStart})
		},
		line: func(_ string, l DiffLine) {
			file := &v.Files[len(v.Files)-1]
			hunk := &file.Hunks[len(file.Hunks)-1]
			hunk.Lines = append(hunk.Lines, l)
		},
	})
	if f == nil || !ParseAnchors(diff).Has(f) {
		return v
	}
	v.InDiff = true
	for i := range v.Files {
		if v.Files[i].Path != f.File {
			continue
		}
		for j := range v.Files[i].Hunks {
			lines := v.Files[i].Hunks[j].Lines
			for k := range lines {
				n := lines[k].On(f.Side)
				if n < f.Lines.Start || n > f.Lines.End {
					continue
				}
				lines[k].Marked = true
				v.MarkedFile, v.MarkedHunk = i, j
				if f.Side == SideNew && n == f.Lines.End {
					lines[k].Suggestion = f.Suggestion
				}
			}
		}
	}
	return v
}

// diffFileStart matches the line that starts one file in a unified diff.
var diffFileStart = regexp.MustCompile(`^diff --git a/(.*) b/(.*)$`)

// hunkStart matches a hunk header and captures the first line of each side.
var hunkStart = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// diffWalker is what walkDiff calls as it reads a diff. A nil func is not
// called.
type diffWalker struct {
	// file is called at the start of each file with its path before the
	// change and the path its lines are under (FileSection.Path).
	file func(oldPath, path string)
	// hunk is called at the start of each hunk of a file.
	hunk func(header string, oldStart, newStart int)
	// line is called for each line of a hunk, with the path of its file.
	line func(path string, l DiffLine)
}

// walkDiff reads a unified diff line by line, and tells w each file, each
// hunk and each line of a hunk, in the diff's order. It is the one reader
// of a diff in the package: ParseAnchors and NewDiffView both walk with it,
// so the lines a comment can be anchored to and the lines a person is shown
// are the same lines. Anything before the first file is not part of one and
// is skipped.
func walkDiff(diff string, w diffWalker) {
	var path string
	var oldLine, newLine int
	inHunk := false
	for _, line := range strings.Split(diff, "\n") {
		if m := diffFileStart.FindStringSubmatch(line); m != nil {
			path, inHunk = m[2], false
			if w.file != nil {
				w.file(m[1], m[2])
			}
			continue
		}
		if path == "" {
			continue
		}
		if m := hunkStart.FindStringSubmatch(line); m != nil {
			oldLine, _ = strconv.Atoi(m[1])
			newLine, _ = strconv.Atoi(m[2])
			inHunk = true
			if w.hunk != nil {
				w.hunk(line, oldLine, newLine)
			}
			continue
		}
		if !inHunk {
			continue
		}
		// A file's header lines (--- and +++) come before its first
		// hunk, after the diff --git line that ends the hunk before, so
		// inside a hunk a line starting with --- or +++ is a removed or
		// added line whose text starts with two dashes or two pluses.
		var l DiffLine
		switch {
		case strings.HasPrefix(line, "+"):
			l = DiffLine{Kind: LineAdded, New: newLine}
			newLine++
		case strings.HasPrefix(line, "-"):
			l = DiffLine{Kind: LineRemoved, Old: oldLine}
			oldLine++
		case strings.HasPrefix(line, " "):
			l = DiffLine{Kind: LineContext, Old: oldLine, New: newLine}
			newLine++
			oldLine++
		default:
			continue
		}
		l.Text = line[1:]
		if w.line != nil {
			w.line(path, l)
		}
	}
}
