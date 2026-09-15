package agent

import (
	"context"
	"fmt"
	"slices"
	"time"
)

const (
	AgentClaude      = "claude"
	AgentCodex       = "codex"
	AgentOpenCode    = "opencode"
	SandboxNone      = "none"
	SandboxClaude    = "claude"
	SandboxContainer = "container"
	ContainerEngine  = "docker"
	DefaultTimeout   = 45 * time.Minute
)

// AgentCredentials names credentials forwarded into an isolated container.
var AgentCredentials = map[string][]string{
	AgentClaude: {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
	AgentCodex:  {"OPENAI_API_KEY", "CODEX_API_KEY"},
}

// Profile contains only agent execution settings. Workflow and identity policy
// belong to the caller. Skills are generic plugin sources, prepared by Skills.
type Profile struct {
	Name            string
	Agent           string
	Model           string
	FallbackModel   string
	Effort          string
	MaxTurns        int
	Timeout         time.Duration
	AllowedTools    []string
	DisallowedTools []string
	// MCP contains prepared entries; MCPEntries expands configured references once.
	MCP                     map[string]MCPEntry
	Sandbox                 string
	SandboxImage            string
	SandboxDomains          []string
	ContainerUseEnvironment string
	// VCSAccess permits the runner to expose a worktree's shared git directory.
	// It is not a security boundary on an unsandboxed host.
	VCSAccess bool
	Shell     string
	Env       map[string]string
	Skills    []string
}

// Validate rejects modes that would silently run without the requested box.
func (p Profile) Validate() error {
	if p.Sandbox != "" && !slices.Contains([]string{SandboxNone, SandboxClaude, SandboxContainer}, p.Sandbox) {
		return fmt.Errorf("sandbox %q is not implemented (agent can run none, claude, container)", p.Sandbox)
	}
	if p.Sandbox == SandboxClaude && p.Agent != "" && p.Agent != AgentClaude {
		return fmt.Errorf("sandbox %q is Claude Code's sandbox and agent %q does not run under it", p.Sandbox, p.Agent)
	}
	if p.Sandbox == SandboxContainer && p.SandboxImage == "" && p.ContainerUseEnvironment == "" {
		return fmt.Errorf("sandbox %q needs sandbox_image or container_use_environment", p.Sandbox)
	}
	return nil
}

// HostMCP describes a caller-owned server that can serve HTTP from the host.
// The runner appends ListenArgs and the listen address to Entry.Args, puts a
// fresh bearer token in TokenEnv, and reads ListeningPrefix + address on stdout.
// Entry is also supplied in Profile.MCP for ordinary host sessions.
type HostMCP struct {
	Name  string
	Entry MCPEntry
	// Env overrides the session environment for the host server (secrets stay in memory).
	Env             map[string]string
	ListenArgs      []string
	TokenEnv        string
	ListeningPrefix string
	Path            string
}

// SkillPreparer supplies prepared plugin directories; acquisition and cache policy belong to the caller.
type SkillPreparer interface {
	Prepare(context.Context, []string) ([]string, error)
}
