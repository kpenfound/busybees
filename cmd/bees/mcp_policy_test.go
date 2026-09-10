package main

import (
	"context"
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

// TestBackendNotesAreTheStateDirsFiles: the backend a session's notes_read
// and notes_write go through reads and replaces the role's notes file under
// $BEES_STATE_DIR — the file `bees notes show` prints — without loading
// bees.toml, which a session does not need for its own memory.
func TestBackendNotesAreTheStateDirsFiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(session.EnvStateDir, dir)
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
