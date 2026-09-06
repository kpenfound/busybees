package session

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// A container session (config.SandboxContainer) runs the backend's command
// line unchanged inside `docker run`, with these differences from a session
// on the host:
//
//   - The container sees three things of the host, each bind-mounted at its
//     host path so every path in the prompts, the environment and mcp.json
//     means the same inside: the worktree, the repository's .git (a linked
//     worktree's .git file points into it, and commits write there) and the
//     state directory (mail, notes, the session directory). A role with
//     skills also gets the skills cache, read-only. Nothing else: no home
//     directory, no other checkout, no credential store.
//   - It runs as the host's user, with a HOME of its own on a tmpfs.
//   - Its environment is built from nothing rather than from the host's:
//     the role's env and shell, the BEES_* variables, the [github] token
//     and git identity, the git configuration a session runs with, the
//     agent's own credential forwarded from the host (config.AgentCredentials)
//     and that HOME. Values are handed to the engine by name, never on its
//     command line.
//   - The bees binary is not in the container, so the built-in MCP server
//     runs on the host — `bees mcp serve --listen`, with the environment a
//     session on the host would have given it — and the session reaches it
//     over HTTP at containerHostAlias with a bearer token of its own; the
//     `bees` commands are not available inside. A configured stdio MCP
//     server starts inside the container and must be in the image; a
//     remote one is reached as configured.
//   - The engine writes the container's id to ContainerIDFile in the session
//     directory, and the container is named after the session and labelled
//     with the session directory, so a kill can find it. Stopping the
//     session removes the container.
//
// The image (sandbox_image) must hold the agent, git and gh; nothing of the
// host's toolchain is available inside.

// ContainerIDFile is the file in a container session's directory the engine
// writes the container's id to when it starts the container (--cidfile).
// It is removed when the session ends, like the pid file, so a session
// directory holding one is a session whose container may still be running.
const ContainerIDFile = "container-id"

// ContainerLabel is the label every container session's container carries,
// with the session directory as its value, so `docker ps --filter
// label=ContainerLabel` lists this factory's sessions and the value says
// which each one is.
const ContainerLabel = "bees.session"

// containerHome is the session's home directory inside the container: a
// tmpfs, so neither the image's home directory nor anything of the host's
// reaches the session, and nothing the agent writes under ~ survives it.
const containerHome = "/home/bees"

// containerHostAlias is the name the container reaches the host by. Docker
// Desktop resolves it on its own; on Linux the container is started with
// --add-host so it does.
const containerHostAlias = "host.docker.internal"

// containerGitConfig is added to gitConfig for a session in a container.
// The mounted repository is owned by the host's user, which the container's
// may not be, so it is trusted explicitly; and the container has no ssh
// keys or agent, so an ssh remote is pushed to over https, where the
// [github] token works.
var containerGitConfig = []envVar{
	{"safe.directory", "*"},
	{"url.https://github.com/.insteadOf", "git@github.com:"},
	{"url.https://github.com/.insteadOf", "ssh://git@github.com/"},
}

// hostOS, hostUID and hostGID describe the machine to the container code,
// as variables so a test can describe one it is not running on.
var (
	hostOS  = runtime.GOOS
	hostUID = os.Getuid
	hostGID = os.Getgid
)

// serverStart is how long the built-in server has to report its address.
const serverStart = 15 * time.Second

// container is one session's box: the engine command that runs it, the
// built-in server on the host it talks to, and what it is given.
type container struct {
	r          *Runner
	req        Request
	sessionDir string
	// name is the container's name (bees-<session name>-<random>).
	name string
	// server is the built-in MCP server on the host, and builtin the entry
	// the session reaches it through.
	server  *exec.Cmd
	builtin MCPEntry
	// vars is the session's environment inside the container.
	vars []envVar
}

// startContainer prepares a container session: it starts the built-in
// server on the host and builds the session's environment. The container
// itself is started by Run, through command; close stops the server.
func (r *Runner) startContainer(ctx context.Context, req Request, sessionDir string) (*container, error) {
	if err := config.CheckSandboxContainer(req.Role, r.GitHub); err != nil {
		return nil, err
	}
	c := &container{r: r, req: req, sessionDir: sessionDir, name: "bees-" + sanitize(req.Name) + "-" + randomHex(4)}
	c.vars = r.containerVars(req, sessionDir)
	if err := c.startServer(ctx); err != nil {
		return nil, err
	}
	return c, nil
}

// startServer runs `bees mcp serve --listen` on the host with the
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
	cmd := exec.Command(r.beesBin(), "mcp", "serve", "--listen", addr)
	cmd.Dir = c.req.WorkDir
	cmd.Env = append(r.env(c.req, c.sessionDir), EnvMCPToken+"="+token)
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
	reported, ok := strings.CutPrefix(listening, MCPListening)
	if !ok {
		c.close()
		return fmt.Errorf("the built-in MCP server did not report its address (got %q); see %s", listening, filepath.Join(c.sessionDir, "mcp-server.log"))
	}
	_, port, err := net.SplitHostPort(reported)
	if err != nil {
		c.close()
		return fmt.Errorf("the built-in MCP server reported %q: %w", reported, err)
	}
	c.builtin = MCPEntry{
		Type:    "http",
		URL:     "http://" + net.JoinHostPort(containerHostAlias, port) + "/mcp",
		Headers: map[string]string{"Authorization": "Bearer " + token},
	}
	return nil
}

