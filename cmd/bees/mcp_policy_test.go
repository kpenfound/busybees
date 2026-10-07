package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// TestIssuePolicyFollowsTheConfig covers the one place the CLI and the MCP
// backend build the policy issues are created under: with
// scheduler.feature_proposals = false the proposal gate is off, and the
// backend a session's tools go through reads the same key.
func TestIssuePolicyFollowsTheConfig(t *testing.T) {
	for toml, want := range map[string]bool{
		botTOML: true,
		botTOML + "[scheduler]\nfeature_proposals = false\n": false,
	} {
		path := setupBotFactory(t, toml)
		b := &backend{g: &globalFlags{config: path}}
		if err := b.load(context.Background()); err != nil {
			t.Fatal(err)
		}
		if b.policy.FeatureProposals != want {
			t.Errorf("%q: backend policy FeatureProposals %v, want %v", strings.TrimPrefix(toml, botTOML), b.policy.FeatureProposals, want)
		}
		if b.policy.Labels.Proposal != "bees:proposal" {
			t.Errorf("policy labels: %+v", b.policy.Labels)
		}
	}
}

// TestBackendReadsReportFactoryErrors: the backend a session's
// report_factory_error goes through answers with scheduler.report_factory_errors,
// off unless a person writes it.
func TestBackendReadsReportFactoryErrors(t *testing.T) {
	for toml, want := range map[string]bool{
		botTOML: false,
		botTOML + "[scheduler]\nreport_factory_errors = true\n": true,
	} {
		path := setupBotFactory(t, toml)
		b := &backend{g: &globalFlags{config: path}}
		got, err := b.ReportFactoryErrors(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("%q: ReportFactoryErrors %v, want %v", strings.TrimPrefix(toml, botTOML), got, want)
		}
	}
}

