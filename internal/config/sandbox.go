package config

import (
	"fmt"
	"os/exec"
	"runtime"
	"slices"
	"strings"
)

// Sandbox modes accepted by the sandbox key, weakest first: how much of the
// machine a session of that role can reach.
const (
	// SandboxNone runs claude directly, as the user bees runs as, with
	// everything that user can reach: the home directory, credentials, the
	// network and every other checkout on the machine.
	SandboxNone = "none"
	// SandboxClaude runs it inside Claude Code's own sandbox: shell commands
	// are boxed by the operating system (Seatbelt on macOS, bubblewrap on
	// Linux) and the built-in tools by Claude Code's permission rules, so
	// the session writes only to the worktree and the state directory and
	// reaches only GitHub.
	SandboxClaude = "claude"
	// SandboxContainer runs it inside a container holding the worktree and
	// the state directory, and nothing else of the host.
	SandboxContainer = "container"
)

// SandboxModes lists the accepted sandbox values, weakest first.
var SandboxModes = []string{SandboxNone, SandboxClaude, SandboxContainer}

// sandboxImplemented are the modes a session can actually run in. The rest
// load from bees.toml but no session runs in them; CheckSandbox refuses them
// before the first one starts.
var sandboxImplemented = []string{SandboxNone, SandboxClaude}

// hostOS and lookPath are what CheckSandboxHost asks about the machine, as
// variables so a test can describe a machine it is not running on.
var (
	hostOS   = runtime.GOOS
	lookPath = exec.LookPath
)

// claudeSandboxTools are the programs Claude Code's sandbox needs on Linux:
// bubblewrap builds the filesystem box and socat relays the network through
// the proxy. On macOS the box is the Seatbelt framework built into the
// system, which needs nothing installed.
var claudeSandboxTools = []string{"bwrap", "socat"}

// CheckSandbox reports whether every enabled role's sandbox mode can be run
// on this machine. `bees run` calls it once at startup: a mode that needs an
// engine needs it for every session, so discovering that a session at a time
// would leave the factory failing one issue after another, and a factory that
// cannot build the box a role asked for must not fall back to running that
// role unboxed.
func (c *Config) CheckSandbox() error {
	for _, name := range Roles {
		r, err := c.Role(name)
		if err != nil {
			return err
		}
		if !r.Enabled {
			continue
		}
		if err := CheckSandboxMode(r.Sandbox); err != nil {
			return fmt.Errorf("roles.%s: %w", name, err)
		}
		if err := CheckSandboxAgent(r.Sandbox, r.Agent); err != nil {
			return fmt.Errorf("roles.%s: %w", name, err)
		}
		if err := CheckSandboxHost(r.Sandbox); err != nil {
			return fmt.Errorf("roles.%s: %w", name, err)
		}
	}
	return nil
}

// CheckSandboxMode reports whether bees implements one resolved mode. An
// empty mode is SandboxNone, so a ResolvedRole a test built by hand is not a
// failure. Loading has already rejected a mode that is not in SandboxModes,
// so what is left is a mode bees knows and cannot provide. The session runner
// asks this before every session, so `bees exec` and `bees tick` refuse the
// same session `bees run` would.
func CheckSandboxMode(mode string) error {
	if mode == "" || slices.Contains(sandboxImplemented, mode) {
		return nil
	}
	return fmt.Errorf("sandbox %q is not implemented (bees can run %s)", mode, strings.Join(sandboxImplemented, ", "))
}

// CheckSandboxAgent reports whether the role's agent can run under one
// mode. SandboxClaude is Claude Code's own sandbox, so a codex role asking
// for it would run with codex's approvals and sandbox switched off and
// nothing boxing it; that is refused, both here and by the runner, rather
// than run unboxed. None asks nothing of the agent, and container is refused
// before this is asked.
func CheckSandboxAgent(mode, agent string) error {
	if mode == SandboxClaude && agent == AgentCodex {
		return fmt.Errorf("sandbox %q is Claude Code's sandbox and agent %q does not run under it", mode, agent)
	}
	return nil
}

// CheckSandboxHost reports whether this machine can build one mode bees
// implements. Claude Code's sandbox runs on macOS, where the system provides
// it, and on Linux, where bubblewrap and socat must be installed: the
// settings bees writes tell claude to refuse to start rather than run
// unboxed when they are missing, so without this check every session would
// fail the same way one at a time. It is a question about the machine, not
// the session, so `bees run` asks it once and the runner does not.
func CheckSandboxHost(mode string) error {
	if mode != SandboxClaude {
		return nil
	}
	switch hostOS {
	case "darwin":
		return nil
	case "linux":
		var missing []string
		for _, tool := range claudeSandboxTools {
			if _, err := lookPath(tool); err != nil {
				missing = append(missing, tool)
			}
		}
		if len(missing) > 0 {
			return fmt.Errorf("sandbox %q needs %s on PATH: Claude Code's sandbox uses bubblewrap and socat on Linux", mode, strings.Join(missing, " and "))
		}
		return nil
	default:
		return fmt.Errorf("sandbox %q runs on macOS and Linux only, not %s", mode, hostOS)
	}
}
