package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/agent/procs"
	"github.com/kpenfound/busybees/core/vcs"
)

// fakeDocker writes a shell script standing in for the docker CLI: `run`
// records its arguments and the client's environment in the session
// directory, writes the cidfile the way docker does, and runs the command
// after the image (a shell pattern) on the host; `network inspect` answers
// with a gateway; `rm` records the call. The engine's images are the lines
// of images.txt beside the script: `image inspect` records the tag it was
// asked about in docker-inspect.txt and succeeds when the tag is listed;
// `build` records its arguments in docker-build.txt, keeps the Dockerfile it
// was given as Dockerfile.built, prints a progress line, fails when a file
// named fail-build is beside the script, and lists the tag otherwise.
func fakeDocker(t *testing.T, image string) string {
	return agenttest.Docker(t, image, "TASK_SESSION_DIR")
}

// fakeBees stands in for the task binary a container session's built-in
// server runs as: `mcp serve --listen` records its environment and its pid,
// reports an address and waits to be killed.
func fakeBees(t *testing.T) string { return agenttest.MCPServer(t, "TASK_SESSION_DIR") }

// fakeWorkspace has no git layout and supplies metadata explicitly.
type fakeWorkspace struct {
	dir    string
	access *vcs.Access
}

func (w fakeWorkspace) Directory() string { return w.dir }
func (w fakeWorkspace) VCS() *vcs.Access  { return w.access }

func workspaceFixture(t *testing.T) (metadata, directory string) {
	t.Helper()
	return t.TempDir(), t.TempDir()
}

// verifiedContainer is the container Run would build for req: the request
// verified by the runner's container boundary, nothing started.
func verifiedContainer(t *testing.T, r *Runner, req Request, sessionDir string) (*container, error) {
	t.Helper()
	req.Profile.Sandbox = SandboxContainer
	if req.Profile.SandboxImage == "" {
		req.Profile.SandboxImage = "image"
	}
	if req.SessionDir == "" {
		req.SessionDir = sessionDir
	}
	turn, err := r.Verify(req)
	if err != nil {
		return nil, err
	}
	return &container{r: r, req: req, sessionDir: sessionDir, turn: turn, name: "task-d-1", image: req.Profile.SandboxImage, vars: turnVars(turn)}, nil
}

func lines(t *testing.T, path string) []string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
}

