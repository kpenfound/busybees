package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/procs"
)

// fakeDocker writes a shell script standing in for the docker CLI: `run`
// records its arguments and the client's environment in the session
// directory, writes the cidfile the way docker does, and runs the command
// after the image (a shell pattern) on the host; `network inspect` answers
// with a gateway; `rm` records the call. The engine's images are the lines
// of images.txt beside the script: `image inspect` records the tag it was
// asked about in docker-inspect.txt and succeeds when the tag is listed;
// `build` records its arguments in docker-build.txt, keeps the Dockerfile it
// was given as Dockerfile.built, fails when a file named fail-build is
// beside the script, and lists the tag otherwise.
func fakeDocker(t *testing.T, image string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	script := `#!/bin/sh
set -e
here="$(dirname "$0")"
case "$1" in
network)
  echo 172.17.0.1
  exit 0
  ;;
rm)
  echo "$@" >> "$here/docker-rm.txt"
  exit 0
  ;;
image)
  tag="$5"
  echo "$tag" >> "$here/docker-inspect.txt"
  [ -f "$here/images.txt" ] && grep -qx "$tag" "$here/images.txt"
  exit $?
  ;;
build)
  printf '%s\n' "$@" > "$here/docker-build.txt"
  tag=""
  while [ $# -gt 0 ]; do
    case "$1" in
    --tag) tag="$2"; shift 2 ;;
    --file) cp "$2" "$here/Dockerfile.built"; shift 2 ;;
    *) shift ;;
    esac
  done
  if [ -f "$here/fail-build" ]; then
    echo "ERROR: process \"/bin/sh -c false\" did not complete successfully: exit code: 1" >&2
    exit 1
  fi
  echo "$tag" >> "$here/images.txt"
  exit 0
  ;;
esac
printf '%s\n' "$@" > "$BEES_SESSION_DIR/docker-args.txt"
env > "$BEES_SESSION_DIR/docker-env.txt"
while [ $# -gt 0 ]; do
  case "$1" in
  --cidfile) echo "abc123" > "$2"; shift 2 ;;
  ` + image + `) shift; break ;;
  *) shift ;;
  esac
done
exec "$@"
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// fakeBees stands in for the bees binary a container session's built-in
// server runs as: `mcp serve --listen` records its environment and its pid,
// reports an address and waits to be killed.
func fakeBees(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "bees")
	script := `#!/bin/sh
env > "$BEES_SESSION_DIR/server-env.txt"
echo $$ > "$BEES_SESSION_DIR/server-pid.txt"
printf '%s\n' "$@" > "$BEES_SESSION_DIR/server-args.txt"
echo "listening on 127.0.0.1:45678"
exec sleep 60
`
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// linkedWorktree creates a repository with one commit and a linked worktree
// of it, and returns both directories.
func linkedWorktree(t *testing.T) (repo, worktree string) {
	t.Helper()
	repo = filepath.Join(t.TempDir(), "repo")
	worktree = filepath.Join(t.TempDir(), "wt")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git("init", "-q", "-b", "main")
	git("commit", "-q", "--allow-empty", "-m", "init")
	git("worktree", "add", "-q", worktree, "-b", "work")
	return repo, worktree
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
printf '%s\n' "$@" > "$BEES_SESSION_DIR/args.txt"
cat > "$BEES_SESSION_DIR/stdin.txt"
[ -f "$BEES_SESSION_DIR/container-id" ] && cp "$BEES_SESSION_DIR/container-id" "$BEES_SESSION_DIR/id-while-running"
[ -f "$BEES_SESSION_DIR/mcp-server-pid" ] && cp "$BEES_SESSION_DIR/mcp-server-pid" "$BEES_SESSION_DIR/server-pid-while-running"
echo '{"type":"result","subtype":"success","is_error":false,"result":"boxed","session_id":"abc","num_turns":2,"total_cost_usd":0.1}'
printf '{"status":"pr-opened","pr":7}' > "$BEES_SESSION_DIR/outcome.json"
`)
	repo, worktree := linkedWorktree(t)
	r := newRunner(t, claude)
	r.DockerBin = fakeDocker(t, "ghcr.io/acme/bees:1")
	r.BeesBin = fakeBees(t)
	r.StateDir = t.TempDir()
	r.GitHub = config.GitHub{Login: "bot", Token: "ghp_secret"}
	r.ContainerListen = "127.0.0.1:0"
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 5, Timeout: time.Minute,
		Sandbox: config.SandboxContainer, SandboxImage: "ghcr.io/acme/bees:1",
		Shell: "/bin/sh", Env: map[string]string{"FACTORY_TOKEN": "abc"}}
	res, err := r.Run(context.Background(), Request{Name: "boxed", Role: role, WorkDir: worktree, SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{EnvIssue: "12"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "boxed" || !res.HasOutcome || res.Outcome.PR != 7 {
		t.Fatalf("result: %+v", res)
	}
	dir := res.SessionDir
	realRepo, err := filepath.EvalSymlinks(repo)
	if err != nil {
		t.Fatal(err)
	}
	realWorktree, _ := filepath.EvalSymlinks(worktree)

	args := lines(t, filepath.Join(dir, "docker-args.txt"))
	joined := strings.Join(args, " ")
	for _, want := range []string{
		"run --rm --interactive --name bees-boxed-",
		"--cidfile " + filepath.Join(dir, procs.ContainerIDFile),
		"--label " + procs.ContainerLabel + "=" + dir,
		"--workdir " + worktree,
		"--mount type=tmpfs,destination=/home/bees,tmpfs-mode=1777",
		"--mount type=bind,source=" + worktree + ",destination=" + worktree + " ",
		"--mount type=bind,source=" + worktree + ",destination=" + realWorktree + " ",
		"--mount type=bind,source=" + filepath.Join(realRepo, ".git") + ",destination=" + filepath.Join(realRepo, ".git") + " ",
		"--mount type=bind,source=" + r.StateDir + ",destination=" + r.StateDir + " ",
		"--env HOME=/home/bees",
		"--env GH_TOKEN ", "--env ANTHROPIC_API_KEY ", "--env FACTORY_TOKEN ", "--env SHELL ",
		"--env BEES_SESSION_DIR ", "--env BEES_ISSUE ", "--env GIT_CONFIG_COUNT ",
		" ghcr.io/acme/bees:1 " + claude + " -p ",
	} {
		if !strings.Contains(joined+" ", want) {
			t.Errorf("docker args missing %q:\n%s", want, joined)
		}
	}
	for _, secret := range []string{"ghp_secret", "sk-host", "abc"} {
		if strings.Contains(joined, secret) {
			t.Errorf("docker args carry a value (%q): %s", secret, joined)
		}
	}
	for _, absent := range []string{"--env BEES_BIN", "--env PATH", "--add-host"} {
		if strings.Contains(joined, absent) {
			t.Errorf("docker args carry %q on macOS: %s", absent, joined)
		}
	}
	if want := "--user 1234:5678 "; !strings.Contains(joined, want) {
		t.Errorf("docker args do not run the session as the host user (%q): %s", want, joined)
	}
	// The values travel in the client's environment, bees' own winning.
	clientEnv := lines(t, filepath.Join(dir, "docker-env.txt"))
	for _, want := range []string{"GH_TOKEN=ghp_secret", "ANTHROPIC_API_KEY=sk-host", "FACTORY_TOKEN=abc", "BEES_ROLE=developer", "BEES_ISSUE=12", "GIT_CONFIG_COUNT=7"} {
		if !slices.Contains(clientEnv, want) {
			t.Errorf("engine client env missing %s", want)
		}
	}
	// The client keeps the host's HOME (docker reads ~/.docker/config.json
	// for its context) and never sees the container's.
	if slices.Contains(clientEnv, "HOME=/home/bees") {
		t.Error("the engine client was given the container's HOME")
	}
	if !slices.Contains(clientEnv, "HOME=/Users/operator") {
		t.Error("the engine client lost its own HOME")
	}

	// The built-in server ran on the host with the session's environment
	// and a token, which is what the session's mcp.json carries.
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
	for _, want := range []string{"BEES_ROLE=developer", "BEES_SESSION_DIR=" + dir, "BEES_ISSUE=12", "GH_TOKEN=ghp_secret"} {
		if !slices.Contains(serverEnv, want) {
			t.Errorf("server env missing %s", want)
		}
	}
	if got := strings.Join(lines(t, filepath.Join(dir, "server-args.txt")), " "); got != "mcp serve --listen 127.0.0.1:0" {
		t.Errorf("server args: %q", got)
	}
	builtin := readMCPConfig(t, dir)[config.BuiltinMCPServer]
	if builtin.Type != "http" || builtin.URL != "http://host.docker.internal:45678/mcp" || builtin.Command != "" {
		t.Errorf("built-in server entry: %+v", builtin)
	}
	if builtin.Headers["Authorization"] != "Bearer "+token {
		t.Errorf("built-in server header: %q, token %q", builtin.Headers["Authorization"], token)
	}
	if len(builtin.Env) != 0 {
		t.Errorf("built-in server entry carries env: %v", builtin.Env)
	}
	// The server is stopped with the session.
	pid, _ := strconv.Atoi(strings.TrimSpace(lines(t, filepath.Join(dir, "server-pid.txt"))[0]))
	// Its pid was recorded while it ran, so a crash that stops bees before
	// it can kill the server leaves `bees kill` something to find it by.
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
func TestContainerVarsCarryNothingOfTheHost(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-host")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("HOST_ONLY", "leak")
	r := newRunner(t, "claude")
	r.GitHub = config.GitHub{Login: "bot", Token: "ghp_x", GitName: "Bot", GitEmail: "bot@example.com"}
	role := config.ResolvedRole{Name: "qa", Sandbox: config.SandboxContainer, SandboxImage: "img", Shell: "/bin/bash",
		Env: map[string]string{"NPM_TOKEN": "$HOST_ONLY"}}
	vars := dedupe(r.containerVars(Request{Name: "q", Role: role, Env: map[string]string{EnvPR: "3"}}, "/sessions/q"))
	got := map[string]string{}
	var names []string
	for _, v := range vars {
		got[v.name] = v.value
		names = append(names, v.name)
	}
	for k, want := range map[string]string{
		"ANTHROPIC_API_KEY": "sk-host", "NPM_TOKEN": "leak", "SHELL": "/bin/bash", "BEES_ROLE": "qa", "BEES_PR": "3",
		"GH_TOKEN": "ghp_x", "GIT_AUTHOR_NAME": "Bot", "GIT_COMMITTER_EMAIL": "bot@example.com", "HOME": "/home/bees",
	} {
		if got[k] != want {
			t.Errorf("%s = %q, want %q", k, got[k], want)
		}
	}
	for _, absent := range []string{"HOST_ONLY", "PATH", "USER", "BEES_BIN", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := got[absent]; ok {
			t.Errorf("%s reached the container", absent)
		}
	}
	if names[len(names)-1] != "HOME" {
		t.Errorf("HOME is not last: %v", names)
	}
	count, _ := strconv.Atoi(got["GIT_CONFIG_COUNT"])
	var entries []string
	for i := range count {
		entries = append(entries, got["GIT_CONFIG_KEY_"+strconv.Itoa(i)]+"="+got["GIT_CONFIG_VALUE_"+strconv.Itoa(i)])
	}
	for _, want := range []string{"credential.helper=!gh auth git-credential", "safe.directory=*", "url.https://github.com/.insteadOf=git@github.com:", "url.https://github.com/.insteadOf=ssh://git@github.com/", "push.autoSetupRemote=true"} {
		if !slices.Contains(entries, want) {
			t.Errorf("git config missing %s: %v", want, entries)
		}
	}
	// A role's env may name the credential itself, and then it is the one
	// the container gets.
	role.Env = map[string]string{"ANTHROPIC_API_KEY": "sk-role"}
	for _, v := range dedupe(r.containerVars(Request{Name: "q", Role: role}, "/sessions/q")) {
		if v.name == "ANTHROPIC_API_KEY" && v.value != "sk-role" {
			t.Errorf("role env did not replace the forwarded credential: %q", v.value)
		}
	}
}

// The container runs as the host's user everywhere (claude refuses to skip
// permissions as root, and on Linux what the session writes must be the
// host user's); only Linux is told how to reach the host.
func TestContainerCommandPerOS(t *testing.T) {
	r := newRunner(t, "claude")
	role := config.ResolvedRole{Name: "developer", Sandbox: config.SandboxContainer, SandboxImage: "img"}
	c := &container{r: r, req: Request{Name: "d", Role: role, WorkDir: t.TempDir()}, sessionDir: t.TempDir(), name: "bees-d-1", image: role.SandboxImage}
	c.vars = r.containerVars(c.req, c.sessionDir)
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

// A container role missing what the box needs is refused before anything
// starts, naming the missing thing, by the runner too: `bees exec` and
// `bees tick` do not ask config.CheckSandbox.
func TestContainerSessionRefusedWithoutWhatItNeeds(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	claude := fakeClaude(t, `touch "$BEES_SESSION_DIR/ran"; echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'`)
	for _, tc := range []struct {
		name string
		role config.ResolvedRole
		gh   config.GitHub
		want string
	}{
		{"no image", config.ResolvedRole{Name: "developer", Sandbox: config.SandboxContainer}, config.GitHub{Login: "bot", Token: "t"}, "sandbox_image"},
		{"no github", config.ResolvedRole{Name: "developer", Sandbox: config.SandboxContainer, SandboxImage: "img"}, config.GitHub{}, "[github]"},
		{"no credential", config.ResolvedRole{Name: "developer", Sandbox: config.SandboxContainer, SandboxImage: "img"}, config.GitHub{Login: "bot", Token: "t"}, "ANTHROPIC_API_KEY"},
	} {
		r := newRunner(t, claude)
		r.DockerBin = fakeDocker(t, "img")
		r.BeesBin = fakeBees(t)
		r.GitHub = tc.gh
		_, err := r.Run(context.Background(), Request{Name: "boxed", Role: tc.role, WorkDir: t.TempDir()})
		if err == nil {
			t.Fatalf("%s: a container session ran", tc.name)
		}
		for _, want := range []string{"developer", tc.want} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not mention %q", tc.name, err, want)
			}
		}
		entries, _ := os.ReadDir(r.SessionsDir)
		for _, e := range entries {
			if _, err := os.Stat(filepath.Join(r.SessionsDir, e.Name(), "ran")); err == nil {
				t.Errorf("%s: claude ran", tc.name)
			}
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
	r.BeesBin = fakeBees(t)
	r.GitHub = config.GitHub{Login: "bot", Token: "t"}
	r.ContainerListen = "127.0.0.1:0"
	role := config.ResolvedRole{Name: "qa", Model: "opus", MaxTurns: 1, Timeout: 200 * time.Millisecond, Sandbox: config.SandboxContainer, SandboxImage: "img"}
	res, err := r.Run(context.Background(), Request{Name: "slow", Role: role, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("result: %+v", res)
	}
	rm, err := os.ReadFile(filepath.Join(filepath.Dir(r.DockerBin), "docker-rm.txt"))
	if err != nil || !strings.HasPrefix(string(rm), "rm --force bees-slow-") {
		t.Errorf("container not removed: %q, %v", rm, err)
	}
}
