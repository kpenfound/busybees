package state

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNotesSize(t *testing.T) {
	s := New(t.TempDir())

	n, err := s.NotesSize("developer")
	if err != nil || n != 0 {
		t.Fatalf("NotesSize on a missing file = %d, %v; want 0, nil", n, err)
	}

	if err := os.MkdirAll(filepath.Dir(s.NotesPath("developer")), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.NotesPath("developer"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	if n, err = s.NotesSize("developer"); err != nil || n != 5 {
		t.Errorf("NotesSize = %d, %v; want 5, nil", n, err)
	}
}

func TestArchiveNotes(t *testing.T) {
	s := New(t.TempDir())
	if err := s.EnsureNotes("qa"); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendNotes("qa", "the smoke test needs a seeded database"); err != nil {
		t.Fatal(err)
	}
	before, err := s.ReadNotes("qa")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	archived, err := s.ArchiveNotes("qa", now)
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(s.NotesArchiveDir(), "qa-20260304-050607.md")
	if archived != want {
		t.Errorf("archive path = %q, want %q", archived, want)
	}
	b, err := os.ReadFile(archived)
	if err != nil {
		t.Fatalf("archive not written: %v", err)
	}
	if string(b) != before {
		t.Errorf("archived content = %q, want %q", b, before)
	}
	fresh, err := s.ReadNotes("qa")
	if err != nil {
		t.Fatal(err)
	}
	if fresh != "# qa notes\n\n" {
		t.Errorf("notes after reset = %q, want a fresh file", fresh)
	}
}

func TestArchiveNotesWithoutANotesFile(t *testing.T) {
	s := New(t.TempDir())
	archived, err := s.ArchiveNotes("reviewer", time.Now())
	if err != nil {
		t.Fatalf("ArchiveNotes on a missing file: %v", err)
	}
	if archived != "" {
		t.Errorf("archive path = %q, want \"\" (nothing to archive)", archived)
	}
	if _, err := os.Stat(s.NotesPath("reviewer")); err != nil {
		t.Errorf("notes file not created: %v", err)
	}
}

func TestAppendNotes(t *testing.T) {
	s := New(t.TempDir())

	if err := s.AppendNotes("developer", "always run dagger check"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadNotes("developer")
	if err != nil {
		t.Fatal(err)
	}
	want := "# developer notes\n\n" + "\n- always run dagger check\n"
	if got != want {
		t.Fatalf("notes = %q, want %q", got, want)
	}

	if err := s.AppendNotes("developer", "and mutation-test the guard"); err != nil {
		t.Fatal(err)
	}
	if got, err = s.ReadNotes("developer"); err != nil {
		t.Fatal(err)
	}
	if suffix := "\n- and mutation-test the guard\n"; !strings.HasSuffix(got, suffix) {
		t.Errorf("notes = %q, want it to end in %q", got, suffix)
	}
	if got != want+"\n- and mutation-test the guard\n" {
		t.Errorf("append did not leave the earlier content alone: %q", got)
	}
}

func TestAppendNotesKeepsEmbeddedNewlines(t *testing.T) {
	s := New(t.TempDir())
	if err := s.AppendNotes("qa", "one\n  two"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadNotes("qa")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(got, "\n- one\n  two\n") {
		t.Errorf("notes = %q, want the text appended verbatim after the bullet", got)
	}
}
