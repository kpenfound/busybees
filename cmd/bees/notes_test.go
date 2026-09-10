package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/mcpserver"
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

	rows := roleRows(context.Background(), store, mcpserver.FileNotes(store), st)
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

// sizedNotes is a Notes backend holding the notes away from the state
// directory — what notes.backend = "neo4j" is — reporting sizes of its own.
type sizedNotes struct {
	sizes map[string]int64
	err   error
}

func (n sizedNotes) ReadNotes(context.Context, string) (string, error) { return "", n.err }
func (n sizedNotes) WriteNotes(context.Context, string, string) error  { return n.err }

func (n sizedNotes) Size(_ context.Context, role string) (int64, error) {
	if n.err != nil {
		return 0, n.err
	}
	return n.sizes[role], nil
}

// The roles table measures the notes through the configured backend, so
// `bees status` and its --json notes_bytes report what the sessions read
// and write, not the size of a notes file the backend may never touch.
func TestRoleRowsMeasureTheNotesBackend(t *testing.T) {
	store := state.New(t.TempDir())
	if err := store.AppendNotes(config.RoleDeveloper, "always run dagger check"); err != nil {
		t.Fatal(err)
	}
	onDisk, err := store.NotesSize(config.RoleDeveloper)
	if err != nil || onDisk == 0 {
		t.Fatalf("notes file size = %d, %v", onDisk, err)
	}
	notes := sizedNotes{sizes: map[string]int64{config.RoleDeveloper: 40960, config.RoleQA: 12}}

	rows := roleRows(context.Background(), store, notes, state.Status{})
	byRole := map[string]int64{}
	for _, r := range rows {
		byRole[r.Role] = r.Notes
	}
	if got := byRole[config.RoleDeveloper]; got != 40960 {
		t.Errorf("developer notes = %d, want the backend's 40960 (the file holds %d)", got, onDisk)
	}
	if got := byRole[config.RoleQA]; got != 12 {
		t.Errorf("qa notes = %d, want the backend's 12", got)
	}
	if got := notesBytes(rows)[config.RoleDeveloper]; got != 40960 {
		t.Errorf("notes_bytes[developer] = %d, want 40960", got)
	}
	// A backend that cannot answer leaves the column empty rather than
	// failing the command.
	rows = roleRows(context.Background(), store, sizedNotes{err: errors.New("neo4j: connection refused")}, state.Status{})
	for _, r := range rows {
		if r.Notes != 0 {
			t.Errorf("%s notes = %d after a failing backend, want 0", r.Role, r.Notes)
		}
	}
}

// notesBackendFor is the one place notes.backend is turned into a backend,
// and `bees run` and `bees status` call it with the config they already
// loaded: "file" reads and writes the state directory's notes files,
// "neo4j" the service at notes.neo4j_url with the configured key.
func TestNotesBackendForFollowsTheSetting(t *testing.T) {
	var reads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer nams_test" {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/conversations":
			_, _ = fmt.Fprint(w, `{"conversations":[{"id":"c1","userId":"bees-notes-qa","createdAt":"2026-09-09T00:00:00Z"}]}`)
		case "/v1/conversations/c1/messages":
			reads++
			_, _ = fmt.Fprint(w, `{"messages":[{"id":"m1","role":"assistant","content":"# qa notes\n","createdAt":"2026-09-09T00:01:00Z"}]}`)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	stateDir := t.TempDir()
	store := state.New(stateDir)
	ctx := context.Background()

	file := notesBackendFor(config.Notes{Backend: config.NotesBackendFile}, store)
	if err := file.WriteNotes(ctx, config.RoleQA, "# on disk\n"); err != nil {
		t.Fatal(err)
	}
	if n, err := file.Size(ctx, config.RoleQA); err != nil || n != int64(len("# on disk\n")) {
		t.Errorf("file backend size = %d, %v; want %d", n, err, len("# on disk\n"))
	}

	neo4j := notesBackendFor(config.Notes{Backend: config.NotesBackendNeo4j, Neo4jURL: srv.URL + "/v1", Neo4jAPIKey: "nams_test"}, store)
	if got, err := neo4j.ReadNotes(ctx, config.RoleQA); err != nil || got != "# qa notes\n" {
		t.Fatalf("neo4j read = %q, %v", got, err)
	}
	if n, err := neo4j.Size(ctx, config.RoleQA); err != nil || n != int64(len("# qa notes\n")) {
		t.Errorf("neo4j size = %d, %v; want the service's %d", n, err, len("# qa notes\n"))
	}
	if reads != 2 {
		t.Errorf("%d service reads, want 2 (the read and the size)", reads)
	}
	if onDisk, _ := store.ReadNotes(config.RoleQA); onDisk != "# on disk\n" {
		t.Errorf("the neo4j backend touched the notes file: %q", onDisk)
	}
}
