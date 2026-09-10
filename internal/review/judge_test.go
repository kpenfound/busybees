package review

import (
	"reflect"
	"regexp"
	"slices"
	"testing"
)

func finding(angle, severity, file string, lines LineRange, title string) Finding {
	f := Finding{Angle: angle, Category: "about " + title, Severity: severity, File: file, Lines: lines, Title: title, Body: "the body of " + title}
	if f.Anchored() {
		f.Side = SideNew
	}
	return f
}

func titles(findings []Finding) []string {
	var out []string
	for _, f := range findings {
		out = append(out, f.Title)
	}
	return out
}

func TestTheJudgeReadsEveryAngleAndNamesTheOnesItCouldNot(t *testing.T) {
	runs := []AngleRun{
		{Angle: AngleAcceptance, SessionID: "sess-a", Answer: sessionAnswer(rawWholeChange)},
		{Angle: AngleTests, SessionID: "sess-t", Answer: "```json\n" + sessionAnswer(rawMissingTest) + "\n```"},
		{Angle: AngleStyle, Error: "exit status 1"},
		{Angle: AngleSideEffects, SessionID: "sess-s", Answer: "Nothing to report from this angle."},
	}
	got := Judge(runs, nil)
	if want := []string{"Gather has no test for a source that cannot read", "the change does more than the issue asked"}; !reflect.DeepEqual(titles(got.Items), want) {
		t.Errorf("findings %v, want %v", titles(got.Items), want)
	}
	// Each finding says which angle and session found it, for the ask
	// action to resume.
	if got.Items[0].Angle != AngleTests || got.Items[0].SessionID != "sess-t" {
		t.Errorf("the missing-test finding is from %s/%s, want %s/sess-t", got.Items[0].Angle, got.Items[0].SessionID, AngleTests)
	}
	wantSkipped := []string{"style: the session failed: exit status 1", "side_effects: the session answered with no JSON object"}
	if !reflect.DeepEqual(got.Skipped, wantSkipped) {
		t.Errorf("skipped %q, want %q", got.Skipped, wantSkipped)
	}
}

func TestTheJudgeOfNothingIsAnEmptyList(t *testing.T) {
	got := Judge(nil, nil)
	if got.Items == nil || len(got.Items) != 0 || got.Skipped != nil {
		t.Errorf("got %+v, want an empty list and nothing skipped", got)
	}
}

func TestSeverityIsNormalisedToTheFourTheSchemaKnows(t *testing.T) {
	for in, want := range map[string]string{
		"high": SeverityHigh, "High": SeverityHigh, " medium ": SeverityMedium, "low": SeverityLow, "info": SeverityInfo,
		"critical": SeverityHigh, "blocker": SeverityHigh, "major": SeverityHigh,
		"warning": SeverityMedium, "moderate": SeverityMedium,
		"minor": SeverityLow, "nit": SeverityLow, "trivial": SeverityLow,
		"note": SeverityInfo, "informational": SeverityInfo, "suggestion": SeverityInfo,
		// A word the judge does not know, and no word, rank in the middle.
		"purple": SeverityMedium, "": SeverityMedium,
		// off is a project's pin, never a finding's severity.
		"off": SeverityMedium,
	} {
		got := Merge([]Finding{finding(AngleStyle, in, "a.go", LineRange{1, 1}, "t")}, nil)
		if len(got) != 1 || got[0].Severity != want {
			t.Errorf("severity %q merged as %+v, want %s", in, got, want)
		}
	}
}

func TestAProjectPinsACategorysSeverityOrDropsIt(t *testing.T) {
	project := projectWith(t, "[categories]\nnaming = \"info\"\n\"Missing Test\" = \"high\"\nscope = \"off\"\n")
	findings := []Finding{
		{Angle: AngleStyle, Category: "Naming", Severity: "high", Title: "a name", Body: "b"},
		{Angle: AngleTests, Category: "missing test", Severity: "low", Title: "a test", Body: "b"},
		{Angle: AngleAcceptance, Category: "scope", Severity: "high", Title: "the scope", Body: "b"},
		{Angle: AngleAcceptance, Category: "other", Severity: "low", Title: "another", Body: "b"},
	}
	got := Merge(findings, project)
	if want := []string{"a test", "another", "a name"}; !reflect.DeepEqual(titles(got), want) {
		t.Fatalf("findings %v, want %v", titles(got), want)
	}
	// The pin applies whatever case either side wrote the category in, the
	// category is kept lowercased, and the pinned severity is the
	// finding's.
	if got[0].Severity != SeverityHigh || got[0].Category != "missing test" {
		t.Errorf("missing test: %s %q", got[0].Severity, got[0].Category)
	}
	if got[2].Severity != SeverityInfo || got[2].Category != "naming" {
		t.Errorf("naming: %s %q", got[2].Severity, got[2].Category)
	}
	if got[1].Severity != SeverityLow {
		t.Errorf("an unpinned category's severity changed to %s", got[1].Severity)
	}
}

