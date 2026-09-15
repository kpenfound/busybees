package review

import "context"

// AgentRequest is one review session.
type AgentRequest struct {
	// Name identifies the session in an error message.
	Name string
	// Prompt is the whole of what the session is told: a review session has
	// no separate system prompt, because codex takes none.
	Prompt string
	// Dir is the directory the session runs in, which is what its
	// read-only tools can reach. It must be a directory the review is
	// willing to have read: the checkout of the repository under review, or
	// an empty one.
	Dir string
	// ResumeID continues an earlier session of this agent (AgentResult.ID)
	// instead of starting one, for the triage action that asks an angle a
	// follow-up question. Only claude can; codex has no resume and ignores
	// it.
	ResumeID string
}

// AgentResult is what a finished review session produced.
type AgentResult struct {
	// ID is the agent's own id for the session, which resumes it later.
	ID string
	// Text is the session's last message, which is what it was asked for.
	Text string
	// Turns is how many turns it took, and CostUSD what it cost when the
	// CLI reported one: codex reports none, so its cost is zero rather
	// than known.
	Turns   int
	CostUSD float64
}

// Agent runs one review session. It is the seam every session in the
// pipeline goes through: the distiller here, the angle sessions after it.
type Agent interface {
	Run(ctx context.Context, req AgentRequest) (*AgentResult, error)
}
