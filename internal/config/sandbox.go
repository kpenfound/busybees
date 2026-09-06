package config

import (
	"fmt"
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
	// SandboxClaude runs it inside Claude Code's own sandbox.
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
var sandboxImplemented = []string{SandboxNone}

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
	}
	return nil
}

// CheckSandboxMode reports whether one resolved mode can be run here. An
// empty mode is SandboxNone, so a ResolvedRole a test built by hand is not a
// failure. Loading has already rejected a mode that is not in SandboxModes,
// so what is left is a mode bees knows and cannot provide.
func CheckSandboxMode(mode string) error {
	if mode == "" || slices.Contains(sandboxImplemented, mode) {
		return nil
	}
	return fmt.Errorf("sandbox %q is not implemented (bees can run %s)", mode, strings.Join(sandboxImplemented, ", "))
}