// TestBackendNotesAreTheStateDirsFiles: on the default backend, the backend
// a session's notes_read and notes_write go through reads and replaces the
// role's notes file under $BEES_STATE_DIR — the file `bees notes show`
// prints — and needs no bees.toml for it: here there is none anywhere, as
// for `bees mcp serve` run by hand outside a factory.
func TestBackendNotesAreTheStateDirsFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(session.EnvStateDir, dir)
	t.Chdir(t.TempDir())
	b := &backend{g: &globalFlags{}}
	ctx := context.Background()
	got, err := b.ReadNotes(ctx, config.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	if got != state.NotesSkeleton(config.RoleReviewer) {
		t.Errorf("first read = %q, want the skeleton", got)
	}
	if err := b.WriteNotes(ctx, config.RoleReviewer, "# reviewer notes\n\n- always run the e2e suite\n"); err != nil {
		t.Fatal(err)
	}
	onDisk, err := state.New(dir).ReadNotes(config.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	if onDisk != "# reviewer notes\n\n- always run the e2e suite\n" {
		t.Errorf("notes file = %q", onDisk)
	}
	if got, err = b.ReadNotes(ctx, config.RoleReviewer); err != nil || got != onDisk {
		t.Errorf("read back %q, %v", got, err)
	}
}

// fakeNeo4jNotes starts a fake Neo4j Agent Memory service that answers one
// reviewer conversation holding content, counting each message read, and
// refusing any request without wantKey as its bearer token. The real
// service is never reached from a test: this is the boundary the neo4j
// notes tests stop at, so what they leave unverified is nams's own request
// shapes and error responses, covered by internal/nams instead.
func fakeNeo4jNotes(t *testing.T, wantKey, content string) (url string, reads *int) {
	t.Helper()
	reads = new(int)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+wantKey {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/conversations":
			_, _ = fmt.Fprint(w, `{"conversations":[{"id":"c1","userId":"bees-notes-reviewer","createdAt":"2026-09-09T00:00:00Z"}]}`)
		case "/v1/conversations/c1/messages":
			*reads++
			_, _ = fmt.Fprintf(w, `{"messages":[{"id":"m1","role":"assistant","content":%q,"createdAt":"2026-09-09T00:01:00Z"}]}`, content)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/v1", reads
}

// writeSessionConfig writes toml to a fresh bees.toml and points
// $BEES_CONFIG at it, the way a session's environment names its config.
func writeSessionConfig(t *testing.T, toml string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bees.toml")
	if err := os.WriteFile(p, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(session.EnvConfig, p)
	return p
}

// TestNotesBackendReadsFromNeo4jWhenConfigured: with notes.backend =
// "neo4j", notes_read and the size behind it go through the Neo4j Agent
// Memory REST API at notes.neo4j_url with the configured key, and the
// state directory's notes files are never touched. Only the [notes] table
// needs to load: a [github] whose "$VAR" the session's environment lacks
// — the shape a session's environment takes — still leaves it working.
func TestNotesBackendReadsFromNeo4jWhenConfigured(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(session.EnvStateDir, stateDir)
	t.Setenv("BEES_TEST_TOKEN", "")
	url, reads := fakeNeo4jNotes(t, "nams_test", "# reviewer notes\n\n- from neo4j\n")
	path := writeSessionConfig(t, botTOML+"[notes]\nbackend = \"neo4j\"\nneo4j_url = \""+url+"\"\nneo4j_api_key = \"nams_test\"\n")
	if _, err := config.Load(path); err == nil {
		t.Fatal("the fixture loads whole: the [github] half of this test proves nothing")
	}
	ctx := context.Background()
	b := &backend{g: &globalFlags{}}
	got, err := b.ReadNotes(ctx, config.RoleReviewer)
	if err != nil {
		t.Fatalf("neo4j read: %v", err)
	}
	if got != "# reviewer notes\n\n- from neo4j\n" || *reads != 1 {
		t.Errorf("neo4j read = %q after %d service reads", got, *reads)
	}
	if entries, _ := os.ReadDir(filepath.Join(stateDir, "notes")); len(entries) != 0 {
		t.Errorf("the neo4j backend wrote %d notes files", len(entries))
	}
	// A size is measured through the same backend: what the service holds,
	// not a notes file it never wrote.
	if n, err := b.Size(ctx, config.RoleReviewer); err != nil || n != int64(len("# reviewer notes\n\n- from neo4j\n")) {
		t.Errorf("neo4j size = %d, %v; want the length of the notes the service holds", n, err)
	}
}

// TestNotesBackendIsPickedOnceAtConstruction: the Notes backend behind one
// backend value is chosen the first time it is used and does not move
// under it, so a person editing bees.toml mid-session cannot redirect a
// running server's notes tools.
func TestNotesBackendIsPickedOnceAtConstruction(t *testing.T) {
	t.Setenv(session.EnvStateDir, t.TempDir())
	t.Setenv("BEES_TEST_TOKEN", "")
	url, _ := fakeNeo4jNotes(t, "nams_test", "# reviewer notes\n\n- from neo4j\n")
	writeSessionConfig(t, botTOML+"[notes]\nbackend = \"neo4j\"\nneo4j_url = \""+url+"\"\nneo4j_api_key = \"nams_test\"\n")
	ctx := context.Background()
	b := &backend{g: &globalFlags{}}
	if _, err := b.ReadNotes(ctx, config.RoleReviewer); err != nil {
		t.Fatalf("first read picks the backend: %v", err)
	}
	writeSessionConfig(t, botTOML+"[notes]\nbackend = \"file\"\n")
	got, err := b.ReadNotes(ctx, config.RoleReviewer)
	if err != nil || got != "# reviewer notes\n\n- from neo4j\n" {
		t.Errorf("the backend moved with bees.toml: %q, %v", got, err)
	}
}

// TestNotesBackendSelectsFileExplicitly: notes.backend = "file" in a loaded
// bees.toml reads and replaces the same state-dir file the default (no
// [notes] table at all, covered by TestBackendNotesAreTheStateDirsFiles)
// does: the first read is the skeleton, a write lands on disk, and the
// size is the file's size.
func TestNotesBackendSelectsFileExplicitly(t *testing.T) {
	stateDir := t.TempDir()
	t.Setenv(session.EnvStateDir, stateDir)
	writeSessionConfig(t, botTOML+"[notes]\nbackend = \"file\"\n")
	ctx := context.Background()
	b := &backend{g: &globalFlags{}}
	got, err := b.ReadNotes(ctx, config.RoleReviewer)
	if err != nil || got != state.NotesSkeleton(config.RoleReviewer) {
		t.Fatalf("file read = %q, %v; want the skeleton", got, err)
	}
	if err := b.WriteNotes(ctx, config.RoleReviewer, "# on disk\n"); err != nil {
		t.Fatal(err)
	}
	if onDisk, _ := state.New(stateDir).ReadNotes(config.RoleReviewer); onDisk != "# on disk\n" {
		t.Errorf("file write left %q", onDisk)
	}
	if n, err := b.Size(ctx, config.RoleReviewer); err != nil || n != int64(len("# on disk\n")) {
		t.Errorf("file size = %d, %v; want the file's %d", n, err, len("# on disk\n"))
	}
}

// TestNotesBackendWiringSurfacesAnIncompleteNeo4jConfig: a [notes] table
// naming "neo4j" without neo4j_api_key fails notes_read through the same
// error config.LoadNotes already names the key with, rather than the
// backend wiring swallowing or reshaping it.
func TestNotesBackendWiringSurfacesAnIncompleteNeo4jConfig(t *testing.T) {
	t.Setenv(session.EnvStateDir, t.TempDir())
	writeSessionConfig(t, botTOML+"[notes]\nbackend = \"neo4j\"\nneo4j_url = \"https://nams.example.com/v1\"\n")
	if _, err := (&backend{g: &globalFlags{}}).ReadNotes(context.Background(), config.RoleReviewer); err == nil || !strings.Contains(err.Error(), "needs notes.neo4j_api_key") {
		t.Errorf("incomplete [notes]: %v, want an error naming notes.neo4j_api_key", err)
	}
}
