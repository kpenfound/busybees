package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
	"github.com/kpenfound/busybees/core/agent/procs"
)

// A SandboxSbx session runs the backend command unchanged inside a Docker
// Sandbox (https://docs.docker.com/ai/sandboxes/): a microVM the sbx CLI
// creates, with its own kernel, filesystem, Docker daemon and network. The
// sandbox is given the turn's binds as its workspaces, each mounted at its
// host path (sbx mounts a workspace nowhere else), read-only ones with
// ":ro", and nothing else of the host: the shared skills store sbx mounts by
// default is switched off. The agent's own credential is not forwarded: the
// sandbox's credential proxy injects the one stored with `sbx secret set`
// into the agent's requests, and the value never enters the VM. Everything
// else is as a container session: an environment built from the session's
// variables alone, passed to the client by name, and the caller-supplied
// server on the host, reached at host.docker.internal over HTTP with a
// per-session token.
//
// The session is three sbx commands: `sbx create` before the server starts
// (a failed create leaves nothing to stop), `sbx exec` with the backend's
// command line, its prompt on stdin, and `sbx rm --force` when the session
// ends, because a sandbox outlives the command it ran. The sandbox's name is
// recorded in the session directory while it exists (procs.SandboxNameFile).

// sandbox is one session's Docker Sandbox: the container fields it shares
// (the caller-supplied server, the session's variables, the turn) and the
// sandbox itself.
type sandbox struct {
	container
	// created is whether `sbx create` succeeded, so that close removes
	// what exists and nothing else.
	created bool
}

// startSandbox creates the sandbox and starts the caller-supplied server on
// the host. Run has already asked Profile.Validate what the box needs, and
// the boundary what it is granted. The agent is started by Run, through
// command; close removes the sandbox and stops the server.
func (r *Runner) startSandbox(ctx context.Context, req Request, sessionDir string, turn *Turn) (*sandbox, error) {
	s := &sandbox{container: container{r: r, req: req, sessionDir: sessionDir, turn: turn, name: sandboxName(r.namePrefix()+req.Name) + "-" + randomHex(4), image: req.Profile.SandboxImage, listen: r.sandboxListen}}
	s.vars = turnVars(turn)
	if err := s.create(ctx); err != nil {
		return nil, err
	}
	if err := procs.WriteSandboxName(sessionDir, s.name); err != nil {
		s.close()
		return nil, fmt.Errorf("record the sandbox's name: %w", err)
	}
	if req.HostMCP != nil {
		if err := s.startServer(ctx); err != nil {
			s.close()
			return nil, err
		}
	}
	return s, nil
}

// sandboxListen is the address the caller-supplied server listens on for a
// sandbox session: ContainerListen when set, else the loopback. The
// sandbox's proxy turns host.docker.internal into the host's localhost
// before it forwards a connection, on every operating system.
func (r *Runner) sandboxListen(context.Context) (string, error) {
	if r.ContainerListen != "" {
		return r.ContainerListen, nil
	}
	return "127.0.0.1:0", nil
}

// create runs `sbx create`: the sandbox is named, given the turn's binds as
// its workspaces with the working directory first (the primary workspace,
// where `sbx exec` starts), created from the profile's template when it
// names one, and without the shared skills store.
func (s *sandbox) create(ctx context.Context) error {
	workspaces, err := s.workspaces()
	if err != nil {
		return err
	}
	args := []string{"create", "--quiet", "--name", s.name, "--skills", "off"}
	if s.image != "" {
		args = append(args, "--template", s.image)
	}
	args = append(args, AgentClaude)
	args = append(args, workspaces...)
	cmd := agentbin.CommandContext(ctx, s.r.sbxBin(), args...)
	cmd.Env = hostEnv(s.r.EnvironmentPrefix)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := bytes.TrimSpace(out); len(msg) > 0 {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return fmt.Errorf("create sandbox %s (%s create): %w", s.name, SandboxCLI, err)
	}
	s.created = true
	return nil
}

// workspaces are the turn's binds as `sbx create` takes them: each bind's
// destination, which sbx mounts the host directory at that path at, ":ro"
// when it is read-only; the working directory first, the rest in path
// order. SandboxBoundary has refused a destination sbx cannot take. A
// working directory that lies inside a bind without being one is refused:
// it is the sandbox's primary workspace.
func (s *sandbox) workspaces() ([]string, error) {
	work := s.req.workDir()
	binds := slices.Clone(s.turn.Binds)
	slices.SortStableFunc(binds, func(a, b Bind) int {
		switch {
		case a.Destination == work:
			return -1
		case b.Destination == work:
			return 1
		}
		return strings.Compare(a.Destination, b.Destination)
	})
	if len(binds) == 0 || binds[0].Destination != work {
		return nil, fmt.Errorf("%w: the working directory %s is not among the sandbox's workspaces", ErrNotGranted, work)
	}
	var out []string
	for _, b := range binds {
		w := b.Destination
		if b.Access == ReadOnly {
			w += ":ro"
		}
		out = append(out, w)
	}
	return out, nil
}

