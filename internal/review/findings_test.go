package review

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testFinding() Finding {
	return Finding{
		ID:         "1a2b3c4d",
		Angle:      AngleTests,
		SessionID:  "sess-test_coverage",
		Category:   "missing test",
		Severity:   SeverityHigh,
		File:       "internal/review/gather.go",
		Lines:      LineRange{12, 14},
		Side:       SideNew,
		Title:      "Gather has no test for a source that cannot read",
		Body:       "the acceptance criterion says a source that cannot read something does not fail the review, and nothing exercises it",
		Suggestion: "func TestASourceThatCannotReadDoesNotFailTheReview(t *testing.T) {",
		Evidence:   "gather_test.go has no test naming Skipped",
		Sources:    []string{"#566", "CONTRIBUTING.md"},
		AlsoFrom:   []string{AngleAcceptance},
	}
}

func TestAFindingIsWrittenAsTheSchemaAndReadBack(t *testing.T) {
	want := testFinding()
	data, err := json.Marshal(want)
	if err != nil {
		t.Fatal(err)
	}
	// The field names are the schema's, and the lines are the two-element
	// array the angle prompt asks for, so what a session said and what the
	// review keeps read the same.
	for _, field := range []string{`"id":"1a2b3c4d"`, `"angle":"test_coverage"`, `"session_id":"sess-test_coverage"`, `"category":"missing test"`, `"severity":"high"`,
		`"file":"internal/review/gather.go"`, `"lines":[12,14]`, `"side":"new"`, `"title":"Gather has no test for a source that cannot read"`, `"body":"the acceptance`,
		`"suggestion":"func Test`, `"evidence":"gather_test.go`, `"sources":["#566","CONTRIBUTING.md"]`, `"also_from":["acceptance_criteria"]`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("the JSON lacks %s:\n%s", field, data)
		}
	}
	var got Finding
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back\n%+v\nwant\n%+v", got, want)
	}
}

func TestAFindingAboutTheChangeAsAWholeHasNoAnchor(t *testing.T) {
	f := Finding{ID: "x", Angle: AngleAcceptance, Category: "scope", Severity: SeverityMedium, Title: "the change does more than the issue asked", Body: "..."}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{`"file"`, `"lines"`, `"side"`, `"suggestion"`, `"sources"`, `"also_from"`, `"session_id"`} {
		if strings.Contains(string(data), gone) {
			t.Errorf("an unanchored finding is written with %s:\n%s", gone, data)
		}
	}
	if f.Anchored() {
		t.Error("a finding with no file is anchored")
	}
}

func TestLineRangeIsReadFromEveryShapeASessionWritesIt(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want LineRange
	}{
		{`[12, 14]`, LineRange{12, 14}},
		{`[12]`, LineRange{12, 12}},
		{`12`, LineRange{12, 12}},
		{`{"start": 12, "end": 14}`, LineRange{12, 14}},
		{`{"start": 12}`, LineRange{12, 12}},
		{`[]`, LineRange{}},
		{`null`, LineRange{}},
		// A range that makes no sense reads as no lines, not as an error.
		{`[14, 12]`, LineRange{}},
		{`[0, 3]`, LineRange{}},
		{`-1`, LineRange{}},
	} {
		var got LineRange
		if err := json.Unmarshal([]byte(tc.in), &got); err != nil {
			t.Errorf("%s: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s read as %v, want %v", tc.in, got, tc.want)
		}
	}
	var got LineRange
	if err := json.Unmarshal([]byte(`"twelve"`), &got); err == nil {
		t.Error("a range that is a word read as lines")
	}
}

func TestLineRangeOverlapAndText(t *testing.T) {
	for _, tc := range []struct {
		a, b LineRange
		want bool
	}{
		{LineRange{12, 14}, LineRange{14, 20}, true},
		{LineRange{12, 14}, LineRange{15, 20}, false},
		{LineRange{12, 14}, LineRange{1, 12}, true},
		{LineRange{12, 12}, LineRange{12, 12}, true},
		{LineRange{}, LineRange{}, false},
		{LineRange{12, 14}, LineRange{}, false},
	} {
		if got := tc.a.Overlaps(tc.b); got != tc.want {
			t.Errorf("%v overlaps %v = %v, want %v", tc.a, tc.b, got, tc.want)
		}
		if got := tc.b.Overlaps(tc.a); got != tc.want {
			t.Errorf("%v overlaps %v = %v, want %v", tc.b, tc.a, got, tc.want)
		}
	}
	for r, want := range map[LineRange]string{{}: "", {12, 12}: "12", {12, 14}: "12-14"} {
		if got := r.String(); got != want {
			t.Errorf("%v reads %q, want %q", r, got, want)
		}
	}
}

// sessionAnswer is what an angle session answers with: the JSON object the
// prompt asks for, with these findings in it.
func sessionAnswer(findings ...string) string {
	return `{"findings": [` + strings.Join(findings, ",") + `]}`
}

const (
	rawMissingTest = `{"category": "Missing test", "severity": "high", "file": "internal/review/gather.go", "lines": [12, 14], "side": "new",
		"title": " Gather has no test for a source that cannot read ", "body": "nothing exercises Skipped", "evidence": "gather_test.go has no test naming Skipped", "sources": ["#566", " CONTRIBUTING.md ", ""]}`
	rawWholeChange = `{"category": "scope", "severity": "low", "title": "the change does more than the issue asked", "body": "it also renames Gather", "evidence": "the diff", "lines": [3, 4], "side": "old"}`
)

