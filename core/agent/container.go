package agent

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"maps"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
	"github.com/kpenfound/busybees/core/agent/procs"
)

// A container session runs the backend command unchanged inside the engine.
// It sees the granted mounts and, of the host, nothing else but the runner's
// own stand-ins (ContainerBoundary.Masks), each mount at its host
// path, so references in prompts also resolve in the container; the work
// directory, session directory, shared VCS metadata, caller mounts and skill
// cache must lie inside them (ContainerBoundary).
// The container runs as the host user with a private tmpfs home. Environment
// values reach the engine by name, never on its command line.
//
// When HostMCP is supplied, its server runs in a separate host process group
// and the container receives an HTTP entry with a per-session bearer token.
// The engine writes a container id, and the runner records the host server PID
// beside it so orphan cleanup can remove both after a crash.

// containerHostAlias is the name the container reaches the host by. Docker
// Desktop resolves it on its own; on Linux the container is started with
// --add-host so it does.
const containerHostAlias = "host.docker.internal"

// hostOS, hostUID and hostGID describe the machine to the container code,
// as variables so a test can describe one it is not running on.
var (
	hostOS  = runtime.GOOS
	hostUID = os.Getuid
	hostGID = os.Getgid
)

// serverStart is how long the caller-supplied server has to report its address.
const serverStart = 15 * time.Second

// container is one session's box: the engine command that runs it, the
// caller-supplied server on the host it talks to, and what it is given.
type container struct {
	r          *Runner
	req        Request
	sessionDir string
	// name is the container's name (<name prefix><session name>-<random>).
	name string
	// image is what the container runs: the role's sandbox_image, or the
	// image built from its container_use_environment.
	image string
	// server is the caller-supplied MCP server on the host, and builtin the entry
	// the session reaches it through.
	server  *exec.Cmd
	builtin MCPEntry
	// turn is what the boundary verified the session may have.
	turn *Turn
	// vars is the session's environment inside the container.
	vars []envVar
}

// startContainer prepares a container session: it settles the image
// (building it from the role's container_use_environment when that is set,
// before anything else starts, so a failed build leaves nothing to stop),
// starts the caller-supplied server on the host and builds the session's
// environment. Run has already asked Profile.Validate what the
// box needs, and the boundary what it is granted. The container itself is started by Run, through command;
// close stops the server.
func (r *Runner) startContainer(ctx context.Context, req Request, sessionDir string, turn *Turn) (*container, error) {
	c := &container{r: r, req: req, sessionDir: sessionDir, turn: turn, name: r.namePrefix() + sanitize(req.Name) + "-" + randomHex(4), image: req.Profile.SandboxImage}
	if req.Profile.ContainerUseEnvironment != "" {
		image, err := r.containerUseImage(ctx, req, sessionDir)
		if err != nil {
			return nil, err
		}
		c.image = image
	}
	c.vars = turnVars(turn)
	if req.HostMCP != nil {
		if err := c.startServer(ctx); err != nil {
			return nil, err
		}
	}
	return c, nil
}

