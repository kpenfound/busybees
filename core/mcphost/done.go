package mcphost

import (
	"context"
	"strings"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/work"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// DoneInput is the tracker-independent wire format. Callers with an existing
// wire contract can use their own input type with AddDoneTool.
type DoneInput struct {
	Status string `json:"status" jsonschema:"the outcome to report"`
	Note   string `json:"note,omitempty" jsonschema:"one short line describing the outcome"`
}

// DoneOptions supplies workflow policy and default work context. Report must
// validate the outcome and persist/report it; its returned status is acknowledged
// to the client. Statuses constrain the schema (empty means unconstrained).
// Description is the caller's prose; the host appends the valid status list.
type DoneOptions struct {
	Title       string
	Description string
	Statuses    []string
	Work        work.Ref
	Report      func(context.Context, agent.Outcome) (agent.Outcome, error)
}

// AddDone registers done using the standard input shape and default work.
func AddDone(r *Registry, opts DoneOptions, roles ...string) {
	AddDoneTool(r, opts, func(in DoneInput, ref work.Ref) agent.Outcome {
		return agent.Outcome{Status: in.Status, Note: in.Note, Work: ref}
	}, roles...)
}

// AddDoneTool registers done with a caller-defined input shape containing a
// status property. Decode maps that shape onto a generic outcome. Each request
// receives its own copy of the default work tags, including under concurrency.
func AddDoneTool[In any](r *Registry, opts DoneOptions, decode func(In, work.Ref) agent.Outcome, roles ...string) {
	if opts.Report == nil || decode == nil {
		panic("mcphost: done needs an outcome decoder and reporter")
	}
	ref := opts.Work.Clone()
	desc := opts.Description
	if len(opts.Statuses) > 0 {
		desc += " Valid statuses for this role: " + strings.Join(opts.Statuses, ", ") + "."
	}
	AddTool(r, &mcp.Tool{Name: "done", Title: opts.Title, Description: desc,
		InputSchema: SchemaFor[In](map[string][]string{"status": opts.Statuses}),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in In) (*mcp.CallToolResult, any, error) {
		o, err := opts.Report(ctx, decode(in, ref.Clone()))
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "outcome recorded: " + o.Status}}}, nil, nil
	}, roles...)
}
