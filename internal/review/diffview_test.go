package review

import (
	"fmt"
	"reflect"
	"slices"
	"testing"
)

func TestADiffViewIsSectionedByFileThenHunk(t *testing.T) {
	v := NewDiffView(sampleDiff, nil)
	want := []FileSection{
		{Path: "gather.go", OldPath: "gather.go", Hunks: []Hunk{
			{Header: "@@ -10,6 +10,7 @@ func Gather() {", OldStart: 10, NewStart: 10, Lines: []DiffLine{
				{Kind: LineContext, Old: 10, New: 10, Text: "\ta()"},
				{Kind: LineContext, Old: 11, New: 11, Text: "\tb()"},
				{Kind: LineRemoved, Old: 12, Text: "\tc()"},
				{Kind: LineAdded, New: 12, Text: "\tc(1)"},
				{Kind: LineAdded, New: 13, Text: "\td()"},
				{Kind: LineContext, Old: 13, New: 14, Text: "\te()"},
				{Kind: LineContext, Old: 14, New: 15, Text: "\tf()"},
				{Kind: LineContext, Old: 15, New: 16, Text: "\tg()"},
			}},
			{Header: "@@ -40,3 +41,4 @@ func Other() {", OldStart: 40, NewStart: 41, Lines: []DiffLine{
				{Kind: LineContext, Old: 40, New: 41, Text: "\tx()"},
				{Kind: LineContext, Old: 41, New: 42, Text: "\ty()"},
				{Kind: LineContext, Old: 42, New: 43, Text: "\tz()"},
				{Kind: LineAdded, New: 44, Text: "\tw()"},
			}},
		}},
		{Path: "old.go", OldPath: "old.go", Hunks: []Hunk{
			{Header: "@@ -1,2 +0,0 @@", OldStart: 1, NewStart: 0, Lines: []DiffLine{
				{Kind: LineRemoved, Old: 1, Text: "package old"},
				{Kind: LineRemoved, Old: 2, Text: ""},
			}},
		}},
		{Path: "after.go", OldPath: "before.go", Hunks: []Hunk{
			{Header: "@@ -1,2 +1,2 @@", OldStart: 1, NewStart: 1, Lines: []DiffLine{
				{Kind: LineContext, Old: 1, New: 1, Text: "package widgets"},
				{Kind: LineRemoved, Old: 2, Text: "// before"},
				{Kind: LineAdded, New: 2, Text: "// after"},
			}},
		}},
		{Path: "schema.sql", OldPath: "schema.sql", Hunks: []Hunk{
			{Header: "@@ -1,3 +1,3 @@", OldStart: 1, NewStart: 1, Lines: []DiffLine{
				{Kind: LineContext, Old: 1, New: 1, Text: "create table widgets (id int);"},
				{Kind: LineRemoved, Old: 2, Text: "-- the old comment"},
				{Kind: LineAdded, New: 2, Text: "++ the new comment, which starts with two pluses"},
				{Kind: LineContext, Old: 3, New: 3, Text: "create table gadgets (id int);"},
			}},
		}},
	}
	if !reflect.DeepEqual(v.Files, want) {
		t.Errorf("files:\n got %+v\nwant %+v", v.Files, want)
	}
	if v.InDiff {
		t.Error("a view with no finding says a finding is in the diff")
	}
}

func TestADiffViewIsEmptyForADiffWithNoFile(t *testing.T) {
	// Hunks before the first diff --git line belong to no file, and a file
	// with no hunks (a pure rename) is a section with none.
	v := NewDiffView("@@ -1 +1 @@\n-a\n+b\ndiff --git a/x.go b/y.go\nsimilarity index 100%\nrename from x.go\nrename to y.go\n", nil)
	want := []FileSection{{Path: "y.go", OldPath: "x.go"}}
	if !reflect.DeepEqual(v.Files, want) {
		t.Errorf("files: got %+v, want %+v", v.Files, want)
	}
}

