package review

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
