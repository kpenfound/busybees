package mcpserver

import (
	"context"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/mcphost"
	"github.com/kpenfound/busybees/core/work"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

type doneInput struct {
	Status string `json:"status" jsonschema:"the outcome to report"`
	Note   string `json:"note,omitempty" jsonschema:"one short line for the orchestrator and the logs"`
	PR     int    `json:"pr,omitempty" jsonschema:"pull request the outcome is about (defaults to this session's PR); required for pr-opened and pr-updated"`
	Issue  int    `json:"issue,omitempty" jsonschema:"issue the outcome is about (defaults to this session's issue)"`
}

func (s *server) addDoneTool(srv *mcphost.Registry) {
	desc := "Report the outcome of this session. This is the last thing you do: the " +
		"orchestrator uses the outcome to decide what happens next, and a session that " +
		"ends without one is treated as failed."

	mcphost.AddDoneTool(srv, mcphost.DoneOptions{
		Title: "Report the session outcome", Description: desc,
		Statuses: session.ValidOutcomes(s.env.Role), Work: ghwork.New(s.env.Issue, s.env.PR),
		Report: func(_ context.Context, o agent.Outcome) (agent.Outcome, error) {
			if s.env.StateDir != "" {
				if err := state.New(s.env.StateDir).Migrate(); err != nil {
					return o, err
				}
			}
			return session.Report(s.env.SessionDir, s.env.Role, o)
		},
	}, func(in doneInput, ref work.Ref) agent.Outcome {
		if in.PR == 0 {
			in.PR = ghwork.PR(ref)
		}
		if in.Issue == 0 {
			in.Issue = ghwork.Issue(ref)
		}
		return agent.Outcome{Status: in.Status, Note: in.Note, Work: ghwork.New(in.Issue, in.PR)}
	})
}
