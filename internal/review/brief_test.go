package review

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testBrief() *Brief {
	return &Brief{
		Ref:                Ref{Repo: testRepo, Number: 7},
		Title:              "widgets: gather the context",
		Summary:            "gathers the context sources a project declares",
		AcceptanceCriteria: []Point{{Text: "a source that cannot read something does not fail the review", Source: "#566"}, {Text: "nothing is truncated"}},
		StyleRules:         []Point{{Text: "every new key needs a test", Source: "CLAUDE.md"}},
		TouchedAreas:       []TouchedArea{{Name: "internal/review", Paths: []string{"gather.go", "sources.go"}, Summary: "the pipeline and its sources"}},
		Sources:            []string{SourceDiff, SourceStyleFiles},
		NotGathered:        []string{"#99 is not an issue that could be read"},
		SessionID:          "sess-1",
	}
}

func TestTheBriefIsWhatTheNextSessionReads(t *testing.T) {
	got := testBrief().Text()
	for _, want := range []string{
		"# Review brief: acme/widgets#7\n",
		"widgets: gather the context\n",
		"https://github.com/acme/widgets/pull/7\n",
		"\n## Summary\n\ngathers the context sources a project declares\n",
		"\n## Acceptance criteria\n\n- a source that cannot read something does not fail the review (#566)\n- nothing is truncated\n",
		"\n## Style rules\n\n- every new key needs a test (CLAUDE.md)\n",
		"\n## Touched areas\n\n### internal/review\n\n- gather.go\n- sources.go\n\nthe pipeline and its sources\n",
		"\n## Gathered from\n\ndiff, style_files\n",
		"\n## Not gathered\n\n- #99 is not an issue that could be read\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the brief does not read %q:\n%s", want, got)
		}
	}
}

func TestABriefWithNothingInASectionHasNoSuchHeading(t *testing.T) {
	brief := &Brief{Ref: Ref{Repo: testRepo, Number: 7}, Summary: "a small change"}
	got := brief.Text()
	for _, gone := range []string{"Acceptance criteria", "Style rules", "Touched areas", "Gathered from", "Not gathered"} {
		if strings.Contains(got, gone) {
			t.Errorf("an empty brief has a %q heading:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "a small change") {
		t.Errorf("the summary is missing:\n%s", got)
	}
}

func TestABriefIsWrittenIntoTheArtifactDirectoryAndReadBack(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review-7")
	want := testBrief()
	if err := WriteBrief(dir, want); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, BriefFile)); err != nil {
		t.Fatalf("the brief is not at %s: %v", BriefFile, err)
	}
	got, err := ReadBrief(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back\n%+v\nwant\n%+v", got, want)
	}
}

func TestReadingABriefThatIsNotThere(t *testing.T) {
	if _, err := ReadBrief(t.TempDir()); err == nil {
		t.Fatal("a directory with no brief in it read as one")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, BriefFile), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadBrief(dir)
	if err == nil || !strings.Contains(err.Error(), BriefFile) {
		t.Fatalf("err = %v, want the file named", err)
	}
}
