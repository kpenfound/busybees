package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
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
// ":ro", and nothing else of the host but, when the working directory
// cannot be sbx's primary workspace, an empty directory of the runner's
// (below): the shared skills store sbx mounts by default is switched off.
// The agent's own credential is not forwarded: the
// sandbox's credential proxy injects the one stored with `sbx secret set`
// into the agent's requests, and the value never enters the VM. Everything
// else is as a container session: an environment built from the session's
// variables alone, passed to the client by name, and the caller-supplied
// server on the host, reached at host.docker.internal over HTTP with a
// per-session token.
//
// The sandbox's proxy turns host.docker.internal into the host's localhost
// and holds it to the network policy, which denies it by default. The
// runner allows the sandbox, and no other, each port on the host's loopback
// it listens on for the session, and each granted host server's
// (sbxhost.go), with `sbx policy allow network --sandbox <name>
// localhost:<port>`, once the sandbox exists and before the backend's
// probes or command run. A rule sbx refuses stops the session before
// anything runs in the sandbox. The runner removes each rule it added
// before it removes the sandbox, whether the session ended, a later step
// of its setup failed, or it was stopped; a rule that cannot be removed is
// logged and the sandbox removed all the same. sbx drops a sandbox's rules with the
// sandbox, so a crash that leaves the sandbox behind leaves its rules with
// it, and `sbx rm` removes both.
//
// sbx starts the sandbox by writing the agent's instructions (CLAUDE.md and
// kits-agent-context/ for claude) into the parent of the primary workspace,
// the first one `sbx create` is given, and fails the whole create with a
// bare HTTP 500 when that parent is read-only; it refuses a read-only
// primary workspace outright. The working directory is the primary
// workspace when it can be: writable, and with a parent no bind covers, so
// that sbx writes into the sandbox's own filesystem and not onto the host.
// Otherwise, a working directory that is read-only or directly inside
// another workspace, the runner makes an empty directory of its own under
// the host's temporary directory the primary workspace, puts the working
// directory with the other workspaces and its access unchanged, and removes
// the directory with the sandbox. `sbx exec` is always given the working
// directory, so the session never starts in it.
//
// The sandbox is created for the profile's agent, from sbx's own template
// for it unless the profile names one. A profile that asks for Dagger also
// gets the Dagger CLI and the host's engine (sbxdagger.go).
//
// The session is `sbx create` before the server starts (a failed create
// leaves nothing to stop), with Dagger a setup `sbx exec` that installs the
// Dagger CLI, an `sbx policy allow network` for each host port the session
// reaches (the Dagger engine's, the caller-supplied server's, then the host
// servers'), `sbx exec --interactive` with the backend's command line, its
// prompt on stdin, an `sbx policy rm network` for each of those rules, and
// `sbx rm --force` when the session ends, because a sandbox outlives the
// command it ran. The sandbox's name is recorded in the session directory
// while it exists (procs.SandboxNameFile).

// sandbox is one session's Docker Sandbox: the container fields it shares
// (the caller-supplied server, the session's variables, the turn) and the
// sandbox itself.
type sandbox struct {
	container
	// created is whether `sbx create` succeeded, so that close removes
	// what exists and nothing else.
	created bool
	// forward carries the session's connections to a Dagger engine on a
	// host socket; nil without one (sbxdagger.go).
	forward *forward
	// allowed are the network policy rules the runner added for this
	// sandbox alone (allowHost), as `sbx policy` names their resources.
	allowed []string
	// primary is the directory the runner made to be the sandbox's
	// primary workspace when the working directory cannot be
	// (ownPrimary); empty when the working directory is.
	primary string
}

