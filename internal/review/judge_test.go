package review

import (
	"reflect"
	"testing"
)

func titles(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Title)
	}
	return out
}

func TestTheSameFindingFromTwoAnglesIsKeptOnce(t *testing.T) {
	a := Finding{Angle: AngleAcceptance, Category: "missing check", Severity: "medium", File: "run.go", Lines: LineRange{Start: 40, End: 42}, Side: SideNew,
		Title: "Run dereferences a nil project", Body: "a nil project panics at line 41", Sources: []string{"#568"}}
	a.Suggestion = "if project == nil {"
	b := Finding{Angle: AngleSideEffects, Category: "nil", Severity: "high", File: "run.go", Lines: LineRange{Start: 41, End: 41}, Side: SideNew,
		Title: "nil project dereferenced in Run", Body: "callers pass nil for a repository with no context.toml", Sources: []string{"angles.go"}}
	// Two more reports of it: one from a third angle, one from the angle
	// whose report is kept.
	c, d := a, b
	c.Angle, c.Severity = AngleTests, "low"
	d.Severity = "low"
	got := Merge([]Finding{a, b, c, d}, nil)
	if len(got) != 1 {
		t.Fatalf("got %d findings, want one:\n%+v", len(got), got)
	}
	// The more severe report is the one kept, with each other angle that
	// reported it named once and its own angle not named, the others'
	// sources on it, and a suggestion since the kept one had none.
	want := b
	want.ID = got[0].ID
	want.AlsoFrom = []string{AngleAcceptance, AngleTests}
	want.Sources = []string{"angles.go", "#568"}
	want.Suggestion = a.Suggestion
	if !reflect.DeepEqual(got[0], want) {
		t.Errorf("merged\n%+v\nwant\n%+v", got[0], want)
	}
	// Merged again behind a fresh report of it, the finding keeps the
	// angles it already named.
	fresh := a
	fresh.Severity = "high"
	again := Merge([]Finding{fresh, got[0]}, nil)
	if wantAlso := []string{AngleSideEffects, AngleTests}; len(again) != 1 || !reflect.DeepEqual(again[0].AlsoFrom, wantAlso) {
		t.Errorf("re-merged: %+v, want also_from %v", again, wantAlso)
	}
}

func TestTwoFindingsOnTheSameLinesAreTwoWhenTheyReadUnalike(t *testing.T) {
	a := Finding{Angle: AngleGeneral, Category: "naming", Severity: "low", File: "run.go", Lines: LineRange{Start: 41, End: 41}, Side: SideNew, Title: "prj is not a name the package uses", Body: "every other file says project"}
	b := Finding{Angle: AngleTests, Category: "missing test", Severity: "high", File: "run.go", Lines: LineRange{Start: 41, End: 41}, Side: SideNew, Title: "the nil branch has no test", Body: "nothing passes a nil project"}
	if got := Merge([]Finding{a, b}, nil); len(got) != 2 {
		t.Errorf("two different problems on one line merged into %d:\n%+v", len(got), got)
	}
	// The same category on the same lines is the same finding, however it
	// is worded.
	b.Category = "naming"
	if got := Merge([]Finding{a, b}, nil); len(got) != 1 {
		t.Errorf("the same category on one line kept as %d:\n%+v", len(got), got)
	}
}

func TestFindingsInDifferentPlacesAreNotMerged(t *testing.T) {
	base := Finding{Angle: AngleGeneral, Category: "naming", Severity: "low", File: "run.go", Lines: LineRange{Start: 41, End: 41}, Side: SideNew, Title: "prj is not a name the package uses", Body: "every other file says project"}
	for name, change := range map[string]func(*Finding){
		"another file":  func(f *Finding) { f.File = "resume.go" },
		"the old side":  func(f *Finding) { f.Side = SideOld },
		"lines apart":   func(f *Finding) { f.Lines = LineRange{Start: 50, End: 52} },
		"no anchor":     func(f *Finding) { f.File, f.Lines, f.Side = "", LineRange{}, "" },
		"another angle": func(f *Finding) { f.Angle = AngleTests },
	} {
		other := base
		change(&other)
		want := 2
		if name == "another angle" {
			want = 1
		}
		// In both orders: which of the two the judge sees first must not
		// matter.
		if got := Merge([]Finding{base, other}, nil); len(got) != want {
			t.Errorf("%s: %d findings, want %d", name, len(got), want)
		}
		if got := Merge([]Finding{other, base}, nil); len(got) != want {
			t.Errorf("%s, other first: %d findings, want %d", name, len(got), want)
		}
	}
	// Two findings about the change as a whole are one when they read
	// alike and two when they do not.
	whole := func(title string) Finding {
		return Finding{Angle: AngleAcceptance, Category: "scope", Severity: "low", Title: title, Body: ""}
	}
	if got := Merge([]Finding{whole("the change renames Gather without an issue asking"), whole("Gather renamed, no issue asking for the change")}, nil); len(got) != 1 {
		t.Errorf("two alike whole-change findings kept as %d", len(got))
	}
	if got := Merge([]Finding{whole("the change renames Gather without an issue asking"), whole("no migration for the storage path")}, nil); len(got) != 2 {
		t.Errorf("two unalike whole-change findings kept as %d", len(got))
	}
}
