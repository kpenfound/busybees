package main

import (
	"context"
	"strings"
	"testing"
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