// A container session is the claude command inside `docker run`, with the
// worktree, the repository's .git and the state directory mounted at their
// host paths, the session's variables handed to the engine by name, the
// built-in server on the host reached over HTTP with the session's token,
// and the container's id recorded in the session directory for as long as
// the session runs.
func TestContainerSessionRunsInsideTheEngine(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-host")
	t.Setenv("HOME", "/Users/operator")
	fakeContainerHost(t, "darwin")
	claude := fakeClaude(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
cat > "$TASK_SESSION_DIR/stdin.txt"
[ -f "$TASK_SESSION_DIR/container-id" ] && cp "$TASK_SESSION_DIR/container-id" "$TASK_SESSION_DIR/id-while-running"
[ -f "$TASK_SESSION_DIR/mcp-server-pid" ] && cp "$TASK_SESSION_DIR/mcp-server-pid" "$TASK_SESSION_DIR/server-pid-while-running"
echo '{"type":"result","subtype":"success","is_error":false,"result":"boxed","session_id":"abc","num_turns":2,"total_cost_usd":0.1}'
printf '{"status":"submitted","work":{"key":"task/7","tags":{"ticket":"seven"}}}' > "$TASK_SESSION_DIR/outcome.json"
`)
	metadata, worktree := workspaceFixture(t)
	metadata, _ = filepath.EvalSymlinks(metadata)
	r := newRunner(t, claude)
	r.DockerBin = fakeDocker(t, "ghcr.io/acme/task:1")
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	r.ContainerListen = "127.0.0.1:0"
	role := Profile{Name: "builder", Model: "opus", MaxTurns: 5, Timeout: time.Minute,
		Sandbox: SandboxContainer, SandboxImage: "ghcr.io/acme/task:1",
		Shell: "/bin/sh", Env: map[string]string{"FACTORY_TOKEN": "abc"}}
	res, err := r.Run(context.Background(), Request{Name: "boxed", Profile: role, Workspace: fakeWorkspace{dir: worktree, access: &vcs.Access{Mounts: []string{metadata}}}, SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{EnvIssue: "12"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "boxed" || !res.HasOutcome || res.Outcome.Work.Key != "task/7" || res.Outcome.Work.Tags["ticket"] != "seven" {
		t.Fatalf("result: %+v", res)
	}
	dir := res.SessionDir
	realRepo, err := filepath.EvalSymlinks(metadata)
	if err != nil {
		t.Fatal(err)
	}
	realWorktree, _ := filepath.EvalSymlinks(worktree)
	realState, _ := filepath.EvalSymlinks(r.StateDir)

	args := lines(t, filepath.Join(dir, "docker-args.txt"))
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --rm --interactive --name task-boxed-",
		"--cidfile " + filepath.Join(dir, procs.ContainerIDFile),
		"--label " + r.ContainerLabel + "=" + dir,
		"--workdir " + worktree,
		"--mount type=tmpfs,destination=/home/task,tmpfs-mode=1777",
		"--mount type=bind,source=" + realWorktree + ",destination=" + worktree + " ",
		"--mount type=bind,source=" + realWorktree + ",destination=" + realWorktree + " ",
		"--mount type=bind,source=" + realRepo + ",destination=" + realRepo + " ",
		"--mount type=bind,source=" + realState + ",destination=" + r.StateDir + " ",
		"--mount type=bind,source=" + realState + ",destination=" + realState + " ",
		"--env HOME=/home/task",
		"--env ACCESS_TOKEN ", "--env ANTHROPIC_API_KEY ", "--env FACTORY_TOKEN ", "--env SHELL ",
		"--env TASK_SESSION_DIR ", "--env TASK_ISSUE ",
		" ghcr.io/acme/task:1 " + claude + " -p ",
	} {
		if !strings.Contains(joined+" ", want) {
			t.Errorf("docker args missing %q:\n%s", want, joined)
		}
	}
	for _, secret := range []string{"secret", "sk-host", "abc"} {
		if strings.Contains(joined, secret) {
			t.Errorf("docker args carry a value (%q): %s", secret, joined)
		}
	}
	for _, absent := range []string{"--env TASK_BIN", "--env PATH", "--add-host"} {
		if strings.Contains(joined, absent) {
			t.Errorf("docker args carry %q on macOS: %s", absent, joined)
		}
	}
	if want := "--user 1234:5678 "; !strings.Contains(joined, want) {
		t.Errorf("docker args do not run the session as the host user (%q): %s", want, joined)
	}
	// The values travel in the client's environment, task' own winning.
	clientEnv := lines(t, filepath.Join(dir, "docker-env.txt"))
	for _, want := range []string{"ACCESS_TOKEN=secret", "ANTHROPIC_API_KEY=sk-host", "FACTORY_TOKEN=abc", "TASK_ROLE=builder", "TASK_ISSUE=12"} {
		if !slices.Contains(clientEnv, want) {
			t.Errorf("engine client env missing %s", want)
		}
	}
	// The client keeps the host's HOME (docker reads ~/.docker/config.json
	// for its context) and never sees the container's.
	if slices.Contains(clientEnv, "HOME=/home/task") {
		t.Error("the engine client was given the container's HOME")
	}
	if !slices.Contains(clientEnv, "HOME=/Users/operator") {
		t.Error("the engine client lost its own HOME")
	}

	// The built-in server ran on the host with the session's environment
	// and a token, referenced through the environment in mcp.json.
	serverEnv := lines(t, filepath.Join(dir, "server-env.txt"))
	var token string
	for _, kv := range serverEnv {
		if v, ok := strings.CutPrefix(kv, EnvMCPToken+"="); ok {
			token = v
		}
	}
	if len(token) != 64 {
		t.Fatalf("server token: %q", token)
	}
	for _, want := range []string{"TASK_ROLE=builder", "TASK_SESSION_DIR=" + dir, "TASK_ISSUE=12", "ACCESS_TOKEN=secret"} {
		if !slices.Contains(serverEnv, want) {
			t.Errorf("server env missing %s", want)
		}
	}
	if got := strings.Join(lines(t, filepath.Join(dir, "server-args.txt")), " "); got != "mcp serve --listen 127.0.0.1:0" {
		t.Errorf("server args: %q", got)
	}
	builtin := readMCPConfig(t, dir)["tools"]
	if builtin.Type != "http" || builtin.URL != "http://host.docker.internal:45678/mcp" || builtin.Command != "" {
		t.Errorf("built-in server entry: %+v", builtin)
	}
	if builtin.Headers["Authorization"] != "Bearer ${"+EnvMCPToken+"}" {
		t.Errorf("built-in server header: %q, token %q", builtin.Headers["Authorization"], token)
	}
	if len(builtin.Env) != 0 {
		t.Errorf("built-in server entry carries env: %v", builtin.Env)
	}
	// The server is stopped with the session.
	pid, _ := strconv.Atoi(strings.TrimSpace(lines(t, filepath.Join(dir, "server-pid.txt"))[0]))
	// Its pid was recorded while it ran, so a crash that stops task before
	// it can kill the server leaves `task kill` something to find it by.
	if got := lines(t, filepath.Join(dir, "server-pid-while-running"))[0]; got != strconv.Itoa(pid) {
		t.Errorf("%s while running: %q, want the server's pid %d", procs.ServerPIDFile, got, pid)
	}
	deadline := time.Now().Add(5 * time.Second)
	for syscall.Kill(pid, 0) == nil && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if syscall.Kill(pid, 0) == nil {
		t.Errorf("the built-in server (pid %d) outlived the session", pid)
	}

	// The container id was there while the session ran and is gone after.
	if b, err := os.ReadFile(filepath.Join(dir, "id-while-running")); err != nil || strings.TrimSpace(string(b)) != "abc123" {
		t.Errorf("container id while running: %q, %v", b, err)
	}
	if _, err := os.Stat(filepath.Join(dir, procs.ContainerIDFile)); err == nil {
		t.Error("container id file left behind after the session")
	}
	if _, err := os.Stat(filepath.Join(dir, procs.ServerPIDFile)); err == nil {
		t.Error("server pid file left behind after the session")
	}
	// The command inside is the ordinary one, with the prompt on stdin.
	claudeArgs := strings.Join(lines(t, filepath.Join(dir, "args.txt")), " ")
	for _, want := range []string{"--dangerously-skip-permissions", "--append-system-prompt-file " + filepath.Join(dir, "system-prompt.md"), "--mcp-config " + filepath.Join(dir, "mcp.json")} {
		if !strings.Contains(claudeArgs, want) {
			t.Errorf("claude args missing %q: %s", want, claudeArgs)
		}
	}
	if stdin, _ := os.ReadFile(filepath.Join(dir, "stdin.txt")); string(stdin) != "TASK" {
		t.Errorf("stdin: %q", stdin)
	}
}

// The session's environment inside the container is built from nothing:
// the host's variables stay out, the agent's credential is forwarded, a
// role's env may replace it, git trusts the mounted repository and pushes an
// ssh remote over https, and HOME is the container's own.

// The container runs as the host's user everywhere (claude refuses to skip
// permissions as root, and on Linux what the session writes must be the
// host user's); only Linux is told how to reach the host.
func TestContainerCommandPerOS(t *testing.T) {
	r := newRunner(t, "claude")
	role := Profile{Name: "builder", Sandbox: SandboxContainer, SandboxImage: "img", VCSAccess: true}
	sessionDir := t.TempDir()
	c, err := verifiedContainer(t, r.Runner, grantAll(Request{Name: "d", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, SessionDir: sessionDir}), sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		goos    string
		present []string
		absent  []string
	}{
		{"darwin", []string{"--user 1234:5678"}, []string{"--add-host"}},
		{"linux", []string{"--user 1234:5678", "--add-host host.docker.internal:host-gateway"}, nil},
	} {
		fakeContainerHost(t, tc.goos)
		bin, args, err := c.command(context.Background(), "claude", []string{"-p"})
		if err != nil {
			t.Fatal(err)
		}
		if bin != "docker" {
			t.Errorf("%s: engine %q", tc.goos, bin)
		}
		joined := strings.Join(args, " ")
		for _, want := range tc.present {
			if !strings.Contains(joined, want) {
				t.Errorf("%s: args missing %q: %s", tc.goos, want, joined)
			}
		}
		for _, absent := range tc.absent {
			if strings.Contains(joined, absent) {
				t.Errorf("%s: args carry %q: %s", tc.goos, absent, joined)
			}
		}
		if !strings.HasSuffix(joined, " img claude -p") {
			t.Errorf("%s: command does not end with the image and the command: %s", tc.goos, joined)
		}
	}
}

// fakeContainerHost describes a machine to the container code for the rest
// of the test.
func fakeContainerHost(t *testing.T, goos string) {
	t.Helper()
	oldOS, oldUID, oldGID := hostOS, hostUID, hostGID
	t.Cleanup(func() { hostOS, hostUID, hostGID = oldOS, oldUID, oldGID })
	hostOS = goos
	hostUID = func() int { return 1234 }
	hostGID = func() int { return 5678 }
}

// The built-in server listens where the container can reach the host: the
// loopback on macOS, the bridge gateway on Linux (asked of the engine), a
// configured address anywhere, and nowhere else.
func TestContainerListenPerOS(t *testing.T) {
	r := newRunner(t, "claude")
	r.DockerBin = fakeDocker(t, "img")
	t.Setenv(EnvSessionDir, t.TempDir())
	for _, tc := range []struct {
		goos, configured, want string
		wantErr                bool
	}{
		{"darwin", "", "127.0.0.1:0", false},
		{"linux", "", "172.17.0.1:0", false},
		{"windows", "", "", true},
		{"windows", "127.0.0.1:0", "127.0.0.1:0", false},
	} {
		fakeContainerHost(t, tc.goos)
		r.ContainerListen = tc.configured
		got, err := r.containerListen(context.Background())
		if tc.wantErr {
			if err == nil {
				t.Errorf("%s: listened on %q", tc.goos, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%s (configured %q): got %q, %v; want %q", tc.goos, tc.configured, got, err, tc.want)
		}
	}
}

// A session that is stopped — its timeout here — has its container removed,
// not only its engine client killed: the container outlives the client.
func TestStoppedContainerSessionIsRemoved(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk")
	claude := fakeClaude(t, `sleep 5`)
	r := newRunner(t, claude)
	r.DockerBin = fakeDocker(t, "img")
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	r.ContainerListen = "127.0.0.1:0"
	role := Profile{Name: "auditor", Model: "opus", MaxTurns: 1, Timeout: 200 * time.Millisecond, Sandbox: SandboxContainer, SandboxImage: "img"}
	res, err := r.Run(context.Background(), Request{Name: "slow", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("result: %+v", res)
	}
	rm, err := os.ReadFile(filepath.Join(filepath.Dir(r.DockerBin), "docker-rm.txt"))
	if err != nil || !strings.HasPrefix(string(rm), "rm --force task-slow-") {
		t.Errorf("container not removed: %q, %v", rm, err)
	}
}

// opencode is authenticated through whichever provider it is configured
// for, so every provider key it reads is a credential: the ones the host
// has are forwarded into the container by name, and the rest are not there
// to forward.
func TestContainerForwardsOpenCodeProviderCredential(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "or-host")
	t.Setenv("DEEPSEEK_API_KEY", "")
	fakeContainerHost(t, "linux")
	r := newRunner(t, "opencode")
	sessionDir := t.TempDir()
	role := Profile{Name: "builder", Agent: AgentOpenCode, Sandbox: SandboxContainer, SandboxImage: "img"}
	c, err := verifiedContainer(t, r.Runner, grantAll(Request{Name: "d", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, SessionDir: sessionDir}), sessionDir)
	if err != nil {
		t.Fatal(err)
	}
	_, args, err := c.command(context.Background(), "opencode", []string{"run"})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(args, " ") + " "
	if !strings.Contains(joined, "--env OPENROUTER_API_KEY ") {
		t.Errorf("the provider credential was not forwarded: %s", joined)
	}
	if strings.Contains(joined, "or-host") {
		t.Errorf("docker args carry the credential's value: %s", joined)
	}
	if strings.Contains(joined, "--env DEEPSEEK_API_KEY ") {
		t.Errorf("an unset provider key was forwarded: %s", joined)
	}
}