// startServer runs the caller HTTP server on the host with the
// environment a session on the host would have started it with, reads the
// address it reports, and builds the entry the session reaches it by: the
// host's alias, the port, and a token this session alone holds.
func (c *container) startServer(ctx context.Context) error {
	r := c.r
	addr, err := r.containerListen(ctx)
	if err != nil {
		return err
	}
	token := randomHex(32)
	h := c.req.HostMCP
	args := append(slices.Clone(h.Entry.Args), h.ListenArgs...)
	args = append(args, addr)
	cmd := agentbin.CommandContext(context.Background(), h.Entry.Command, args...)
	cmd.Dir = c.req.workDir()
	serverReq := c.req
	if h.Env != nil {
		serverReq.Env = h.Env
	}
	cmd.Env = r.env(serverReq, c.sessionDir)
	for _, key := range slices.Sorted(maps.Keys(h.Entry.Env)) {
		cmd.Env = append(cmd.Env, key+"="+h.Entry.Env[key])
	}
	cmd.Env = append(cmd.Env, h.TokenEnv+"="+token)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stderr, err := os.Create(filepath.Join(c.sessionDir, "mcp-server.log"))
	if err != nil {
		return err
	}
	defer func() { _ = stderr.Close() }()
	cmd.Stderr = stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start the built-in MCP server: %w", err)
	}
	c.server = cmd
	// The server is in a process group of its own, outside the session's
	// and outside this process's, so nothing finds it once this process is
	// gone. Recording its pid is what lets orphan cleanup reap it after a
	// crash; close removes the file again.
	if err := procs.WriteServerPID(c.sessionDir, cmd.Process.Pid); err != nil {
		c.close()
		return fmt.Errorf("record the built-in MCP server's pid: %w", err)
	}
	line := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		if sc.Scan() {
			line <- sc.Text()
		}
		close(line)
	}()
	var listening string
	select {
	case listening = <-line:
	case <-time.After(serverStart):
	case <-ctx.Done():
	}
	reported, ok := strings.CutPrefix(listening, h.ListeningPrefix)
	if !ok {
		c.close()
		return fmt.Errorf("the built-in MCP server did not report its address (got %q); see %s", listening, filepath.Join(c.sessionDir, "mcp-server.log"))
	}
	_, port, err := net.SplitHostPort(reported)
	if err != nil {
		c.close()
		return fmt.Errorf("the built-in MCP server reported %q: %w", reported, err)
	}
	c.vars = append(c.vars, envVar{h.TokenEnv, token})
	c.builtin = MCPEntry{
		Type:           "http",
		URL:            "http://" + net.JoinHostPort(containerHostAlias, port) + h.Path,
		BearerTokenEnv: h.TokenEnv,
	}
	return nil
}

// containerListen is the address the caller-supplied server listens on for a
// container session: ContainerListen when set, else the loopback on macOS,
// which Docker Desktop's host alias reaches, and the bridge gateway on
// Linux, which is the address the host alias resolves to there and the one
// address of the host a container can reach — the loopback is the
// container's own. Nothing else is supported.
func (r *Runner) containerListen(ctx context.Context) (string, error) {
	if r.ContainerListen != "" {
		return r.ContainerListen, nil
	}
	switch hostOS {
	case "darwin":
		return "127.0.0.1:0", nil
	case "linux":
		out, err := agentbin.CommandContext(ctx, r.dockerBin(), "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").Output()
		if err != nil {
			return "", fmt.Errorf("find the address the container reaches the host by (%s network inspect bridge): %w", ContainerEngine, err)
		}
		gw := strings.TrimSpace(string(out))
		if net.ParseIP(gw) == nil {
			return "", fmt.Errorf("%s network inspect bridge reported %q, not an address", ContainerEngine, gw)
		}
		return net.JoinHostPort(gw, "0"), nil
	default:
		return "", fmt.Errorf("sandbox %q runs on macOS and Linux only, not %s", SandboxContainer, hostOS)
	}
}

// command wraps the backend's command line in the engine's: the container
// is created removed-on-exit, named, labelled and recorded, given its
// mounts and its environment, and runs bin with args in the worktree.
// Values are passed to the engine by variable name, read from the engine
// client's own environment (clientEnv), so no secret appears on a command
// line another user of the machine can list.
func (c *container) command(ctx context.Context, bin string, args []string) (string, []string, error) {
	r, req := c.r, c.req
	out := []string{
		"run", "--rm", "--interactive",
		"--name", c.name,
		"--cidfile", filepath.Join(c.sessionDir, procs.ContainerIDFile),
		"--label", r.containerLabel() + "=" + c.sessionDir,
		"--workdir", req.workDir(),
		"--mount", "type=tmpfs,destination=" + r.containerHome() + ",tmpfs-mode=1777",
	}
	out = append(out, c.mounts()...)
	// The session runs as the host's user, not the image's: claude refuses
	// --dangerously-skip-permissions as root, and on Linux what the session
	// writes into the mounts must be the host user's or the host cannot
	// remove the worktree afterwards (Docker Desktop maps ownership on
	// macOS, where any user would do). The user has no passwd entry in the
	// image, which is why HOME is set explicitly.
	out = append(out, "--user", strconv.Itoa(hostUID())+":"+strconv.Itoa(hostGID()))
	if hostOS == "linux" {
		// The host alias is Docker Desktop's; Linux is told to resolve it
		// to the bridge gateway.
		out = append(out, "--add-host", containerHostAlias+":host-gateway")
	}
	for _, v := range dedupe(c.vars) {
		if v.name == "HOME" {
			// The engine client needs its own HOME to find its
			// configuration, so this one is not read from its environment.
			out = append(out, "--env", v.name+"="+v.value)
			continue
		}
		out = append(out, "--env", v.name)
	}
	out = append(out, c.image)
	if denied := c.turn.DeniedExecutables; len(denied) > 0 {
		// The image's PATH is not known here, so the stand-ins are put in
		// front of it inside, by a shell that then runs the command.
		dir := filepath.Join(c.sessionDir, deniedBinDir)
		if err := writeDenied(dir, denied); err != nil {
			return "", nil, err
		}
		out = append(out, "/bin/sh", "-c", `PATH="$0:$PATH" exec "$@"`, dir)
	}
	out = append(out, bin)
	out = append(out, args...)
	return r.dockerBin(), out, nil
}

