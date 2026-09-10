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

// TestBackendNotesFollowNotesBackend: the backend behind the notes tools is
// picked from [notes] in the session's bees.toml ($BEES_CONFIG), once. With
// "neo4j" a read goes to the Neo4j Agent Memory REST API at notes.neo4j_url
// with the configured key and never touches the state directory; with
// "file" it is the state directory's file. Only the [notes] table is read:
// a [github] whose "$VAR" the session's environment lacks — the shape a
// session's environment takes — leaves the notes tools working, while a
// [notes] that is incomplete fails the tool call naming the key.
func TestBackendNotesFollowNotesBackend(t *testing.T) {
	var reads int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer nams_test" {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/conversations":
			_, _ = fmt.Fprint(w, `{"conversations":[{"id":"c1","userId":"bees-notes-reviewer","createdAt":"2026-09-09T00:00:00Z"}]}`)
		case "/v1/conversations/c1/messages":
			reads++
			_, _ = fmt.Fprint(w, `{"messages":[{"id":"m1","role":"assistant","content":"# reviewer notes\n\n- from neo4j\n","createdAt":"2026-09-09T00:01:00Z"}]}`)
		default:
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	stateDir := t.TempDir()
	t.Setenv(session.EnvStateDir, stateDir)
	// botTOML's github.token reads $BEES_TEST_TOKEN; unset, the file does
	// not load whole.
	t.Setenv("BEES_TEST_TOKEN", "")
	neo4j := botTOML + "[notes]\nbackend = \"neo4j\"\nneo4j_url = \"" + srv.URL + "/v1\"\nneo4j_api_key = \"nams_test\"\n"
	ctx := context.Background()

	write := func(toml string) {
		t.Helper()
		p := filepath.Join(t.TempDir(), "bees.toml")
		if err := os.WriteFile(p, []byte(toml), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv(session.EnvConfig, p)
	}

	write(neo4j)
	if _, err := config.Load(os.Getenv(session.EnvConfig)); err == nil {
		t.Fatal("the fixture loads whole: the [github] half of this test proves nothing")
	}
	b := &backend{g: &globalFlags{}}
	got, err := b.ReadNotes(ctx, config.RoleReviewer)
	if err != nil {
		t.Fatalf("neo4j read: %v", err)
	}
	if got != "# reviewer notes\n\n- from neo4j\n" || reads != 1 {
		t.Errorf("neo4j read = %q after %d service reads", got, reads)
	}
	if entries, _ := os.ReadDir(filepath.Join(stateDir, "notes")); len(entries) != 0 {
		t.Errorf("the neo4j backend wrote %d notes files", len(entries))
	}
	// A size is measured through the same backend: what the service holds,
	// not a notes file it never wrote.
	if n, err := b.Size(ctx, config.RoleReviewer); err != nil || n != int64(len("# reviewer notes\n\n- from neo4j\n")) {
		t.Errorf("neo4j size = %d, %v; want the length of the notes the service holds", n, err)
	}
	// Picked once: a later edit of bees.toml does not move a running server.
	write(botTOML + "[notes]\nbackend = \"file\"\n")
	if got, _ = b.ReadNotes(ctx, config.RoleReviewer); got != "# reviewer notes\n\n- from neo4j\n" {
		t.Errorf("the backend moved with bees.toml: %q", got)
	}

	b = &backend{g: &globalFlags{}}
	if got, err = b.ReadNotes(ctx, config.RoleReviewer); err != nil || got != state.NotesSkeleton(config.RoleReviewer) {
		t.Errorf("file read = %q, %v; want the skeleton", got, err)
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

	write(botTOML + "[notes]\nbackend = \"neo4j\"\nneo4j_url = \"" + srv.URL + "/v1\"\n")
	b = &backend{g: &globalFlags{}}
	if _, err := b.ReadNotes(ctx, config.RoleReviewer); err == nil || !strings.Contains(err.Error(), "needs notes.neo4j_api_key") {
		t.Errorf("incomplete [notes]: %v", err)
	}
}
