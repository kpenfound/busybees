package agent

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/agent/procs"
)

const (
	AgentClaude      = "claude"
	AgentCodex       = "codex"
	AgentOpenCode    = "opencode"
	AgentPi          = "pi"
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

// Agents lists the backends the runner builds a command line for. It is
// procs' list of agent executables: procs recognizes a running session in
// the process table by the command it runs and cannot import this
// package, so the two are one list and an agent added to it is known to
// both. The backend descriptors (Backends) describe the same agents and
// are held to this list by test rather than derived from it.
var Agents = procs.AgentExecutables

// Sandboxes lists the sandbox kinds the runner implements, weakest first.
var Sandboxes = []string{SandboxNone, SandboxClaude, SandboxContainer, SandboxSbx}

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
	// Sandbox is one of Sandboxes; empty is SandboxNone.
	Sandbox string
	// Confine has the operating system hold a host session (SandboxNone or
	// SandboxClaude) to its granted mounts: see HostBoundary. A platform
	// that cannot refuses the session with ErrUnsupported.
	Confine bool
	// SandboxImage is the image a SandboxContainer session runs in, and the
	// template a SandboxSbx session is created from (empty selects the
	// sbx CLI's own template for the agent, SbxTemplates).
	SandboxImage string
	// Dagger gives a SandboxSbx session the Dagger CLI and a host's Dagger
	// engine to run it against; nil is neither. The session must also be
	// granted the engine (Grants.DaggerEngine).
	Dagger                  *Dagger
	SandboxDomains          []string
	ContainerUseEnvironment string
	// VCSAccess permits workspace VCS mounts and caller-supplied VCS environment.
	// It is not a security boundary on an unsandboxed host that is not confined.
	VCSAccess bool
	Shell     string
	Env       map[string]string
	Skills    []string
	// PiPackages are pi package sources (npm:, git:, a URL or a local path)
	// a pi session loads after PiMCPAdapter, which it always loads.
	PiPackages []string
}

// Validate rejects modes that would silently run without the requested box.
func (p Profile) Validate() error {
	if p.Sandbox != "" && !slices.Contains(Sandboxes, p.Sandbox) {
		return fmt.Errorf("sandbox %q is not implemented (agent can run %s)", p.Sandbox, strings.Join(Sandboxes, ", "))
	}
	if p.Sandbox == SandboxClaude && p.Agent != "" && p.Agent != AgentClaude {
		return fmt.Errorf("sandbox %q is Claude Code's sandbox and agent %q does not run under it", p.Sandbox, p.Agent)
	}
	if p.Sandbox == SandboxSbx && p.Agent != "" && SbxTemplates[p.Agent] == "" {
		return fmt.Errorf("sandbox %q has no template for agent %q", p.Sandbox, p.Agent)
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
	if p.Dagger != nil {
		if p.Sandbox != SandboxSbx {
			return fmt.Errorf("the Dagger engine is given to a session in sandbox %q only, not %q", SandboxSbx, p.Sandbox)
		}
		if err := CheckDaggerEngine(p.Dagger.Engine); err != nil {
			return err
		}
		if err := CheckDaggerVersion(p.Dagger.Version); err != nil {
			return err
		}
	}
	return nil
}

// SbxTemplates are the sbx CLI's template images, per agent: what a
// template of the operator's is built on (sbx's own default for `sbx
// create <agent>` is the image's "-docker" variant). An agent without one
// cannot run in SandboxSbx.
var SbxTemplates = map[string]string{
	AgentClaude:   "docker/sandbox-templates:claude-code",
	AgentCodex:    "docker/sandbox-templates:codex",
	AgentOpenCode: "docker/sandbox-templates:opencode",
}

// Dagger is the Dagger engine a SandboxSbx session runs the Dagger CLI
// against, and the CLI release installed in the sandbox for it.
type Dagger struct {
	// Engine is where the engine listens on the host: "unix://<path>" for
	// its socket, or "tcp://<host>:<port>".
	Engine string
	// Version is the Dagger CLI release installed in the sandbox, the
	// engine's own: "v0.20.5" or "0.20.5".
	Version string
}

// daggerVersion is a release as the Dagger install script takes one.
var daggerVersion = regexp.MustCompile(`^v?[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

// CheckDaggerEngine reports whether an engine address is one a sandbox can
// be given: a socket by its absolute path, or a TCP address with a port.
func CheckDaggerEngine(engine string) error {
	scheme, rest, ok := strings.Cut(engine, "://")
	switch {
	case !ok:
	case scheme == "unix" && filepath.IsAbs(rest):
		return nil
	case scheme == "tcp":
		host, port, err := net.SplitHostPort(rest)
		if n, perr := strconv.Atoi(port); err == nil && host != "" && perr == nil && n >= 1 && n <= 65535 {
			return nil
		}
	}
	return fmt.Errorf("the Dagger engine %q must be unix://<absolute path of its socket> or tcp://<host>:<port>", engine)
}

// CheckDaggerVersion reports whether a Dagger CLI release is one the
// install script takes.
func CheckDaggerVersion(version string) error {
	if !daggerVersion.MatchString(version) {
		return fmt.Errorf("the Dagger CLI version %q must be a release such as v0.20.5", version)
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
