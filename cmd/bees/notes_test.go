package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

func TestEditorArgv(t *testing.T) {
	for _, tc := range []struct {
		visual, editor string
		want           []string
	}{
		{"", "", []string{"vi"}},
		{"", "nano", []string{"nano"}},
		{"emacs", "nano", []string{"emacs"}},
		{"  ", "nano", []string{"nano"}},
		{"code -w", "", []string{"code", "-w"}},
	} {
		got := editorArgv(tc.visual, tc.editor)
		if strings.Join(got, " ") != strings.Join(tc.want, " ") {
			t.Errorf("editorArgv(%q, %q) = %v, want %v", tc.visual, tc.editor, got, tc.want)
		}
	}
}

func TestNotesSizeText(t *testing.T) {
	for _, tc := range []struct {
		n    int64
		want string
	}{
		{0, "-"},
		{512, "512 B"},
		{4300, "4.2 KB"},
		{2 * 1024 * 1024, "2.0 MB"},
	} {
		if got := notesSizeText(tc.n); got != tc.want {
			t.Errorf("notesSizeText(%d) = %q, want %q", tc.n, got, tc.want)
		}
	}
}

func TestRoleRowsCoverEveryRole(t *testing.T) {
	store := state.New(t.TempDir())
	if err := store.AppendNotes(config.RoleDeveloper, "always run dagger check"); err != nil {
		t.Fatal(err)
	}
	st := state.Status{Singletons: map[string]string{config.RoleQA: "running"}}

	rows := roleRows(store, st)
	if len(rows) != len(config.Roles) {
		t.Fatalf("got %d rows, want one per role (%d)", len(rows), len(config.Roles))
	}
	byRole := map[string]roleRow{}
	for _, r := range rows {
		byRole[r.Role] = r
	}
	if got := byRole[config.RoleDeveloper].State; got != "-" {
		t.Errorf("developer state = %q, want %q (pooled role)", got, "-")
	}
	if got := byRole[config.RoleReviewer].State; got != "-" {
		t.Errorf("reviewer state = %q, want %q (pooled role)", got, "-")
	}
	if got := byRole[config.RoleQA].State; got != "running" {
		t.Errorf("qa state = %q, want %q", got, "running")
	}
	if got := byRole[config.RoleProductManager].State; got != "idle" {
		t.Errorf("product_manager state = %q, want %q", got, "idle")
	}
	n, _ := store.NotesSize(config.RoleDeveloper)
	if byRole[config.RoleDeveloper].Notes != n || n == 0 {
		t.Errorf("developer notes = %d, want the file size %d", byRole[config.RoleDeveloper].Notes, n)
	}
	if byRole[config.RoleQA].Notes != 0 {
		t.Errorf("qa notes = %d, want 0 (no file)", byRole[config.RoleQA].Notes)
	}
	if got := notesBytes(rows)[config.RoleDeveloper]; got != n {
		t.Errorf("notes_bytes[developer] = %d, want %d", got, n)
	}
}

func TestRolesText(t *testing.T) {
	now := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	got := rolesText([]roleRow{
		{Role: config.RoleQA, State: "running", LastRun: now.Add(-3 * time.Minute), Notes: 4300},
		{Role: config.RoleDeveloper, State: "-", Notes: 0},
	}, now)
	want := "" +
		"  qa               running  last run 3m0s ago     notes 4.2 KB\n" +
		"  developer        -        last run never        notes -\n"
	if got != want {
		t.Errorf("rolesText =\n%q\nwant\n%q", got, want)
	}
}

// The command layer holds no logic beyond argument handling; these check the
// wiring: the state directory comes from $BEES_STATE_DIR, so `bees notes` works
// inside a session and without a bees.toml.
func TestNotesCommandsUseTheSessionStateDir(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(session.EnvStateDir, dir)
	store := state.New(dir)

	if err := runRoot(t, "notes", "add", "pjm", "Always run dagger check"); err != nil {
		t.Fatalf("notes add: %v", err)
	}
	notes, err := store.ReadNotes(config.RoleProjectManager)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(notes, "\n- Always run dagger check\n") {
		t.Errorf("project_manager notes = %q, want the bullet appended", notes)
	}
	if err := runRoot(t, "notes", "show", "project-manager"); err != nil {
		t.Errorf("notes show: %v", err)
	}

	if err := runRoot(t, "notes", "reset", "qa"); err != nil {
		t.Fatalf("notes reset: %v", err)
	}
	entries, err := os.ReadDir(store.NotesArchiveDir())
	if err == nil && len(entries) != 0 {
		t.Errorf("reset archived %v, want nothing (qa had no notes)", entries)
	}
	if err := runRoot(t, "notes", "add", "qa", "seed the database first"); err != nil {
		t.Fatal(err)
	}
	if err := runRoot(t, "notes", "reset", "qa"); err != nil {
		t.Fatalf("notes reset: %v", err)
	}
	entries, err = os.ReadDir(store.NotesArchiveDir())
	if err != nil {
		t.Fatalf("archive directory: %v", err)
	}
	if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), "qa-") {
		t.Fatalf("archive holds %v, want one qa-<stamp>.md", entries)
	}
	b, err := os.ReadFile(filepath.Join(store.NotesArchiveDir(), entries[0].Name()))
	if err != nil || !strings.Contains(string(b), "seed the database first") {
		t.Errorf("archived file = %q, %v; want the old notes", b, err)
	}
}

func TestNotesRejectsAnUnknownRole(t *testing.T) {
	t.Setenv(session.EnvStateDir, t.TempDir())
	err := runRoot(t, "notes", "show", "architect")
	if err == nil || !strings.Contains(err.Error(), "unknown role") {
		t.Errorf("notes show architect = %v, want an unknown role error", err)
	}
}

func TestNotesEditRefusesInsideASession(t *testing.T) {
	t.Setenv(session.EnvStateDir, t.TempDir())
	t.Setenv(session.EnvSessionDir, t.TempDir())
	// An editor that exits at once, so dropping the refusal makes this test
	// fail rather than hang waiting on the test binary's stdin.
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "false")
	err := runRoot(t, "notes", "edit", "developer")
	if err == nil || !strings.Contains(err.Error(), "cannot run inside a session") {
		t.Errorf("notes edit inside a session = %v, want an error", err)
	}
}