// startSandbox creates the sandbox and starts the caller-supplied server on
// the host. Run has already asked Profile.Validate what the box needs, and
// the boundary what it is granted. The agent is started by Run, through
// command; close removes the sandbox and stops the server.
func (r *Runner) startSandbox(ctx context.Context, req Request, sessionDir string, turn *Turn) (*sandbox, error) {
	s := &sandbox{container: container{r: r, req: req, sessionDir: sessionDir, turn: turn, name: sandboxName(r.namePrefix()+req.Name, randomHex(4)), image: req.Profile.SandboxImage, listen: r.sandboxListen}}
	s.vars = turnVars(turn)
	if err := s.create(ctx); err != nil {
		return nil, err
	}
	if err := procs.WriteSandboxName(sessionDir, s.name); err != nil {
		s.close()
		return nil, fmt.Errorf("record the sandbox's name: %w", err)
	}
	if req.Profile.Dagger != nil {
		if err := s.startDagger(ctx); err != nil {
			s.close()
			return nil, err
		}
	}
	if req.HostMCP != nil {
		if err := s.startServer(ctx); err != nil {
			s.close()
			return nil, err
		}
		if err := s.allowHost(ctx, s.serverPort); err != nil {
			s.close()
			return nil, err
		}
	}
	for _, h := range s.turn.HostServers {
		if err := s.allowHost(ctx, strconv.Itoa(h.Port)); err != nil {
			s.close()
			return nil, err
		}
	}
	return s, nil
}

// mcpEntries are the MCP servers the session is given: the container's,
// with each granted host server's entry at host.docker.internal.
func (s *sandbox) mcpEntries(mcp map[string]MCPEntry) map[string]MCPEntry {
	mcp = s.container.mcpEntries(mcp)
	for _, h := range s.turn.HostServers {
		e := mcp[h.Name]
		e.URL = sandboxURL(e.URL)
		mcp[h.Name] = e
	}
	return mcp
}