func TestTheSameFindingFromTwoAnglesIsKeptOnce(t *testing.T) {
	a := Finding{Angle: AngleAcceptance, Category: "missing check", Severity: "medium", File: "run.go", Lines: LineRange{40, 42}, Side: SideNew,
		Title: "Run dereferences a nil project", Body: "a nil project panics at line 41", Sources: []string{"#568"}}
	a.Suggestion = "if project == nil {"
	b := Finding{Angle: AngleSideEffects, Category: "nil", Severity: "high", File: "run.go", Lines: LineRange{41, 41}, Side: SideNew,
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
	a := Finding{Angle: AngleStyle, Category: "naming", Severity: "low", File: "run.go", Lines: LineRange{41, 41}, Side: SideNew, Title: "prj is not a name the package uses", Body: "every other file says project"}
	b := Finding{Angle: AngleTests, Category: "missing test", Severity: "high", File: "run.go", Lines: LineRange{41, 41}, Side: SideNew, Title: "the nil branch has no test", Body: "nothing passes a nil project"}
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
	base := Finding{Angle: AngleStyle, Category: "naming", Severity: "low", File: "run.go", Lines: LineRange{41, 41}, Side: SideNew, Title: "prj is not a name the package uses", Body: "every other file says project"}
	for name, change := range map[string]func(*Finding){
		"another file":  func(f *Finding) { f.File = "resume.go" },
		"the old side":  func(f *Finding) { f.Side = SideOld },
		"lines apart":   func(f *Finding) { f.Lines = LineRange{50, 52} },
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

func TestFindingsAreOrderedMostSevereFirstThenByPlace(t *testing.T) {
	// Titles that share no word, so nothing here reads alike.
	findings := []Finding{
		finding(AngleStyle, "low", "b.go", LineRange{1, 1}, "alpha"),
		finding(AngleAcceptance, "high", "", LineRange{}, "bravo"),
		finding(AngleTests, "high", "b.go", LineRange{9, 9}, "charlie"),
		finding(AngleSideEffects, "high", "b.go", LineRange{2, 2}, "delta"),
		finding(AngleStyle, "high", "b.go", LineRange{2, 2}, "echo"),
		finding(AngleStyle, "medium", "a.go", LineRange{5, 5}, "foxtrot"),
		finding(AngleStyle, "info", "a.go", LineRange{1, 1}, "golf"),
		finding(AngleTests, "high", "a.go", LineRange{7, 7}, "hotel"),
	}
	got := Merge(findings, nil)
	// High first; within a severity by file and line, two on one line in
	// angle order (style before side effects), and the finding about the
	// change as a whole after the anchored ones.
	want := []string{"hotel", "echo", "delta", "charlie", "bravo", "foxtrot", "alpha", "golf"}
	if !reflect.DeepEqual(titles(got), want) {
		t.Errorf("order %v, want %v", titles(got), want)
	}
}

func TestAFindingsIDIsStableAndUnique(t *testing.T) {
	// Findings that differ in one of the things an id is made of: the
	// title, the file, the lines, the side, the anchor.
	old := finding(AngleStyle, "low", "a.go", LineRange{1, 1}, "one")
	old.Side = SideOld
	findings := []Finding{
		finding(AngleStyle, "low", "a.go", LineRange{1, 1}, "one"),
		finding(AngleTests, "high", "a.go", LineRange{1, 1}, "two"),
		finding(AngleStyle, "low", "a.go", LineRange{5, 5}, "one"),
		finding(AngleStyle, "low", "b.go", LineRange{1, 1}, "one"),
		old,
		finding(AngleTests, "high", "", LineRange{}, "three"),
	}
	first := Merge(findings, nil)
	if len(first) != len(findings) {
		t.Fatalf("got %d findings, want %d kept apart:\n%+v", len(first), len(findings), first)
	}
	ids := map[string]string{}
	hex := regexp.MustCompile(`^[0-9a-f]{8}$`)
	for _, f := range first {
		if !hex.MatchString(f.ID) {
			t.Errorf("%q has id %q, want eight hex digits", f.Title, f.ID)
		}
		if other, dup := ids[f.ID]; dup {
			t.Errorf("%q and %q share id %s", f.Title, other, f.ID)
		}
		ids[f.ID] = f.Title
	}
	// Merging again, with a finding added, keeps the ids: they hash what
	// places a finding, not where it sits in the list.
	again := Merge(append(slices.Clone(first), finding(AngleTests, "high", "a.go", LineRange{1, 1}, "zero")), nil)
	for _, f := range again {
		if want, ok := ids[f.ID]; ok && want != f.Title {
			t.Errorf("id %s moved from %q to %q", f.ID, want, f.Title)
		}
	}
	if len(again) != len(findings)+1 {
		t.Fatalf("got %d findings, want %d", len(again), len(findings)+1)
	}
	for _, f := range again {
		if f.Title != "zero" {
			continue
		}
		if _, taken := ids[f.ID]; taken {
			t.Errorf("the new finding took an existing id %s", f.ID)
		}
	}
	// Two angles reporting the same line and title are one finding with
	// the id that finding has when one angle reports it.
	alone := Merge([]Finding{finding(AngleTests, "low", "a.go", LineRange{1, 1}, "one")}, nil)
	same := Merge([]Finding{finding(AngleStyle, "low", "a.go", LineRange{1, 1}, "one"), finding(AngleTests, "low", "a.go", LineRange{1, 1}, "one")}, nil)
	if len(same) != 1 || same[0].ID != alone[0].ID {
		t.Errorf("the same finding twice: %+v; alone %+v", same, alone)
	}
}