// mounts are the bind mounts the container gets: the turn's binds, parents
// before what is bound inside them.
func (c *container) mounts() []string {
	binds := slices.Clone(c.turn.Binds)
	slices.SortStableFunc(binds, func(a, b Bind) int { return strings.Compare(a.Destination, b.Destination) })
	var out []string
	for _, b := range binds {
		spec := "type=bind,source=" + b.Source + ",destination=" + b.Destination
		if b.Access == ReadOnly {
			spec += ",readonly"
		}
		out = append(out, "--mount", spec)
	}
	return out
}

// clientEnv is the environment the engine client runs with: the host's,
// with the session's variables laid over it so the engine reads their
// values by name. Each name appears once, holding the value set last: the
// engine reads a name's first occurrence, and a role's env must not win
// over the caller's own variables here when it does not on the host. HOME is the
// exception (see command).
func (c *container) clientEnv() []string {
	vars := dedupe(c.vars)
	names := map[string]bool{}
	for _, v := range vars {
		if v.name == "HOME" {
			continue // the container's HOME is on the command line; the client keeps its own
		}
		names[v.name] = true
	}
	var env []string
	for _, kv := range hostEnv(c.r.EnvironmentPrefix) {
		name, _, _ := strings.Cut(kv, "=")
		if !names[name] {
			env = append(env, kv)
		}
	}
	for _, v := range vars {
		if v.name != "HOME" {
			env = append(env, v.name+"="+v.value)
		}
	}
	return env
}

// dedupe keeps one entry per name, in the order names first appear, with
// the value that was set last: the order sessionVars sets them in is the
// precedence order, later wins.
func dedupe(vars []envVar) []envVar {
	var out []envVar
	index := map[string]int{}
	for _, v := range vars {
		if i, ok := index[v.name]; ok {
			out[i].value = v.value
			continue
		}
		index[v.name] = len(out)
		out = append(out, v)
	}
	return out
}

// remove stops and removes the container, for a session that is being
// stopped: killing the engine client alone leaves the container running.
func (c *container) remove() {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	_ = agentbin.CommandContext(ctx, c.r.dockerBin(), "rm", "--force", c.name).Run()
}

// close stops the caller-supplied server and forgets the container id and the
// server's pid: the container is gone (--rm) or removed, and the server has
// been killed, so neither record has anything left to point at.
func (c *container) close() {
	if c.server != nil && c.server.Process != nil {
		_ = syscall.Kill(-c.server.Process.Pid, syscall.SIGKILL)
		_ = c.server.Wait()
		c.server = nil
	}
	procs.RemoveServerPID(c.sessionDir)
	procs.RemoveContainerID(c.sessionDir)
}

// turnVars reads a turn's environment back into name/value pairs.
func turnVars(turn *Turn) []envVar {
	vars := make([]envVar, 0, len(turn.Env))
	for _, kv := range turn.Env {
		k, v, _ := strings.Cut(kv, "=")
		vars = append(vars, envVar{k, v})
	}
	return vars
}

func (r *Runner) dockerBin() string {
	if r.DockerBin != "" {
		return r.DockerBin
	}
	return ContainerEngine
}

