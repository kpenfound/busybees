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
		{Angle: AngleGeneral, Error: "exit status 1"},
		{Angle: AngleSideEffects, SessionID: "sess-s", Answer: "Nothing to report from this angle."},
	}
	got := Judge(runs, nil, nil)
	if want := []string{"Gather has no test for a source that cannot read", "the change does more than the issue asked"}; !reflect.DeepEqual(titles(got.Items), want) {
		t.Errorf("findings %v, want %v", titles(got.Items), want)
	}
	// Each finding says which angle and session found it, for the ask
	// action to resume.
	if got.Items[0].Angle != AngleTests || got.Items[0].SessionID != "sess-t" {
		t.Errorf("the missing-test finding is from %s/%s, want %s/sess-t", got.Items[0].Angle, got.Items[0].SessionID, AngleTests)
	}
	wantSkipped := []string{"general: the session failed: exit status 1", "side_effects: the session answered with no JSON object"}
	if !reflect.DeepEqual(got.Skipped, wantSkipped) {
		t.Errorf("skipped %q, want %q", got.Skipped, wantSkipped)
	}
}

func TestTheJudgeOfNothingIsAnEmptyList(t *testing.T) {
	got := Judge(nil, nil, nil)
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
		got := Merge([]Finding{finding(AngleGeneral, in, "a.go", LineRange{1, 1}, "t")}, nil, nil)
		if len(got) != 1 || got[0].Severity != want {
			t.Errorf("severity %q merged as %+v, want %s", in, got, want)
		}
	}
}

func TestASettingsPinsACategorysSeverityOrDropsIt(t *testing.T) {
	project := &Settings{Categories: map[string]string{"naming": "info", "Missing Test": "high", "scope": "off"}}
	findings := []Finding{
		{Angle: AngleGeneral, Category: "Naming", Severity: "high", Title: "a name", Body: "b"},
		{Angle: AngleTests, Category: "missing test", Severity: "low", Title: "a test", Body: "b"},
		{Angle: AngleAcceptance, Category: "scope", Severity: "high", Title: "the scope", Body: "b"},
		{Angle: AngleAcceptance, Category: "other", Severity: "low", Title: "another", Body: "b"},
	}
	got := Merge(findings, project, nil)
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

func TestFindingsAreOrderedMostSevereFirstThenByPlace(t *testing.T) {
	// Titles that share no word, so nothing here reads alike.
	findings := []Finding{
		finding(AngleGeneral, "low", "b.go", LineRange{1, 1}, "alpha"),
		finding(AngleAcceptance, "high", "", LineRange{}, "bravo"),
		finding(AngleTests, "high", "b.go", LineRange{9, 9}, "charlie"),
		finding(AngleSideEffects, "high", "b.go", LineRange{2, 2}, "delta"),
		finding(AngleGeneral, "high", "b.go", LineRange{2, 2}, "echo"),
		finding(AngleGeneral, "medium", "a.go", LineRange{5, 5}, "foxtrot"),
		finding(AngleGeneral, "info", "a.go", LineRange{1, 1}, "golf"),
		finding(AngleTests, "high", "a.go", LineRange{7, 7}, "hotel"),
	}
	got := Merge(findings, nil, nil)
	// High first; within a severity by file and line, two on one line in
	// angle order (general before side effects), and the finding about the
	// change as a whole after the anchored ones.
	want := []string{"hotel", "echo", "delta", "charlie", "bravo", "foxtrot", "alpha", "golf"}
	if !reflect.DeepEqual(titles(got), want) {
		t.Errorf("order %v, want %v", titles(got), want)
	}
}

func TestAFindingsIDIsStableAndUnique(t *testing.T) {
	// Findings that differ in one of the things an id is made of: the
	// title, the file, the lines, the side, the anchor.
	old := finding(AngleGeneral, "low", "a.go", LineRange{1, 1}, "one")
	old.Side = SideOld
	findings := []Finding{
		finding(AngleGeneral, "low", "a.go", LineRange{1, 1}, "one"),
		finding(AngleTests, "high", "a.go", LineRange{1, 1}, "two"),
		finding(AngleGeneral, "low", "a.go", LineRange{5, 5}, "one"),
		finding(AngleGeneral, "low", "b.go", LineRange{1, 1}, "one"),
		old,
		finding(AngleTests, "high", "", LineRange{}, "three"),
	}
	first := Merge(findings, nil, nil)
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
	again := Merge(append(slices.Clone(first), finding(AngleTests, "high", "a.go", LineRange{1, 1}, "zero")), nil, nil)
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
	alone := Merge([]Finding{finding(AngleTests, "low", "a.go", LineRange{1, 1}, "one")}, nil, nil)
	same := Merge([]Finding{finding(AngleGeneral, "low", "a.go", LineRange{1, 1}, "one"), finding(AngleTests, "low", "a.go", LineRange{1, 1}, "one")}, nil, nil)
	if len(same) != 1 || same[0].ID != alone[0].ID {
		t.Errorf("the same finding twice: %+v; alone %+v", same, alone)
	}
}