// allowHost lets this sandbox, and no other, reach one port on the host's
// loopback, which it reaches as host.docker.internal. The sandbox must
// exist: sbx refuses a rule for a sandbox it does not have. remove takes
// the rule away again. A port allowed already, which two servers can
// share, is allowed once.
func (s *sandbox) allowHost(ctx context.Context, port string) error {
	resource := "localhost:" + port
	if slices.Contains(s.allowed, resource) {
		return nil
	}
	if out, err := s.r.sbxCommand(ctx, "policy", "allow", "network", "--sandbox", s.name, resource).CombinedOutput(); err != nil {
		if msg := bytes.TrimSpace(out); len(msg) > 0 {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return fmt.Errorf("allow sandbox %s to reach the host's port %s (%s policy allow network): %w", s.name, port, SandboxCLI, err)
	}
	s.allowed = append(s.allowed, resource)
	return nil
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

// create runs `sbx create` for the profile's agent: the sandbox is named,
// given the turn's binds as its workspaces with the primary one first (the
// working directory, or a directory of the runner's when it cannot be,
// ownPrimary), created from the profile's template when it names one and
// from sbx's own for the agent otherwise (SbxTemplates), and without the
// shared skills store. A failed create leaves no directory of the runner's
// behind; sbx removes the sandbox it could not start.
func (s *sandbox) create(ctx context.Context) (err error) {
	defer func() {
		if err != nil {
			s.removePrimary()
		}
	}()
	own, err := s.ownPrimary()
	if err != nil {
		return err
	}
	if own {
		if err := s.makePrimary(); err != nil {
			return err
		}
	}
	workspaces, err := s.workspaces()
	if err != nil {
		return err
	}
	args := []string{"create", "--quiet", "--name", s.name, "--skills", "off"}
	if s.image != "" {
		args = append(args, "--template", s.image)
	}
	agent := s.req.Profile.Agent
	if agent == "" {
		agent = AgentClaude
	}
	args = append(args, agent)
	args = append(args, workspaces...)
	if out, err := s.r.sbxCommand(ctx, args...).CombinedOutput(); err != nil {
		if msg := bytes.TrimSpace(out); len(msg) > 0 {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return fmt.Errorf("create sandbox %s with workspaces %s (%s create): %w", s.name, strings.Join(workspaces, " "), SandboxCLI, err)
	}
	s.created = true
	return nil
}

// ownPrimary is whether the working directory cannot be the sandbox's
// primary workspace: sbx refuses a read-only one, and fails to start when
// the primary workspace's parent is read-only, where it writes the agent's
// instructions. A parent inside a writable bind would take those writes
// onto the host, so a working directory directly inside any bind is not
// the primary workspace either.
func (s *sandbox) ownPrimary() (bool, error) {
	work := s.req.workDir()
	i := slices.IndexFunc(s.turn.Binds, func(b Bind) bool { return b.Destination == work })
	if i < 0 {
		return false, fmt.Errorf("%w: the working directory %s is not among the sandbox's workspaces", ErrNotGranted, work)
	}
	if s.turn.Binds[i].Access == ReadOnly {
		return true, nil
	}
	return underBind(s.turn.Binds, filepath.Dir(work)), nil
}

// underBind is whether path is at or inside one of binds' destinations.
func underBind(binds []Bind, path string) bool {
	return slices.ContainsFunc(binds, func(b Bind) bool { return inside(b.Destination, path) })
}

// makePrimary makes the empty directory that is the sandbox's primary
// workspace in place of the working directory, under the host's temporary
// directory at its real path, which is where sbx mounts it. Its parent,
// where sbx writes the agent's instructions, must lie outside every bind.
func (s *sandbox) makePrimary() error {
	dir, err := os.MkdirTemp("", s.r.namePrefix()+"sbx-primary-")
	if err != nil {
		return fmt.Errorf("make the sandbox's primary workspace: %w", err)
	}
	s.primary = dir
	if dir, err = filepath.EvalSymlinks(dir); err != nil {
		return fmt.Errorf("make the sandbox's primary workspace: %w", err)
	}
	s.primary = dir
	if strings.Contains(dir, ":") {
		return fmt.Errorf("%w: the sandbox's primary workspace %s holds a colon, which %s create cannot take", ErrUnsupported, dir, SandboxCLI)
	}
	if underBind(s.turn.Binds, filepath.Dir(dir)) {
		return fmt.Errorf("%w: the sandbox's primary workspace %s lies directly inside one of its workspaces, where %s would write the agent's instructions", ErrUnsupported, dir, SandboxCLI)
	}
	return nil
}

// removePrimary removes the directory makePrimary made, with whatever the
// sandbox left in it.
func (s *sandbox) removePrimary() {
	if s.primary == "" {
		return
	}
	if err := os.RemoveAll(s.primary); err != nil {
		s.r.Logger.Warn("remove sandbox primary workspace", "sandbox", s.name, "dir", s.primary, "err", err)
	}
	s.primary = ""
}

// workspaces are the turn's binds as `sbx create` takes them: each bind's
// destination, which sbx mounts the host directory at that path at, ":ro"
// when it is read-only; the primary workspace first, the working directory
// or the runner's own directory (primary), then the rest in path order.
// SandboxBoundary has refused a destination sbx cannot take. A working
// directory that lies inside a bind without being one is refused: sbx
// mounts a workspace only at its own path.
func (s *sandbox) workspaces() ([]string, error) {
	work := s.req.workDir()
	binds := slices.Clone(s.turn.Binds)
	slices.SortStableFunc(binds, func(a, b Bind) int {
		if s.primary == "" {
			switch {
			case a.Destination == work:
				return -1
			case b.Destination == work:
				return 1
			}
		}
		return strings.Compare(a.Destination, b.Destination)
	})
	if !slices.ContainsFunc(binds, func(b Bind) bool { return b.Destination == work }) {
		return nil, fmt.Errorf("%w: the working directory %s is not among the sandbox's workspaces", ErrNotGranted, work)
	}
	var out []string
	if s.primary != "" {
		out = append(out, s.primary)
	}
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
	return s.execCommand(bin, args, true)
}

// probe runs a command in the session's sandbox before the session's own:
// `sbx exec` without stdin unless talk converses over it, with the
// session's variables and the backend's extra ones laid over them.
func (s *sandbox) probe(ctx context.Context, bin string, args []string, extra []envVar, talk talker) ([]byte, error) {
	if _, err := agentbin.Resolve(bin); err != nil {
		return nil, err
	}
	p := *s
	p.vars = append(slices.Clone(s.vars), extra...)
	sbx, sbxArgs, err := p.execCommand(bin, args, false)
	if talk != nil && err == nil {
		// After --workdir, so the probe keeps a probe's shape: the
		// session's own exec is the one that starts with --interactive.
		sbxArgs = slices.Insert(sbxArgs, 3, "--interactive")
	}
	if err != nil {
		return nil, err
	}
	cmd := agentbin.CommandContext(ctx, sbx, sbxArgs...)
	cmd.Dir = s.req.workDir()
	cmd.Env = p.clientEnv()
	return runProbe(cmd, nil, nil, talk)
}

// execCommand is the `sbx exec` command line that runs bin with args in
// the sandbox: the session's own (session: stdin kept open for its prompt)
// or a probe's.
func (s *sandbox) execCommand(bin string, args []string, session bool) (string, []string, error) {
	out := []string{"exec"}
	if session {
		out = append(out, "--interactive")
	}
	out = append(out, "--workdir", s.req.workDir())
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
	prefix, err := s.pathPrefix()
	if err != nil {
		return "", nil, err
	}
	out = append(out, prefix...)
	out = append(out, bin)
	out = append(out, args...)
	return s.r.sbxBin(), out, nil
}

// remove removes the sandbox's network policy rules and then the sandbox,
// for a session that is being stopped or has ended: killing the client
// leaves the sandbox, and the sandbox persists once its command has
// exited. A rule is removed before the sandbox, while sbx still knows the
// sandbox it is scoped to.
func (s *sandbox) remove() {
	if !s.created {
		return
	}
	s.created = false
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, resource := range s.allowed {
		if out, err := s.r.sbxCommand(ctx, "policy", "rm", "network", "--sandbox", s.name, "--resource", resource, "--force").CombinedOutput(); err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) || len(out) > 0 {
				err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
			}
			s.r.Logger.Warn("remove sandbox network rule", "sandbox", s.name, "resource", resource, "err", err)
		}
	}
	s.allowed = nil
	if out, err := s.r.sbxCommand(ctx, "rm", "--force", s.name).CombinedOutput(); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) || len(out) > 0 {
			err = fmt.Errorf("%w: %s", err, bytes.TrimSpace(out))
		}
		s.r.Logger.Warn("remove sandbox", "sandbox", s.name, "err", err)
	}
}