func (r *Runner) containerHome() string {
	if r.ContainerHome != "" {
		return r.ContainerHome
	}
	return "/home/agent"
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// ContainerBoundary runs a session in a container. The container is given
// the granted mounts, with their access, and nothing else of the host; an
// environment built from the allowlist alone; and, without VCS, stand-ins
// for the VCS executables in front of the image's PATH, which deny them by
// name, and Masks over the paths the caller found them at. Verify refuses a
// request whose own paths the grants do not cover.
type ContainerBoundary struct {
	// Environ is the host environment agent credentials are read from; nil
	// reads os.Environ.
	Environ func() []string
	// Home is the container's private HOME.
	Home string
	// SessionsDir is where a session directory is created for a request
	// that names none.
	SessionsDir string
	// MountDirs must be granted read-write.
	MountDirs []string
	// SkillMountDirs must be granted when the profile has skills.
	SkillMountDirs []string
	// Masks are read-only binds the runner owns, laid over paths of the
	// image for a turn without VCS: a stand-in over each VCS executable the
	// image has (see NewContainer). Their sources need no grant: they show
	// the container nothing of the host but the stand-ins themselves.
	Masks []Bind
}

// Verify checks the request and builds the container turn. It fails closed:
// a path the container needs that no grant covers, a mount the engine
// cannot be given exactly, or a variable outside the allowlist is refused.
func (b ContainerBoundary) Verify(req Request) (*Turn, error) {
	turn, err := verifyCommon(req)
	if err != nil {
		return nil, err
	}
	if err := b.bind(req, turn); err != nil {
		return nil, err
	}
	if turn.Env, err = b.env(req, turn); err != nil {
		return nil, err
	}
	if !turn.VCS {
		turn.DeniedExecutables = slices.Clone(VCSExecutables)
	}
	return turn, nil
}

// bindSet collects binds, one per destination.
type bindSet struct {
	binds []Bind
	index map[string]int
}

// add binds src at dst. A destination bound twice must be bound the same way
// both times.
func (s *bindSet) add(src, dst string, access Access) error {
	for _, p := range []string{src, dst} {
		// --mount is a comma-separated list read as CSV.
		if strings.ContainsAny(p, ",\"\n\r") {
			return fmt.Errorf("%w: path %q cannot be passed to %s's --mount", ErrUnsupported, p, ContainerEngine)
		}
	}
	if i, ok := s.index[dst]; ok {
		if prev := s.binds[i]; prev.Source != src || prev.Access != access {
			return fmt.Errorf("%w: %s would be bound twice (%s %s and %s %s)", ErrUnsupported, dst, prev.Source, prev.Access, src, access)
		}
		return nil
	}
	if s.index == nil {
		s.index = map[string]int{}
	}
	s.index[dst] = len(s.binds)
	s.binds = append(s.binds, Bind{Source: src, Destination: dst, Access: access})
	return nil
}

// mounts binds every granted mount at its real path and at the path it was
// granted by. resolved are the granted mounts with their links resolved.
func (s *bindSet) mounts(granted, resolved []Mount) error {
	for i, m := range granted {
		real := resolved[i]
		if real.Path == string(filepath.Separator) {
			return fmt.Errorf("%w: a container cannot be given the host's root; grant the directories it needs", ErrUnsupported)
		}
		if err := s.add(real.Path, real.Path, real.Access); err != nil {
			return err
		}
		if err := s.add(real.Path, filepath.Clean(m.Path), real.Access); err != nil {
			return err
		}
	}
	return nil
}

// bind builds the turn's binds: every granted mount at its real path and at
// the path it was granted by, every path the session is told about at that
// path too, when a symbolic link makes it differ from its real one, and,
// without VCS, the masks.
func (b ContainerBoundary) bind(req Request, turn *Turn) error {
	set := &bindSet{}
	defer func() { turn.Binds = set.binds }()
	add := set.add
	if err := set.mounts(req.Grants.Mounts, turn.Mounts); err != nil {
		return err
	}
	need := func(what, path string, access Access, resolveFn func(string) (string, error)) error {
		real, err := resolveFn(path)
		if err != nil {
			return fmt.Errorf("%s: %w", what, err)
		}
		m := findMount(turn.Mounts, real)
		if m == nil {
			return fmt.Errorf("%w: %s %s is outside every mount", ErrNotGranted, what, path)
		}
		if access == ReadWrite && m.Access != ReadWrite {
			return fmt.Errorf("%w: %s %s is writable in the container and granted %s", ErrNotGranted, what, path, m.Access)
		}
		alias := filepath.Clean(path)
		if alias == real {
			return nil
		}
		// The alias shows what the real path shows, including the
		// grants inside it.
		if err := add(real, alias, m.Access); err != nil {
			return err
		}
		for _, inner := range turn.Mounts {
			if rel, err := filepath.Rel(real, inner.Path); err == nil && rel != "." && inside(real, inner.Path) {
				if err := add(inner.Path, filepath.Join(alias, rel), inner.Access); err != nil {
					return err
				}
			}
		}
		return nil
	}
	// Read-only when granted so: the engine binds it that way.
	if err := need("working directory", req.workDir(), "", resolve); err != nil {
		return err
	}
	if turn.VCS && req.Workspace != nil {
		if access := req.Workspace.VCS(); access != nil {
			for _, dir := range access.Mounts {
				if err := need("VCS mount", dir, ReadWrite, resolve); err != nil {
					return err
				}
			}
		}
	}
	for _, dir := range b.MountDirs {
		if err := need("mount directory", dir, ReadWrite, resolve); err != nil {
			return err
		}
	}
	switch {
	case req.SessionDir != "":
		if err := need("session directory", req.SessionDir, "", resolve); err != nil {
			return err
		}
	case b.SessionsDir != "":
		// Not created yet: the runner verifies again once it is.
		if err := need("sessions directory", b.SessionsDir, "", resolveCreatable); err != nil {
			return err
		}
	}
	if len(req.Profile.Skills) > 0 {
		for _, dir := range b.SkillMountDirs {
			if err := need("skill directory", dir, "", resolve); err != nil {
				return err
			}
		}
	}
	if !turn.VCS {
		for _, m := range b.Masks {
			if err := add(m.Source, m.Destination, ReadOnly); err != nil {
				return err
			}
		}
	}
	return nil
}

// env builds the container environment from nothing: the agent's own
// credential from the host, when granted (inside there is no keychain to
// hold one), then the session's variables, then HOME.
func (b ContainerBoundary) env(req Request, turn *Turn) ([]string, error) {
	g := req.Grants
	environ := b.Environ
	if environ == nil {
		environ = os.Environ
	}
	host := map[string]string{}
	for _, kv := range environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			host[k] = v
		}
	}
	agent := req.Profile.Agent
	if agent == "" {
		agent = AgentClaude
	}
	var vars []envVar
	// Set before the role's env so a role can name a different one.
	for _, name := range AgentCredentials[agent] {
		if v := host[name]; v != "" && envGranted(g.Env, name) && (turn.VCS || !isVCSEnv(name)) {
			vars = append(vars, envVar{name, v})
		}
	}
	vars = append(vars, sessionVars(req, turn.VCS)...)
	extra := func(m map[string]string) error {
		for _, k := range slices.Sorted(maps.Keys(m)) {
			if !envGranted(g.Env, k) {
				return fmt.Errorf("%w: variable %s is set in the container but not in the environment allowlist", ErrNotGranted, k)
			}
			vars = append(vars, envVar{k, m[k]})
		}
		return nil
	}
	if err := extra(req.ContainerEnv); err != nil {
		return nil, err
	}
	if turn.VCS {
		if err := extra(req.VCSContainerEnv); err != nil {
			return nil, err
		}
	}
	home := b.Home
	if home == "" {
		home = "/home/agent"
	}
	vars = append(vars, envVar{"HOME", home})
	var env []string
	for _, v := range dedupe(vars) {
		env = append(env, v.name+"="+v.value)
	}
	return env, nil
}

// resolveCreatable resolves a path that may not exist yet: its nearest
// existing ancestor has its links resolved and the rest is joined on.
func resolveCreatable(path string) (string, error) {
	if _, err := os.Lstat(path); err == nil || !os.IsNotExist(err) {
		return resolve(path)
	}
	parent := filepath.Dir(path)
	if parent == path {
		return resolve(path)
	}
	real, err := resolveCreatable(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(real, filepath.Base(path)), nil
}