func TestADiffViewMarksAFindingOnlyWhereTheDiffHasIt(t *testing.T) {
	for _, c := range []struct {
		name      string
		finding   *Finding
		inDiff    bool
		marked    []string // file#hunk:line
		suggested []string // file#hunk:line=suggestion
		at        [2]int   // MarkedFile, MarkedHunk
	}{
		{
			name:      "new side, with a suggestion on the last line",
			finding:   &Finding{File: "gather.go", Side: SideNew, Lines: LineRange{12, 13}, Suggestion: "\tc(1) // tested\n\td()"},
			inDiff:    true,
			marked:    []string{"gather.go#0:3", "gather.go#0:4"},
			suggested: []string{"gather.go#0:4=\tc(1) // tested\n\td()"},
		},
		{
			name:      "new side, across a removed line, which is not marked",
			finding:   &Finding{File: "gather.go", Side: SideNew, Lines: LineRange{11, 12}, Suggestion: "\tb(1)"},
			inDiff:    true,
			marked:    []string{"gather.go#0:1", "gather.go#0:3"},
			suggested: []string{"gather.go#0:3=\tb(1)"},
		},
		{
			name:    "new side, without a suggestion, in the second hunk",
			finding: &Finding{File: "gather.go", Side: SideNew, Lines: LineRange{44, 44}},
			inDiff:  true,
			marked:  []string{"gather.go#1:3"},
			at:      [2]int{0, 1},
		},
		{
			name:    "old side, a removed line, its suggestion not attached",
			finding: &Finding{File: "gather.go", Side: SideOld, Lines: LineRange{12, 12}, Suggestion: "\tc(2)"},
			inDiff:  true,
			marked:  []string{"gather.go#0:2"},
		},
		{
			name:    "old side, context lines by their old numbers",
			finding: &Finding{File: "gather.go", Side: SideOld, Lines: LineRange{13, 14}},
			inDiff:  true,
			marked:  []string{"gather.go#0:5", "gather.go#0:6"},
		},
		{
			name:    "old side of a deleted file, its suggestion not attached",
			finding: &Finding{File: "old.go", Side: SideOld, Lines: LineRange{1, 2}, Suggestion: "package older"},
			inDiff:  true,
			marked:  []string{"old.go#0:0", "old.go#0:1"},
			at:      [2]int{1, 0},
		},
		{
			name:      "a renamed file under its new path",
			finding:   &Finding{File: "after.go", Side: SideNew, Lines: LineRange{2, 2}, Suggestion: "// later"},
			inDiff:    true,
			marked:    []string{"after.go#0:2"},
			suggested: []string{"after.go#0:2=// later"},
			at:        [2]int{2, 0},
		},
		{
			name:    "a finding about the change as a whole",
			finding: &Finding{Title: "The change renames Gather", Suggestion: "x"},
		},
		{
			name:    "a file and no lines",
			finding: &Finding{File: "gather.go", Side: SideNew, Suggestion: "x"},
		},
		{
			name:    "a file the diff does not touch",
			finding: &Finding{File: "nowhere.go", Side: SideNew, Lines: LineRange{1, 1}, Suggestion: "x"},
		},
		{
			name:    "a range leaving its hunk marks none of it",
			finding: &Finding{File: "gather.go", Side: SideNew, Lines: LineRange{16, 17}, Suggestion: "x"},
		},
		{
			name:    "a side the file does not have",
			finding: &Finding{File: "old.go", Side: SideNew, Lines: LineRange{1, 1}, Suggestion: "x"},
		},
		{
			name:    "a renamed file under its old path",
			finding: &Finding{File: "before.go", Side: SideNew, Lines: LineRange{2, 2}, Suggestion: "x"},
		},
		{
			name: "no finding",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			v := NewDiffView(sampleDiff, c.finding)
			if v.InDiff != c.inDiff {
				t.Errorf("InDiff = %v, want %v", v.InDiff, c.inDiff)
			}
			if c.finding != nil && v.InDiff != ParseAnchors(sampleDiff).Has(c.finding) {
				t.Errorf("InDiff = %v, and Anchors.Has disagrees", v.InDiff)
			}
			marked, suggested := marksOf(v)
			if !slices.Equal(marked, c.marked) {
				t.Errorf("marked %q, want %q", marked, c.marked)
			}
			if !slices.Equal(suggested, c.suggested) {
				t.Errorf("suggestions %q, want %q", suggested, c.suggested)
			}
			if at := [2]int{v.MarkedFile, v.MarkedHunk}; at != c.at {
				t.Errorf("marked hunk at %v, want %v", at, c.at)
			}
		})
	}
}

// marksOf lists the lines of a view that are marked and the ones that carry
// a suggestion, as file#hunk:line with the indexes of the hunk in its file
// and of the line in its hunk.
func marksOf(v *DiffView) (marked, suggested []string) {
	for _, file := range v.Files {
		for h, hunk := range file.Hunks {
			for i, l := range hunk.Lines {
				where := fmt.Sprintf("%s#%d:%d", file.Path, h, i)
				if l.Marked {
					marked = append(marked, where)
				}
				if l.Suggestion != "" {
					suggested = append(suggested, where+"="+l.Suggestion)
				}
			}
		}
	}
	return marked, suggested
}
