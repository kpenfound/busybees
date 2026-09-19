package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	AgentClaude      = "claude"
	AgentCodex       = "codex"
	AgentOpenCode    = "opencode"
	SandboxNone      = "none"
	SandboxClaude    = "claude"
	SandboxContainer = "container"
	// SandboxSbx runs the session inside a Docker Sandbox, a microVM the
	// sbx CLI manages (https://docs.docker.com/ai/sandboxes/).
	SandboxSbx      = "sbx"
	ContainerEngine = "docker"
	// SandboxCLI is the command SandboxSbx sessions are run with.
	SandboxCLI     = "sbx"
	DefaultTimeout = 45 * time.Minute
)

// Agents lists the backends the runner builds a command line for.
var Agents = []string{AgentClaude, AgentCodex, AgentOpenCode}

// Sandboxes lists the sandbox kinds the runner implements, weakest first.
var Sandboxes = []string{SandboxNone, SandboxClaude, SandboxContainer, SandboxSbx}

// AgentCredentials names credentials forwarded into an isolated container.
var AgentCredentials = map[string][]string{
	AgentClaude: {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
	AgentCodex:  {"OPENAI_API_KEY", "CODEX_API_KEY"},
}

// Profile contains only agent execution settings. Workflow and identity policy
// belong to the caller. Skills are generic plugin sources, prepared by Skills.
type Profile struct {
	Name  string
	Agent string
	Model string
	// Fallback is the profile a caller runs the session on instead when
	// this one has no capacity (ops.SelectProfile), agent included, and it
	// may have a fallback of its own. The claude backend also passes a
	// claude fallback's model as --fallback-model, so claude switches to it
	// within the session; a fallback on another agent is the caller's to
	// run. Nil is no fallback.
	Fallback        *Profile
	Effort          string
	MaxTurns        int
	Timeout         time.Duration
	AllowedTools    []string
	DisallowedTools []string
	// MCP contains prepared entries; MCPEntries expands configured references once.
	MCP map[string]MCPEntry
	// Sandbox is one of Sandboxes; empty is SandboxNone. SandboxSbx runs
	// the claude agent only.
	Sandbox string
	// Confine has the operating system hold a host session (SandboxNone or
	// SandboxClaude) to its granted mounts: see HostBoundary. A platform
	// that cannot refuses the session with ErrUnsupported.
	Confine bool
	// SandboxImage is the image a SandboxContainer session runs in, and the
	// template a SandboxSbx session is created from (empty selects the
	// sbx CLI's own claude template).
	SandboxImage            string
	SandboxDomains          []string
	ContainerUseEnvironment string
	// VCSAccess permits workspace VCS mounts and caller-supplied VCS environment.
	// It is not a security boundary on an unsandboxed host that is not confined.
	VCSAccess bool
	Shell     string
	Env       map[string]string
	Skills    []string
}

// Validate rejects modes that would silently run without the requested box.
func (p Profile) Validate() error {
	if p.Sandbox != "" && !slices.Contains(Sandboxes, p.Sandbox) {
		return fmt.Errorf("sandbox %q is not implemented (agent can run %s)", p.Sandbox, strings.Join(Sandboxes, ", "))
	}
	if p.Sandbox == SandboxClaude && p.Agent != "" && p.Agent != AgentClaude {
		return fmt.Errorf("sandbox %q is Claude Code's sandbox and agent %q does not run under it", p.Sandbox, p.Agent)
	}
	if p.Sandbox == SandboxSbx && p.Agent != "" && p.Agent != AgentClaude {
		return fmt.Errorf("sandbox %q runs agent %q only, not %q", p.Sandbox, AgentClaude, p.Agent)
	}
	if p.isolated() && p.Confine {
		return fmt.Errorf("confine holds a host session to its mounts; sandbox %q binds nothing else already", p.Sandbox)
	}
	if p.Sandbox == SandboxContainer && p.SandboxImage == "" && p.ContainerUseEnvironment == "" {
		return fmt.Errorf("sandbox %q needs sandbox_image or container_use_environment", p.Sandbox)
	}
	if p.Sandbox == SandboxSbx && p.ContainerUseEnvironment != "" {
		return fmt.Errorf("sandbox %q takes a template through sandbox_image, not a container_use_environment", p.Sandbox)
	}
	return nil
}

// isolated reports whether the session runs somewhere other than this host:
// in a container or in a Docker Sandbox. Such a session is given its mounts
// and nothing else of the host, and reaches a caller-supplied server over
// HTTP.
func (p Profile) isolated() bool {
	return p.Sandbox == SandboxContainer || p.Sandbox == SandboxSbx
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
