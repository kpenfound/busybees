package main

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/mcpserver"
)

// TestToolSetIsScopedByRole pins the exact set of tools each role is
// offered: a role argues with nothing it cannot even see. The expected sets
// are literal, so a change that silently widens or narrows a role's tools
// (for example by dropping the role arguments on an AddTool call) fails
// here. It does not cover tool descriptions or schema ordering, which are
// presentation, not a role boundary; TestReadOnlyToolsAreAnnotatedReadOnly
// covers the one annotation a client acts on.
func TestToolSetIsScopedByRole(t *testing.T) {
	base := []string{"comment", "done", "issue_create", "issue_link", "issue_view", "mail_list", "mail_send", "notes_read", "notes_write", "pr_view", "report_factory_error"}
	// An unknown or empty role is hand use through `bees mcp serve`: the
	// full tool set, unrestricted.
	full := []string{"comment", "done", "file_bug", "issue_create", "issue_edit_body", "issue_link", "issue_question", "issue_set_state", "issue_view", "mail_list", "mail_send", "notes_read", "notes_write", "pr_view", "release_ship", "report_factory_error", "submit_review"}
	for role, want := range map[string][]string{
		config.RoleDeveloper:      base,
		config.RoleReviewer:       append(append([]string{}, base...), "submit_review"),
		config.RoleQA:             append(append([]string{}, base...), "file_bug"),
		config.RoleProjectManager: append(append([]string{}, base...), "issue_edit_body", "issue_set_state"),
		config.RoleProductManager: append(append([]string{}, base...), "issue_edit_body", "issue_question"),
		config.RoleReleaseManager: append(append([]string{}, base...), "release_ship"),
		"":                        full,
		"unknown":                 full,
	} {
		list, err := mcpserver.Tools(context.Background(), mcpserver.Env{Role: role})
		if err != nil {
			t.Fatalf("role %q: %v", role, err)
		}
		got := toolNames(list)
		wantSorted := sortedCopy(want)
		if strings.Join(got, ",") != strings.Join(wantSorted, ",") {
			t.Errorf("role %q tool set =\n%s\nwant\n%s", role, strings.Join(got, ", "), strings.Join(wantSorted, ", "))
		}
	}
}

// TestDoneStatusEnumMatchesRoleOutcomes pins the done tool's status enum per
// role, the outcomes session.Report will accept from that role. The
// expected lists are the literal values a session sees, not derived by
// calling the validation they protect. An unknown or empty role gets no
// enum at all, matching the unrestricted outcome a hand-run `bees mcp
// serve` accepts.
func TestDoneStatusEnumMatchesRoleOutcomes(t *testing.T) {
	for role, want := range map[string][]string{
		config.RoleDeveloper:      {"pr-opened", "pr-updated", "question", "failed"},
		config.RoleReviewer:       {"approved", "changes-requested", "failed"},
		config.RoleQA:             {"done", "failed"},
		config.RoleProductManager: {"done", "idle", "failed"},
		config.RoleProjectManager: {"done", "idle", "failed"},
		config.RoleReleaseManager: {"done", "failed"},
		"":                        nil,
		"unknown":                 nil,
	} {
		list, err := mcpserver.Tools(context.Background(), mcpserver.Env{Role: role})
		if err != nil {
			t.Fatalf("role %q: %v", role, err)
		}
		got := doneStatusEnum(t, role, list)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("role %q done status enum = %v, want %v", role, got, want)
		}
	}
}

// TestReadOnlyToolsAreAnnotatedReadOnly pins which tools carry the
// ReadOnlyHint annotation, the one a client can act on (for example to skip
// a confirmation it would ask before a mutating call). The literal set
// comes from reading the tool definitions, not from running them.
func TestReadOnlyToolsAreAnnotatedReadOnly(t *testing.T) {
	wantReadOnly := []string{"issue_view", "pr_view", "mail_list", "notes_read"}
	list, err := mcpserver.Tools(context.Background(), mcpserver.Env{Role: ""})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tl := range list {
		if tl.Annotations != nil && tl.Annotations.ReadOnlyHint {
			got = append(got, tl.Name)
		}
	}
	sort.Strings(got)
	wantSorted := sortedCopy(wantReadOnly)
	if strings.Join(got, ",") != strings.Join(wantSorted, ",") {
		t.Errorf("tools annotated read-only = %v, want %v", got, wantSorted)
	}
}

func toolNames(tools []*mcp.Tool) []string {
	names := make([]string, 0, len(tools))
	for _, tl := range tools {
		names = append(names, tl.Name)
	}
	sort.Strings(names)
	return names
}

func sortedCopy(s []string) []string {
	out := append([]string{}, s...)
	sort.Strings(out)
	return out
}

// doneStatusEnum reads the done tool's status enum out of the schema a
// session actually sees, the same JSON a client reads over the wire.
func doneStatusEnum(t *testing.T, role string, tools []*mcp.Tool) []string {
	t.Helper()
	for _, tl := range tools {
		if tl.Name != "done" {
			continue
		}
		b, err := json.Marshal(tl.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties struct {
				Status struct {
					Enum []string `json:"enum"`
				} `json:"status"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatal(err)
		}
		return schema.Properties.Status.Enum
	}
	t.Fatalf("role %q: no done tool", role)
	return nil
}
