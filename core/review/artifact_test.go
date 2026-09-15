package review

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func testArtifact(dir string) *Artifact[testRef] {
	return &Artifact[testRef]{
		Dir:   dir,
		Brief: testBrief(),
		Runs: []AngleRun{
			{Angle: AngleAcceptance, Provider: "fake", Model: "opus", Dir: "/tmp/checkout", SessionID: "sess-a", Answer: `{"findings": []}`, Turns: 2, CostUSD: 0.25},
			{Angle: AngleSideEffects, Provider: "fake", Model: "opus", Dir: "/tmp/checkout", Error: "exit status 1"},
		},
		Findings: &Findings{Items: []Finding{testFinding()}, Skipped: []string{"side_effects: the session failed: exit status 1"}},
		Triage:   &Triage{Decisions: []Decision{}},
	}
}
func TestAnArtifactIsWrittenAndReadBackIntoTheSameStructs(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review")
	want := testArtifact(dir)
	if err := want.Write(); err != nil {
		t.Fatal(err)
	}
	// The layout the package comment documents.
	for _, name := range []string{BriefFile, filepath.Join(AnglesDir, AngleAcceptance+".json"), filepath.Join(AnglesDir, AngleSideEffects+".json"), FindingsFile, TriageFile} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("%s is not in the artifact: %v", name, err)
		}
	}
	got, err := ReadArtifact[testRef](dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("read back\n%+v\nwant\n%+v", got, want)
	}
	// The session ids the artifact holds are the ones the angles can be
	// resumed from.
	if got.Runs[0].SessionID != "sess-a" || got.Brief.SessionID != "sess-1" {
		t.Errorf("session ids: brief %q, angle %q", got.Brief.SessionID, got.Runs[0].SessionID)
	}
}

func TestAnArtifactIsReadAsFarAsTheReviewGot(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review")
	if _, err := ReadArtifact[testRef](dir); err == nil || !strings.Contains(err.Error(), "not a review") {
		t.Errorf("a directory that is not there read as a review: %v", err)
	}
	// A brief alone is a review whose angles have not run.
	if err := WriteBrief(dir, testBrief()); err != nil {
		t.Fatal(err)
	}
	got, err := ReadArtifact[testRef](dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, &Artifact[testRef]{Dir: dir, Brief: testBrief()}) {
		t.Errorf("a brief alone read as %+v", got)
	}
	// The angles, then the findings, then the triage state are each read
	// once they are there, and a nil part is not written over.
	run := AngleRun{Angle: AngleTests, Provider: "fake", Dir: dir, SessionID: "sess-t", Answer: `{"findings": []}`}
	if err := (&Artifact[testRef]{Dir: dir, Brief: testBrief(), Runs: []AngleRun{run}}).Write(); err != nil {
		t.Fatal(err)
	}
	if err := (&Artifact[testRef]{Dir: dir, Brief: testBrief()}).Write(); err != nil {
		t.Fatal(err)
	}
	got, err = ReadArtifact[testRef](dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Runs, []AngleRun{run}) || got.Findings != nil || got.Triage != nil {
		t.Errorf("after the angles: %+v", got)
	}
	if err := WriteFindings(dir, &Findings{}); err != nil {
		t.Fatal(err)
	}
	if err := WriteTriage(dir, &Triage{}); err != nil {
		t.Fatal(err)
	}
	got, err = ReadArtifact[testRef](dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Findings == nil || len(got.Findings.Items) != 0 || got.Triage == nil || len(got.Triage.Decisions) != 0 {
		t.Errorf("after the judge: %+v", got)
	}
}

func TestAnArtifactWithoutABriefIsNotWritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review")
	if err := (&Artifact[testRef]{Dir: dir, Findings: &Findings{}}).Write(); err == nil {
		t.Fatal("an artifact with no brief was written")
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Errorf("the directory was created anyway: %v", err)
	}
}

func TestAPartOfAnArtifactThatDoesNotReadIsAnErrorNamingIt(t *testing.T) {
	for _, name := range []string{FindingsFile, TriageFile, filepath.Join(AnglesDir, AngleSideEffects+".json")} {
		dir := filepath.Join(t.TempDir(), "review")
		if err := WriteBrief(dir, testBrief()); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, AnglesDir), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte("not json"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := ReadArtifact[testRef](dir)
		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s unreadable: err = %v, want it named", name, err)
		}
	}
}

func TestTheTriageStateStartsEmpty(t *testing.T) {
	dir := t.TempDir()
	if err := WriteTriage(dir, &Triage{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, TriageFile))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"decisions": []`) {
		t.Errorf("triage.json:\n%s", data)
	}
	got, err := ReadTriage(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, &Triage{Decisions: []Decision{}}) {
		t.Errorf("read back %+v", got)
	}
	// A file with no list at all reads as an empty one too.
	if err := os.WriteFile(filepath.Join(dir, TriageFile), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err = ReadTriage(dir); err != nil || !reflect.DeepEqual(got, &Triage{Decisions: []Decision{}}) {
		t.Errorf("{} read back as %+v (%v)", got, err)
	}
	// A decision round-trips, so the triage queue can keep its state here.
	want := &Triage{Decisions: []Decision{{Finding: "1a2b3c4d", Action: "dismiss", Reason: "the test exists under another name"}, {Finding: "5e6f7a8b", Action: "select", Comment: "edited"}}}
	if err := WriteTriage(dir, want); err != nil {
		t.Fatal(err)
	}
	if got, err = ReadTriage(dir); err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("read back %+v (%v), want %+v", got, err, want)
	}
}