// containerListen is the address the built-in server listens on for a
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
		out, err := exec.CommandContext(ctx, r.dockerBin(), "network", "inspect", "bridge", "--format", "{{(index .IPAM.Config 0).Gateway}}").Output()
		if err != nil {
			return "", fmt.Errorf("find the address the container reaches the host by (%s network inspect bridge): %w", config.ContainerEngine, err)
		}
		gw := strings.TrimSpace(string(out))
		if net.ParseIP(gw) == nil {
			return "", fmt.Errorf("%s network inspect bridge reported %q, not an address", config.ContainerEngine, gw)
		}
		return net.JoinHostPort(gw, "0"), nil
	default:
		return "", fmt.Errorf("sandbox %q runs on macOS and Linux only, not %s", config.SandboxContainer, hostOS)
	}
}

// containerVars is the session's environment inside the container, built
// from nothing: the host's variables stay on the host. BEES_BIN is left
// out because the binary is not inside, and HOME is the tmpfs the container
// is given.
func (r *Runner) containerVars(req Request, sessionDir string) []envVar {
	var vars []envVar
	agent := req.Role.Agent
	if agent == "" {
		agent = config.AgentClaude
	}
	// The agent's own credential, forwarded from the host when it is
	// there: inside there is no keychain to hold one. Set before the
	// role's env so a role can name a different one.
	for _, name := range config.AgentCredentials[agent] {
		if v := os.Getenv(name); v != "" {
			vars = append(vars, envVar{name, v})
		}
	}
	for _, v := range r.sessionVars(req, sessionDir) {
		if v.name != EnvBin {
			vars = append(vars, v)
		}
	}
	vars = append(vars, gitConfigVars(append(r.gitConfig(), containerGitConfig...))...)
	vars = append(vars, envVar{"HOME", containerHome})
	return vars
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
		"--cidfile", filepath.Join(c.sessionDir, ContainerIDFile),
		"--label", ContainerLabel + "=" + c.sessionDir,
		"--workdir", req.WorkDir,
		"--mount", "type=tmpfs,destination=" + containerHome + ",tmpfs-mode=1777",
	}
	mounts, err := c.mounts(ctx)
	if err != nil {
		return "", nil, err
	}
	out = append(out, mounts...)
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
	out = append(out, req.Role.SandboxImage, bin)
	out = append(out, args...)
	return r.dockerBin(), out, nil
}

// mounts are the bind mounts the container gets, each at its host path.
// A path that goes through a symbolic link is mounted at its real path as
// well: git records real paths in a linked worktree's pointers (on macOS
// the temp directory the worktrees live under is one, /var -> /private/var),
// while the prompts and the environment name the path as bees knows it,
// and both must resolve inside.
func (c *container) mounts(ctx context.Context) ([]string, error) {
	r, req := c.r, c.req
	bind := func(path string, ro bool) []string {
		var out []string
		dests := []string{path}
		if real, err := filepath.EvalSymlinks(path); err == nil && real != path {
			dests = append(dests, real)
		}
		for _, dst := range dests {
			spec := "type=bind,source=" + path + ",destination=" + dst
			if ro {
				spec += ",readonly"
			}
			out = append(out, "--mount", spec)
		}
		return out
	}
	var out []string
	out = append(out, bind(req.WorkDir, false)...)
	if gitDir, err := commonGitDir(ctx, req.WorkDir); err == nil && !within(gitDir, req.WorkDir) {
		out = append(out, bind(gitDir, false)...)
	}
	if r.StateDir != "" {
		out = append(out, bind(r.StateDir, false)...)
	}
	if r.Skills != nil && len(req.Role.Skills) > 0 {
		out = append(out, bind(r.Skills.CacheDir, true)...)
	}
	return out, nil
}

// commonGitDir is the repository directory a worktree's git operations
// write to: the repository's own .git for a linked worktree, which is
// outside the worktree and must be mounted with it. Git reports it as a
// real path, which is also how the worktree's .git file names it.
func commonGitDir(ctx context.Context, workDir string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", workDir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return "", err
	}
	return filepath.Clean(strings.TrimSpace(string(out))), nil
}

// within reports whether path is dir or inside it, either as given or as
// a real path.
func within(path, dir string) bool {
	if real, err := filepath.EvalSymlinks(dir); err == nil {
		dir = real
	}
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, "../")
}

// clientEnv is the environment the engine client runs with: the host's,
// with the session's variables laid over it so the engine reads their
// values by name. Each name appears once, holding the value set last: the
// engine reads a name's first occurrence, and a role's env must not win
// over bees' own variables here when it does not on the host. HOME is the
// exception (see command).
func (c *container) clientEnv() []string {
	vars := dedupe(c.vars)
	names := map[string]bool{}
	for _, v := range vars {
		names[v.name] = true
	}
	var env []string
	for _, kv := range hostEnv() {
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
	_ = exec.CommandContext(ctx, c.r.dockerBin(), "rm", "--force", c.name).Run()
}

// close stops the built-in server and forgets the container id: the
// container is gone (--rm) or removed.
func (c *container) close() {
	if c.server != nil && c.server.Process != nil {
		_ = syscall.Kill(-c.server.Process.Pid, syscall.SIGKILL)
		_ = c.server.Wait()
		c.server = nil
	}
	_ = os.Remove(filepath.Join(c.sessionDir, ContainerIDFile))
}

func (r *Runner) dockerBin() string {
	if r.DockerBin != "" {
		return r.DockerBin
	}
	return config.ContainerEngine
}

// beesBin is the bees executable: BeesBin, else this very binary.
func (r *Runner) beesBin() string {
	if r.BeesBin != "" {
		return r.BeesBin
	}
	if self, err := os.Executable(); err == nil {
		return self
	}
	return "bees"
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