func TestFindingsAreReadOutOfASessionsAnswer(t *testing.T) {
	got, err := ParseFindings(AngleTests, "sess-1", "Here is what I found:\n\n```json\n"+sessionAnswer(rawMissingTest, rawWholeChange)+"\n```\n")
	if err != nil {
		t.Fatal(err)
	}
	want := []Finding{
		{Angle: AngleTests, SessionID: "sess-1", Category: "missing test", Severity: "high", File: "internal/review/gather.go", Lines: LineRange{12, 14}, Side: SideNew,
			Title: "Gather has no test for a source that cannot read", Body: "nothing exercises Skipped", Evidence: "gather_test.go has no test naming Skipped", Sources: []string{"#566", "CONTRIBUTING.md"}},
		// A finding with no file has no lines and no side either, whatever
		// the session wrote.
		{Angle: AngleTests, SessionID: "sess-1", Category: "scope", Severity: "low", Title: "the change does more than the issue asked", Body: "it also renames Gather", Evidence: "the diff"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parsed\n%+v\nwant\n%+v", got, want)
	}
}

func TestAFindingsAnchorIsMadeConsistent(t *testing.T) {
	got, err := ParseFindings(AngleStyle, "s", sessionAnswer(
		`{"title": "no side", "file": "a.go", "lines": [1, 2]}`,
		`{"title": "right is new", "file": "a.go", "lines": [1, 2], "side": "RIGHT"}`,
		`{"title": "old is old", "file": "a.go", "lines": [1, 2], "side": "Old"}`,
		`{"title": "a bad side is new", "file": "a.go", "side": "sideways"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("got %d findings, want 4", len(got))
	}
	for i, want := range []string{SideNew, SideNew, SideOld, SideNew} {
		if got[i].Side != want {
			t.Errorf("%q: side %q, want %q", got[i].Title, got[i].Side, want)
		}
	}
}

func TestAFindingWithNothingToShowIsDropped(t *testing.T) {
	got, err := ParseFindings(AngleStyle, "s", sessionAnswer(
		`{"title": "", "body": "", "evidence": "x", "file": "a.go"}`,
		`{"title": "  ", "body": "the first line becomes the title\nand the rest stays", "file": "a.go"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d findings, want the one with a body:\n%+v", len(got), got)
	}
	if got[0].Title != "the first line becomes the title" || got[0].Body != "the first line becomes the title\nand the rest stays" {
		t.Errorf("got title %q body %q", got[0].Title, got[0].Body)
	}
}

func TestAnAnswerThatIsNotAFindingsListIsAnError(t *testing.T) {
	for _, answer := range []string{"", "I found nothing worth reporting.", `{"findings": "none"}`, `{"findings": [1, 2]}`} {
		if _, err := ParseFindings(AngleStyle, "s", answer); err == nil {
			t.Errorf("%q read as findings", answer)
		}
	}
	// An empty list, and an object with no list, are a session that found
	// nothing.
	for _, answer := range []string{`{"findings": []}`, `{}`, `{"findings": null}`} {
		got, err := ParseFindings(AngleStyle, "s", answer)
		if err != nil || len(got) != 0 {
			t.Errorf("%q: %v, %+v; want no findings and no error", answer, err, got)
		}
	}
}

func TestFindingsAreWrittenIntoTheArtifactDirectoryAndReadBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review")
	want := &Findings{Items: []Finding{testFinding()}, Skipped: []string{"style: the session failed: boom"}}
	if err := WriteFindings(dir, want); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FindingsFile))
	if err != nil {
		t.Fatalf("the findings are not at %s: %v", FindingsFile, err)
	}
	if !strings.Contains(string(data), `"findings": [`) || !strings.Contains(string(data), `"skipped": [`) {
		t.Errorf("findings.json:\n%s", data)
	}
	got, err := ReadFindings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back\n%+v\nwant\n%+v", got, want)
	}
}

func TestNoFindingsIsAnEmptyListNotANull(t *testing.T) {
	dir := t.TempDir()
	if err := WriteFindings(dir, &Findings{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, FindingsFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"findings": []`) {
		t.Errorf("findings.json:\n%s", data)
	}
	got, err := ReadFindings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Items == nil || len(got.Items) != 0 {
		t.Errorf("read back %#v, want an empty list", got.Items)
	}
	// A file with no list at all reads as an empty one too.
	if err := os.WriteFile(filepath.Join(dir, FindingsFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = ReadFindings(dir); err != nil || got.Items == nil || len(got.Items) != 0 {
		t.Errorf("{} read back as %#v (%v), want an empty list", got, err)
	}
}

func TestReadingFindingsThatAreNotThere(t *testing.T) {
	if _, err := ReadFindings(t.TempDir()); !os.IsNotExist(err) {
		t.Fatalf("a directory with no findings: %v, want not-exist", err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, FindingsFile), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadFindings(dir)
	if err == nil || !strings.Contains(err.Error(), FindingsFile) {
		t.Fatalf("err = %v, want the file named", err)
	}
}
