package review

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	core "github.com/kpenfound/busybees/core/review"
	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/internal/config"
)

type AgentRequest = core.AgentRequest
type AgentResult = core.AgentResult
type Agent = core.Agent

// CLIAgent runs a review session as the coding agent the configuration
// selects, through the shared restricted execution every busybees
// read-only session goes through (core/agent.RunRestricted): one command
// builder and one output reader per backend, all four of claude, codex,
// opencode and pi, each held to the same read-only floor the descriptor
// declares. It is the busybees half of that execution — the review
// configuration (provider, model, effort, fallback chain, environment,
// bounds) mapped onto the shared request — and deliberately not the
// factory's session runner (internal/session): that one gives every
// session the built-in bees MCP server and the BEES_* environment of the
// factory it belongs to, and `bees review` reviews any pull request on
// GitHub without one.
type CLIAgent struct {
	// Provider is the agent to run, one of SupportedProviders, and Model
	// the model it runs. An empty provider is claude; an empty model leaves
	// the choice to the agent.
	Provider string
	Model    string
	// Fallback is the agent the session runs as instead when this one has
	// no capacity: the agent answered, or died saying on stderr, that the
	// model is rate limited or overloaded. The shared execution walks the
	// chain, holds every profile of it to the same read-only floor, starts
	// a new session when it crosses to another agent, and reports the
	// agent and model that answered. When two profiles of the chain run
	// claude, the first is also given the next claude model as
	// --fallback-model, so claude switches within the session. Nil is no
	// fallback.
	Fallback *CLIAgent
	// Effort maps to each backend's reasoning setting: claude's --effort,
	// codex's model_reasoning_effort (its levels stop at high, so "max"
	// goes as "high"), opencode's agent variant and pi's --thinking.
	Effort string
	// ClaudeBin, CodexBin, OpenCodeBin and PiBin are the executables, the
	// backend's own name when empty.
	ClaudeBin   string
	CodexBin    string
	OpenCodeBin string
	PiBin       string
	// Timeout and MaxTurns bound the session. Zero takes the shared
	// restricted defaults.
	Timeout  time.Duration
	MaxTurns int
	// Env are variables set for the session on top of the environment the
	// caller runs in, a role's own from bees.toml when the factory runs the
	// review. Nothing for `bees review`, whose sessions run as the person.
	// The shared execution drops every BEES_* and VCS variable from both,
	// so a review never inherits factory identity or Git access overrides,
	// and expands $VAR references in these values the way it does for a
	// factory session.
	Env map[string]string
}

// NewAgent is the agent a review's sessions run as, as the global
// configuration says.
func NewAgent(cfg *Config) *CLIAgent {
	agent := &CLIAgent{Provider: DefaultProvider, Model: DefaultModel}
	if cfg != nil {
		agent.Provider, agent.Model = cfg.Provider, cfg.Model
	}
	return agent
}

// Run runs the session and returns what it said last, named for the agent
// that answered. An error is a session that could not be started, one that
// failed, and one that ended without saying anything: none of those is a
// review, and the caller has nothing to go on either way.
func (a *CLIAgent) Run(ctx context.Context, req AgentRequest) (*AgentResult, error) {
	if err := a.validate(); err != nil {
		return nil, fmt.Errorf("%s session: %w", req.Name, err)
	}
	res, err := a.runner().RunRestricted(ctx, agent.Request{
		Name:      req.Name,
		Profile:   a.profile(),
		Workspace: vcs.Directory(req.Dir),
		Prompt:    req.Prompt,
		ResumeID:  req.ResumeID,
	})
	if err != nil {
		return nil, err
	}
	return &AgentResult{
		ID:        res.ClaudeID,
		Text:      res.ResultText,
		Turns:     res.NumTurns,
		CostUSD:   res.CostUSD,
		CostKnown: res.CostKnown,
		Provider:  res.Agent,
		Model:     res.Model,
	}, nil
}

// runner is the shared execution the session runs through, with the
// executables of this agent and of every fallback behind it: a fallback may
// run as another agent, whose executable only it names. Its own record of
// the session — the transcript, the prompt and the result — goes under the
// machine's temporary directory; a review session is not a factory session,
// and its runner logs nothing.
func (a *CLIAgent) runner() *agent.Runner {
	r := &agent.Runner{Logger: slog.New(slog.DiscardHandler)}
	for at := a; at != nil; at = at.Fallback {
		if r.ClaudeBin == "" {
			r.ClaudeBin = at.ClaudeBin
		}
		if r.CodexBin == "" {
			r.CodexBin = at.CodexBin
		}
		if r.OpenCodeBin == "" {
			r.OpenCodeBin = at.OpenCodeBin
		}
		if r.PiBin == "" {
			r.PiBin = at.PiBin
		}
	}
	return r
}

// validate refuses a configuration no backend can carry out, before any
// process starts: a provider no backend is named by, and an agent whose
// descriptor does not declare the restricted read-only floor, in this
// agent or in any profile of its fallback chain.
func (a *CLIAgent) validate() error {
	seen := map[*CLIAgent]bool{}
	for at := a; at != nil; at = at.Fallback {
		if seen[at] {
			return errors.New("review: the fallback chain is a cycle")
		}
		seen[at] = true
		name := at.provider()
		b, ok := backendByName(name)
		if !ok {
			return fmt.Errorf("review: unknown provider %s (want one of %s)", strconv.Quote(name), strings.Join(SupportedProviders, ", "))
		}
		if b.Restricted == nil || !b.Restricted.Supported {
			return fmt.Errorf("review: agent %q cannot establish the read-only restriction (want one of %s)", name, strings.Join(SupportedProviders, ", "))
		}
	}
	return nil
}

// profile is the execution profile of this agent and its fallback chain,
// for the shared restricted request. Only execution settings cross: the
// boundary is RunRestricted's own, whatever a profile of the configuration
// said about a sandbox.
func (a *CLIAgent) profile() agent.Profile {
	p := agent.Profile{
		Name:     "review",
		Agent:    a.provider(),
		Model:    a.Model,
		Effort:   a.Effort,
		Timeout:  a.Timeout,
		MaxTurns: a.MaxTurns,
		Env:      maps.Clone(a.Env),
	}
	if a.Fallback != nil {
		f := a.Fallback.profile()
		p.Fallback = &f
	}
	return p
}

// provider is the agent this profile runs, claude when none was named.
func (a *CLIAgent) provider() string {
	if a.Provider == "" {
		return config.AgentClaude
	}
	return a.Provider
}

// backendByName looks a backend up in the shared descriptors, which are
// what SupportedProviders and the resume support below are read from.
func backendByName(name string) (agent.Backend, bool) {
	for _, b := range agent.Backends {
		if b.Name == name {
			return b, true
		}
	}
	return agent.Backend{}, false
}

// supportedProviders lists the backends whose descriptor declares the
// restricted read-only floor: the agents a review session can run as.
func supportedProviders() []string {
	var out []string
	for _, b := range agent.Backends {
		if b.Restricted != nil && b.Restricted.Supported {
			out = append(out, b.Name)
		}
	}
	return out
}

// followUpSupported says whether a backend can reopen a session it owns,
// from its capability declaration: the triage action that asks an angle a
// question runs on it.
func followUpSupported(name string) bool {
	b, ok := backendByName(name)
	return ok && b.Restricted != nil && b.Restricted.FollowUp
}
