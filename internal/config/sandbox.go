package config

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
	"time"
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
	// SandboxContainer runs it inside a container (docker) holding the
	// worktree, the repository's .git and the state directory, and nothing
	// else of the host: the built-in MCP server stays on the host and is
	// reached over HTTP.
	SandboxContainer = "container"
)

// SandboxModes lists the accepted sandbox values, weakest first.
var SandboxModes = []string{SandboxNone, SandboxClaude, SandboxContainer}

// sandboxImplemented are the modes a session can actually run in. The rest
// load from bees.toml but no session runs in them; CheckSandbox refuses them
// before the first one starts.
var sandboxImplemented = []string{SandboxNone, SandboxContainer}

// ContainerEngine is the container engine SandboxContainer runs on: the
// docker CLI, found on PATH. Anything answering to the same command line
// (podman's docker shim) does.
const ContainerEngine = "docker"

// AgentCredentials are the variables an agent reads its credential from,
// per agent. A container session has no keychain and no home directory of
// the host, so the credential has to be handed in through the environment:
// one of these on the host, forwarded into the container, or a role env
// entry of that name.
var AgentCredentials = map[string][]string{
	AgentClaude: {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
	AgentCodex:  {"OPENAI_API_KEY", "CODEX_API_KEY"},
}

// lookPath, getenv and engineCommand are what the container checks ask of
// the machine, as variables so a test can describe a machine it is not
// running on. engineCommand runs the engine with the arguments and returns
// its combined output.
var (
	lookPath      = exec.LookPath
	getenv        = os.Getenv
	engineCommand = func(args ...string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return exec.CommandContext(ctx, ContainerEngine, args...).CombinedOutput()
	}
)

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
		if err := CheckSandboxContainer(r, c.GitHub); err != nil {
			return fmt.Errorf("roles.%s: %w", name, err)
		}
		if err := CheckSandboxEngine(r.Sandbox, r.SandboxImage); err != nil {
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

// CheckSandboxContainer reports whether one resolved role has what a
// container session needs from its configuration: an image to run in, a
// GitHub credential the session's gh and pushes can use inside it, and a
// credential for its agent. Inside the container there is no keychain, no
// home directory and no gh login of the machine owner, so each of these
// must be handed in, and a session missing one fails at its first command
// rather than at start. `bees run` asks this once for every role in the
// rotation and the session runner asks it again, so `bees exec` and `bees
// tick` refuse the same session.
func CheckSandboxContainer(r ResolvedRole, gh GitHub) error {
	if r.Sandbox != SandboxContainer {
		return nil
	}
	if r.SandboxImage == "" {
		return fmt.Errorf("sandbox %q needs sandbox_image: the image the session runs in, holding the agent, git and gh", r.Sandbox)
	}
	if gh.ResolvedToken() == "" && r.Env[EnvGHToken] == "" {
		return fmt.Errorf("sandbox %q needs [github] (login and token) or %s in the role's env: inside the container gh and git push have no other credentials", r.Sandbox, EnvGHToken)
	}
	names := AgentCredentials[r.Agent]
	if r.Agent == "" {
		names = AgentCredentials[AgentClaude]
	}
	for _, n := range names {
		if getenv(n) != "" || r.Env[n] != "" {
			return nil
		}
	}
	return fmt.Errorf("sandbox %q needs a credential for the agent inside the container: set %s in the environment bees runs in, or in the role's env", r.Sandbox, strings.Join(names, " or "))
}

// EnvGHToken is the variable gh reads its credentials from; the session
// runner sets it from [github], and a role's env may set it instead.
const EnvGHToken = "GH_TOKEN"

// CheckSandboxEngine reports whether this machine can run a container
// session: the engine is on PATH, its daemon answers, and the image is
// present. It is a question about the machine, so `bees run` asks it once,
// ahead of the doctor, and the runner does not: a missing engine fails every
// session the same way, and an image is pulled by a person, deliberately,
// not by the factory at the first session. A mode other than container asks
// nothing.
func CheckSandboxEngine(mode, image string) error {
	if mode != SandboxContainer {
		return nil
	}
	if _, err := lookPath(ContainerEngine); err != nil {
		return fmt.Errorf("sandbox %q needs %s on PATH", mode, ContainerEngine)
	}
	if out, err := engineCommand("info", "--format", "{{.ServerVersion}}"); err != nil {
		return fmt.Errorf("sandbox %q: %s is installed but its daemon does not answer: %s", mode, ContainerEngine, oneLine(out, err))
	}
	if out, err := engineCommand("image", "inspect", "--format", "{{.Id}}", image); err != nil {
		return fmt.Errorf("sandbox %q: image %q is not on this machine (%s); pull or build it first: %s pull %s", mode, image, oneLine(out, err), ContainerEngine, image)
	}
	return nil
}

// oneLine renders a failed command for an error message: its output when
// it printed any, else the error.
func oneLine(out []byte, err error) string {
	if s := strings.TrimSpace(string(out)); s != "" {
		return strings.Join(strings.Fields(s), " ")
	}
	return err.Error()
}
