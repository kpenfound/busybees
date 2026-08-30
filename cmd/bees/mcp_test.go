package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/mcpserver"
)

// TestToolsTextDiffersPerRole is the point of `bees mcp tools`: the tool names
// are the same for everybody, the enums are not.
func TestToolsTextDiffersPerRole(t *testing.T) {
	for _, tc := range []struct {
		role  string
		want  string
		tools []string
	}{
		{role: config.RoleDeveloper, want: "    status: pr-opened | pr-updated | question | failed\n",
			tools: []string{"issue_view", "pr_view", "comment"}},
		{role: config.RoleReviewer, want: "    status: approved | changes-requested | failed\n"},
		{role: config.RoleQA, want: "    status: done | failed\n"},
		{role: config.RoleProductManager, want: "    status: done | idle | failed\n",
			tools: []string{"issue_edit_body", "issue_question"}},
		{role: config.RoleProjectManager, want: "    status: done | idle | failed\n",
			tools: []string{"issue_edit_body", "issue_set_state"}},
	} {
		t.Run(tc.role, func(t *testing.T) {
			out := renderTools(t, tc.role)
			if !strings.Contains(out, tc.want) {
				t.Fatalf("tools for %s =\n%s\nwant a line %q", tc.role, out, tc.want)
			}
			// Every role can write to every role, and the tool set is the same.
			if !strings.Contains(out, "    to: "+strings.Join(config.Roles, " | ")+"\n") {
				t.Fatalf("tools for %s =\n%s\nwant the mail_send recipients", tc.role, out)
			}
			for _, name := range append([]string{"done", "issue_create", "issue_link", "mail_list", "mail_send"}, tc.tools...) {
				if !strings.Contains(out, "mcp__"+config.BuiltinMCPServer+"__"+name) {
					t.Fatalf("tools for %s =\n%s\nwant %s", tc.role, out, name)
				}
			}
		})
	}
}

// Without a role the outcomes are unconstrained, so done has no enum to print.
// The project manager's state moves are the enums that differ most: they
// carry the short state and size names, not label names.
func TestToolsTextShowsTheStateAndSizeEnums(t *testing.T) {
	out := renderTools(t, config.RoleProjectManager)
	for _, want := range []string{"    size: xs | s | m | l | xl\n", "    state: ready | blocked\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("tools for the project manager =\n%s\nwant a line %q", out, want)
		}
	}
}

func TestToolsTextWithoutARole(t *testing.T) {
	out := renderTools(t, "")
	if strings.Contains(out, "    status: ") {
		t.Fatalf("tools without a role =\n%s\nwant no status enum", out)
	}
	if !strings.Contains(out, "mcp__"+config.BuiltinMCPServer+"__done") {
		t.Fatalf("tools without a role =\n%s\nwant the done tool", out)
	}
}

func renderTools(t *testing.T, role string) string {
	t.Helper()
	list, err := mcpserver.Tools(context.Background(), mcpserver.Env{Role: role})
	if err != nil {
		t.Fatal(err)
	}
	return toolsText(list)
}

// TestIsCleanShutdown covers how `bees mcp serve` ends. claude closes the
// server's stdin when it is done with it, which the SDK reports as a
// jsonrpc2 "server is closing" error; exiting nonzero there would make every
// session record its bees server as having crashed.
func TestIsCleanShutdown(t *testing.T) {
	closing := &jsonrpc.Error{Code: codeServerClosing, Message: "server is closing"}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, true},
		{"eof", io.EOF, true},
		{"cancelled", context.Canceled, true},
		{"closing", closing, true},
		{"closing wrapping the read error", fmt.Errorf("%w: %v", closing, io.EOF), true},
		{"wrapped further", fmt.Errorf("run: %w", closing), true},
		{"another wire error", &jsonrpc.Error{Code: jsonrpc.CodeInternalError}, false},
		{"a real failure", errors.New("broken pipe"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCleanShutdown(tc.err); got != tc.want {
				t.Fatalf("isCleanShutdown(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
