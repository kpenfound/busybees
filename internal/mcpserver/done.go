package mcpserver

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/session"
)

type doneInput struct {
	Status string `json:"status" jsonschema:"the outcome to report"`
	Note   string `json:"note,omitempty" jsonschema:"one short line for the orchestrator and the logs"`
	PR     int    `json:"pr,omitempty" jsonschema:"pull request the outcome is about (defaults to this session's PR); required for pr-opened and pr-updated"`
	Issue  int    `json:"issue,omitempty" jsonschema:"issue the outcome is about (defaults to this session's issue)"`
}

func (s *server) addDoneTool(srv *mcp.Server) {
	valid := session.ValidOutcomes(s.env.Role)
	desc := "Report the outcome of this session. This is the last thing you do: the " +
		"orchestrator uses the outcome to decide what happens next, and a session that " +
		"ends without one is treated as failed."
	if len(valid) > 0 {
		desc += " Valid statuses for this role: " + strings.Join(valid, ", ") + "."
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "done",
		Title:       "Report the session outcome",
		Description: desc,
		InputSchema: schemaFor[doneInput](map[string][]string{"status": valid}),
	}, s.done)
}

func (s *server) done(ctx context.Context, _ *mcp.CallToolRequest, in doneInput) (*mcp.CallToolResult, any, error) {
	if in.PR == 0 {
		in.PR = s.env.PR
	}
	if in.Issue == 0 {
		in.Issue = s.env.Issue
	}
	o, err := session.Report(s.env.SessionDir, s.env.Role, session.Outcome{
		Status: in.Status, Note: in.Note, PR: in.PR, Issue: in.Issue,
	})
	if err != nil {
		return nil, nil, err
	}
	return text("outcome recorded: %s", o.Status), nil, nil
}
