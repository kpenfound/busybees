package mcpserver

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/feedback"
)

// Feedback is the backend behind report_factory_error's gate: whether
// scheduler.report_factory_errors is on. It is an interface for the same
// reason Issues is — the server is built before bees.toml is read — and a
// nil one makes the tool report that it is unavailable, like the issue and
// GitHub tools do.
type Feedback interface {
	ReportFactoryErrors(ctx context.Context) (bool, error)
}

type reportFactoryErrorInput struct {
	Title  string `json:"title" jsonschema:"one line naming the factory error, with no repository name, path, token or person in it"`
	Detail string `json:"detail" jsonschema:"the error and enough context to act on it: what you did, what the tool or orchestrator did instead, what you expected; already scrubbed of repository names, tokens, file paths and people"`
}

func (s *server) addFeedbackTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "report_factory_error",
		Title: "Report an error the factory caused",
		Description: "Record a draft issue about an error caused by the factory itself, not by the " +
			"product it is building: a tool that behaved unexpectedly, a prompt that contradicted " +
			"the code, an orchestrator label you had to work around. The draft is queued locally " +
			"for filing against the busybees project; nothing is posted to GitHub and nothing is " +
			"checked for duplicates here. Scrub before you call: the title and detail must name no " +
			"repository, no organisation, no file path, no token or secret and no person — replace " +
			"them with placeholders like <repo>, <path> and <login>. Nothing scrubs after you. " +
			"Without scheduler.report_factory_errors in bees.toml the draft is not recorded.",
		InputSchema: schemaFor[reportFactoryErrorInput](nil),
	}, s.reportFactoryError)
}

func (s *server) reportFactoryError(ctx context.Context, _ *mcp.CallToolRequest, in reportFactoryErrorInput) (*mcp.CallToolResult, any, error) {
	if s.feedback == nil {
		return nil, nil, errors.New("factory-error reports are unavailable: bees.toml could not be loaded")
	}
	if s.drafts == nil {
		return nil, nil, errors.New("no draft queue: $BEES_STATE_DIR is not set")
	}
	if s.env.Role == "" {
		return nil, nil, errors.New("a factory error can only be reported from a session ($BEES_ROLE is not set)")
	}
	on, err := s.feedback.ReportFactoryErrors(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !on {
		return text("not recorded: scheduler.report_factory_errors is off in bees.toml, so the factory does not collect error reports. Put the problem in your outcome note instead."), nil, nil
	}
	d, err := s.drafts.Add(feedback.Draft{
		Role: s.env.Role, SessionDir: s.env.SessionDir, Title: in.Title, Detail: in.Detail,
	})
	if err != nil {
		return nil, nil, err
	}
	return text("recorded draft %s; it will be filed against the busybees project", d.ID), nil, nil
}