// close stops the caller-supplied server and the Dagger engine's forward,
// removes the sandbox and the runner's primary workspace, and forgets both
// records.
func (s *sandbox) close() {
	s.container.close()
	if s.forward != nil {
		s.forward.close()
		s.forward = nil
	}
	s.remove()
	s.removePrimary()
	procs.RemoveSandboxName(s.sessionDir)
}

// sandboxNameMax is the longest sandbox name `sbx create` accepts.
const sandboxNameMax = 63

// sandboxName makes a name sbx accepts out of a session name and a random
// suffix: letters, digits, hyphens and periods, the first one a letter or a
// digit, at most sandboxNameMax characters. A session name too long to fit
// is cut short and followed by a hash of the whole of it, so two long names
// that share a beginning still name different sandboxes.
func sandboxName(session, suffix string) string {
	var b strings.Builder
	for _, r := range session {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	name := strings.Trim(b.String(), "-.")
	if name == "" {
		name = "session"
	}
	if room := sandboxNameMax - len("-") - len(suffix); len(name) > room {
		sum := sha256.Sum256([]byte(session))
		hash := hex.EncodeToString(sum[:4])
		name = strings.TrimRight(name[:max(room-len("-")-len(hash), 0)], "-.")
		if name == "" {
			name = hash
		} else {
			name += "-" + hash
		}
	}
	return name + "-" + suffix
}

// sbxCommand is an sbx CLI command outside the session's own `sbx exec`,
// run with the host's environment.
func (r *Runner) sbxCommand(ctx context.Context, args ...string) *exec.Cmd {
	cmd := agentbin.CommandContext(ctx, r.sbxBin(), args...)
	cmd.Env = hostEnv(r.EnvironmentPrefix)
	return cmd
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
// root, or a path holding a colon. The Dagger engine is given only to a
// profile that asks for it and is granted it (verifyDagger), and an MCP
// entry on the host only when its name and port are granted (HostServer).
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