// command wraps the backend's command line in `sbx exec`: stdin kept open
// for the prompt, the session's variables by name (the value read from the
// client's environment, clientEnv), the working directory, then the
// sandbox and the command, behind a shell that puts the stand-ins for the
// denied executables first on PATH when the turn has any.
func (s *sandbox) command(_ context.Context, bin string, args []string) (string, []string, error) {
	out := []string{"exec", "--interactive", "--workdir", s.req.workDir()}
	for _, v := range dedupe(s.vars) {
		if v.name == "HOME" {
			// The client keeps its own HOME to find its configuration
			// (clientEnv), so a HOME the session sets is passed by value:
			// by name it would be the operator's.
			out = append(out, "--env", v.name+"="+v.value)
			continue
		}
		out = append(out, "--env", v.name)
	}
	out = append(out, s.name)
	if denied := s.turn.DeniedExecutables; len(denied) > 0 {
		dir := filepath.Join(s.sessionDir, deniedBinDir)
		if err := writeDenied(dir, denied); err != nil {
			return "", nil, err
		}
		out = append(out, "/bin/sh", "-c", `PATH="$0:$PATH" exec "$@"`, dir)
	}
	out = append(out, bin)
	out = append(out, args...)
	return s.r.sbxBin(), out, nil
}

// remove removes the sandbox, for a session that is being stopped or has
// ended: killing the client leaves the sandbox, and the sandbox persists
// once its command has exited.
func (s *sandbox) remove() {
	if !s.created {
		return
	}
	s.created = false
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := agentbin.CommandContext(ctx, s.r.sbxBin(), "rm", "--force", s.name)
	cmd.Env = hostEnv(s.r.EnvironmentPrefix)
	if out, err := cmd.CombinedOutput(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) || len(out) > 0 {
			err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
		}
		s.r.Logger.Warn("remove sandbox", "sandbox", s.name, "err", err)
	}
}

// close stops the caller-supplied server, removes the sandbox and forgets
// both records.
func (s *sandbox) close() {
	s.container.close()
	s.remove()
	procs.RemoveSandboxName(s.sessionDir)
}

// sandboxName makes a name sbx accepts out of a session name: letters,
// digits, hyphens and periods, the first one a letter or a digit. The
// caller's random suffix makes it long enough.
func sandboxName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		return "session"
	}
	return name
}

func (r *Runner) sbxBin() string {
	if r.SbxBin != "" {
		return r.SbxBin
	}
	return SandboxCLI
}

// SandboxBoundary runs a session in a Docker Sandbox. The sandbox is given
// the granted mounts as its workspaces, with their access, and nothing else
// of the host; an environment built from the allowlist alone, with no HOME
// of its own (the sandbox has one; a HOME the session sets is passed) and
// no agent credential (the sandbox's proxy supplies it); and, without VCS, stand-ins for the VCS executables in front
// of the sandbox's PATH, which deny them by name and by nothing else: a VCS
// executable of the sandbox's image reached by its path is not masked.
// Verify refuses a request whose own paths the grants do not cover, the way
// ContainerBoundary does, and a workspace sbx cannot be handed: the host's
// root, or a path holding a colon.
type SandboxBoundary struct {
	// SessionsDir is where a session directory is created for a request
	// that names none.
	SessionsDir string
	// MountDirs must be granted read-write.
	MountDirs []string
	// SkillMountDirs must be granted when the profile has skills.
	SkillMountDirs []string
}

// Verify checks the request and builds the sandbox turn. It fails closed: a
// path the sandbox needs that no grant covers, a workspace sbx cannot take,
// or a variable outside the allowlist, is refused.
func (b SandboxBoundary) Verify(req Request) (*Turn, error) {
	turn, err := verifyCommon(req)
	if err != nil {
		return nil, err
	}
	binds := ContainerBoundary{SessionsDir: b.SessionsDir, MountDirs: b.MountDirs, SkillMountDirs: b.SkillMountDirs}
	if err := binds.bind(&bindSet{sbx: true}, req, turn); err != nil {
		return nil, err
	}
	if turn.Env, err = isolatedEnv(req, turn, nil, ""); err != nil {
		return nil, err
	}
	if !turn.VCS {
		turn.DeniedExecutables = slices.Clone(VCSExecutables)
	}
	return turn, nil
}
