package review

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

func testArtifact(dir string) *Artifact {
	return &Artifact{
		Dir:   dir,
		Brief: testBrief(),
		Runs: []AngleRun{
			{Angle: AngleAcceptance, Provider: config.AgentClaude, Model: "opus", Dir: "/tmp/checkout", SessionID: "sess-a", Answer: `{"findings": []}`, Turns: 2, CostUSD: 0.25},
			{Angle: AngleSideEffects, Provider: config.AgentClaude, Model: "opus", Dir: "/tmp/checkout", Error: "exit status 1"},
		},
		Findings: &Findings{Items: []Finding{testFinding()}, Skipped: []string{"side_effects: the session failed: exit status 1"}},
		Triage:   &Triage{Decisions: []Decision{}},
	}
}

func TestAnArtifactDirectoryIsUnderTheStoragePath(t *testing.T) {
	cfg, err := ParseConfig("storage_path = \"kept\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	ref := Ref{Repo: testRepo, Number: 7}
	started := time.Date(2026, 9, 10, 15, 4, 5, 0, time.FixedZone("east", 2*3600))
	got := ArtifactDir(cfg.ResolvedStoragePath(), ref, started)
	// One directory per review, under the pull request's own, named by
	// when it started, in UTC.
	want := filepath.Join(cfg.Dir(), "kept", "acme", "widgets", "7", "20260910-130405")
	if got != want {
		t.Errorf("artifact dir %s, want %s", got, want)
	}
	if _, err := os.Stat(got); !os.IsNotExist(err) {
		t.Errorf("ArtifactDir created something: %v", err)
	}
}

func TestTheLatestArtifactOfAPullRequestIsFound(t *testing.T) {
	storage := t.TempDir()
	ref := Ref{Repo: testRepo, Number: 7}
	if _, err := LatestArtifactDir(storage, ref); err == nil {
		t.Error("a pull request never reviewed has a latest review")
	}
	if _, err := LatestArtifactDir(storage, Ref{Repo: testRepo, Number: 8}); err == nil {
		t.Error("a pull request never reviewed has a latest review")
	}
	base := time.Date(2026, 9, 10, 15, 4, 5, 0, time.UTC)
	var dirs []string
	// Written out of order, so the newest is not the last created.
	for _, d := range []time.Duration{time.Hour, 0, 2 * time.Hour} {
		dir := ArtifactDir(storage, ref, base.Add(d))
		if err := WriteBrief(dir, testBrief()); err != nil {
			t.Fatal(err)
		}
		dirs = append(dirs, dir)
	}
	// A file among the reviews is not one.
	if err := os.WriteFile(filepath.Join(filepath.Dir(dirs[0]), "99999999-999999"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Another pull request's reviews are not this one's.
	if err := WriteBrief(ArtifactDir(storage, Ref{Repo: testRepo, Number: 8}, base.Add(3*time.Hour)), testBrief()); err != nil {
		t.Fatal(err)
	}
	got, err := LatestArtifactDir(storage, ref)
	if err != nil {
		t.Fatal(err)
	}
	if got != dirs[2] {
		t.Errorf("latest %s, want %s", got, dirs[2])
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
	got, err := ReadArtifact(dir)
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
	if _, err := ReadArtifact(dir); err == nil || !strings.Contains(err.Error(), "not a review") {
		t.Errorf("a directory that is not there read as a review: %v", err)
	}
	// A brief alone is a review whose angles have not run.
	if err := WriteBrief(dir, testBrief()); err != nil {
		t.Fatal(err)
	}
	got, err := ReadArtifact(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, &Artifact{Dir: dir, Brief: testBrief()}) {
		t.Errorf("a brief alone read as %+v", got)
	}
	// The angles, then the findings, then the triage state are each read
	// once they are there, and a nil part is not written over.
	run := AngleRun{Angle: AngleTests, Provider: config.AgentClaude, Dir: dir, SessionID: "sess-t", Answer: `{"findings": []}`}
	if err := (&Artifact{Dir: dir, Brief: testBrief(), Runs: []AngleRun{run}}).Write(); err != nil {
		t.Fatal(err)
	}
	if err := (&Artifact{Dir: dir, Brief: testBrief()}).Write(); err != nil {
		t.Fatal(err)
	}
	got, err = ReadArtifact(dir)
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
	got, err = ReadArtifact(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Findings == nil || len(got.Findings.Items) != 0 || got.Triage == nil || len(got.Triage.Decisions) != 0 {
		t.Errorf("after the judge: %+v", got)
	}
}

func TestAnArtifactWithoutABriefIsNotWritten(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "review")
	if err := (&Artifact{Dir: dir, Findings: &Findings{}}).Write(); err == nil {
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
		_, err := ReadArtifact(dir)
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
