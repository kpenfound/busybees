package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// containerUseFixture is a definition with every field container-use
// writes, the three bees ignores among them, and commands and values a
// naive rendering would break on.
const containerUseFixture = `{
  "workdir": "/workdir",
  "base_image": "ghcr.io/acme/base:1",
  "setup_commands": [
    "apt-get update && apt-get install -y git gh",
    "npm install -g @anthropic-ai/claude-code"
  ],
  "install_commands": [
    "echo \"it's built\" > /etc/motd"
  ],
  "env": ["NODE_ENV=production", "GREETING=hello \"world\""],
  "secrets": ["NPM_TOKEN=env://NPM_TOKEN"],
  "services": [{"name": "db", "image": "postgres:16", "exposed_ports": [5432]}]
}
`

// containerUseFixtureDockerfile is what containerUseFixture renders to:
// FROM, the env, then setup before install, each command through sh -c, and
// nothing for workdir, secrets or services.
const containerUseFixtureDockerfile = `FROM ghcr.io/acme/base:1
ENV NODE_ENV="production"
ENV GREETING="hello \"world\""
RUN ["sh","-c","apt-get update && apt-get install -y git gh"]
RUN ["sh","-c","npm install -g @anthropic-ai/claude-code"]
RUN ["sh","-c","echo \"it's built\" > /etc/motd"]
`

// writeContainerUseEnvironment puts a definition where container-use keeps
// one, under dir inside the worktree.
func writeContainerUseEnvironment(t *testing.T, worktree, dir, body string) string {
	t.Helper()
	path := filepath.Join(worktree, dir, containerUseDir, containerUseFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// containerUseRunner is a runner for a container session whose role sets
// container_use_environment and no sandbox_image, with the fakes a
// container session needs.
func containerUseRunner(t *testing.T, claudeBody string) (*Runner, config.ResolvedRole) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-host")
	fakeContainerHost(t, "darwin")
	r := newRunner(t, fakeClaude(t, claudeBody))
	r.DockerBin = fakeDocker(t, containerUseRepo+":*")
	r.BeesBin = fakeBees(t)
	r.StateDir = t.TempDir()
	r.GitHub = config.GitHub{Login: "bot", Token: "ghp_secret"}
	r.ContainerListen = "127.0.0.1:0"
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 5, Timeout: time.Minute,
		Sandbox: config.SandboxContainer, ContainerUseEnvironment: "envs/dev"}
	return r, role
}

const containerUseClaude = `
printf '%s\n' "$@" > "$BEES_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"built","session_id":"abc","num_turns":2,"total_cost_usd":0.1}'
`

// A role with container_use_environment runs in an image built from the
// definition in the worktree: the Dockerfile is the definition's base
// image, env and commands (setup before install), the engine is asked for
// the image before it is built, the build's output is kept in the session
// directory, and `docker run` is given the built tag, not a sandbox_image.
// The definition's workdir, secrets and services are ignored. A second
// session with the definition unchanged finds the image and builds nothing.
func TestContainerUseEnvironmentBuildsTheImage(t *testing.T) {
	r, role := containerUseRunner(t, containerUseClaude)
	worktree := t.TempDir()
	writeContainerUseEnvironment(t, worktree, "envs/dev", containerUseFixture)
	fakeDir := filepath.Dir(r.DockerBin)

	res, err := r.Run(context.Background(), Request{Name: "cue", Role: role, WorkDir: worktree})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "built" {
		t.Fatalf("result: %+v", res)
	}
	want := containerUseTag(containerUseFixtureDockerfile)
	if !strings.HasPrefix(want, containerUseRepo+":") || len(want) != len(containerUseRepo)+1+16 {
		t.Fatalf("tag: %q", want)
	}
	built, err := os.ReadFile(filepath.Join(fakeDir, "Dockerfile.built"))
	if err != nil {
		t.Fatalf("no build ran: %v", err)
	}
	if string(built) != containerUseFixtureDockerfile {
		t.Errorf("Dockerfile built:\n%s\nwant:\n%s", built, containerUseFixtureDockerfile)
	}
	if kept, _ := os.ReadFile(filepath.Join(res.SessionDir, "Dockerfile")); string(kept) != containerUseFixtureDockerfile {
		t.Errorf("Dockerfile kept in the session directory:\n%s", kept)
	}
	if got := strings.Join(lines(t, filepath.Join(fakeDir, "docker-build.txt")), " "); !strings.HasPrefix(got, "build --tag "+want+" --file ") {
		t.Errorf("build args: %q", got)
	}
	if got := lines(t, filepath.Join(fakeDir, "docker-inspect.txt")); len(got) != 1 || got[0] != want {
		t.Errorf("the engine was asked about %v before the build, want [%s]", got, want)
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, containerUseBuildLog)); err != nil {
		t.Errorf("build log: %v", err)
	}
	run := strings.Join(lines(t, filepath.Join(res.SessionDir, "docker-args.txt")), " ")
	if !strings.Contains(run, " "+want+" "+r.ClaudeBin+" -p ") {
		t.Errorf("docker run does not run the built image: %s", run)
	}
	if strings.Contains(run, "/workdir") {
		t.Errorf("docker run carries the definition's workdir: %s", run)
	}
	if !strings.Contains(run, "--workdir "+worktree+" ") {
		t.Errorf("docker run does not run in the worktree: %s", run)
	}

	// The image is reused: the second session finds the tag and builds
	// nothing, and runs the same image.
	if err := os.Remove(filepath.Join(fakeDir, "docker-build.txt")); err != nil {
		t.Fatal(err)
	}
	res2, err := r.Run(context.Background(), Request{Name: "cue2", Role: role, WorkDir: worktree})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(fakeDir, "docker-build.txt")); err == nil {
		t.Error("the image was built again for an unchanged definition")
	}
	if _, err := os.Stat(filepath.Join(res2.SessionDir, containerUseBuildLog)); err == nil {
		t.Error("a build log was written for a session that built nothing")
	}
	if run2 := strings.Join(lines(t, filepath.Join(res2.SessionDir, "docker-args.txt")), " "); !strings.Contains(run2, " "+want+" ") {
		t.Errorf("the second session did not run the built image: %s", run2)
	}
	if kept, _ := os.ReadFile(filepath.Join(res2.SessionDir, "Dockerfile")); string(kept) != containerUseFixtureDockerfile {
		t.Errorf("Dockerfile kept for the second session:\n%s", kept)
	}
}

// The tag is the hash of the resolved build inputs: the same for a
// definition that differs only in formatting, ordering or ignored fields,
// different for a change to any of base_image, setup_commands,
// install_commands or env.
func TestContainerUseTagFollowsTheBuildInputs(t *testing.T) {
	render := func(t *testing.T, body string) string {
		t.Helper()
		path := writeContainerUseEnvironment(t, t.TempDir(), "e", body)
		env, err := loadContainerUseEnvironment(path)
		if err != nil {
			t.Fatal(err)
		}
		df, err := containerUseDockerfile(env)
		if err != nil {
			t.Fatal(err)
		}
		return containerUseTag(df)
	}
	base := render(t, containerUseFixture)
	if got := render(t, containerUseFixtureDockerfileSource()); got != base {
		t.Errorf("a reformatted definition changed the tag: %s vs %s", got, base)
	}
	for name, body := range map[string]string{
		"base_image":       strings.Replace(containerUseFixture, "base:1", "base:2", 1),
		"setup_commands":   strings.Replace(containerUseFixture, "install -y git gh", "install -y git gh curl", 1),
		"install_commands": strings.Replace(containerUseFixture, "/etc/motd", "/etc/issue", 1),
		"env":              strings.Replace(containerUseFixture, "production", "test", 1),
	} {
		if body == containerUseFixture {
			t.Fatalf("%s: the fixture was not changed", name)
		}
		if got := render(t, body); got == base {
			t.Errorf("a change to %s left the tag %s", name, got)
		}
	}
	// Setup and install commands are ordered, and the two lists are not one.
	swapped := strings.Replace(strings.Replace(containerUseFixture, `"apt-get update && apt-get install -y git gh",
    "npm install -g @anthropic-ai/claude-code"`, `"npm install -g @anthropic-ai/claude-code",
    "apt-get update && apt-get install -y git gh"`, 1), "", "", 1)
	if swapped == containerUseFixture {
		t.Fatal("the fixture's setup commands were not swapped")
	}
	if got := render(t, swapped); got == base {
		t.Error("reordering setup_commands left the tag unchanged")
	}
}

// containerUseFixtureDockerfileSource is containerUseFixture with the same
// build inputs written differently: keys reordered, the ignored fields
// dropped, other whitespace.
func containerUseFixtureDockerfileSource() string {
	return `{"env":["NODE_ENV=production","GREETING=hello \"world\""],"install_commands":["echo \"it's built\" > /etc/motd"],` +
		`"setup_commands":["apt-get update && apt-get install -y git gh","npm install -g @anthropic-ai/claude-code"],"base_image":"ghcr.io/acme/base:1"}`
}

// A definition that is missing, that is not JSON, that names no base image
// or whose env cannot be rendered fails the session, naming the file and
// what is wrong, before anything starts: no build, no server, no agent.
func TestContainerUseEnvironmentRefusedWhenUnusable(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string // "" writes no file
		want []string
	}{
		{"missing", "", []string{"envs/dev", filepath.Join("envs/dev", containerUseDir, containerUseFile), "does not exist"}},
		{"malformed", "{not json", []string{filepath.Join("envs/dev", containerUseDir, containerUseFile), "not a container-use environment definition"}},
		{"no base image", `{"setup_commands": ["true"]}`, []string{filepath.Join("envs/dev", containerUseDir, containerUseFile), "base_image"}},
		{"env without a key", `{"base_image": "img", "env": ["=x"]}`, []string{filepath.Join("envs/dev", containerUseDir, containerUseFile), "KEY=VALUE"}},
		{"env over two lines", `{"base_image": "img", "env": ["A=1\nFROM evil"]}`, []string{filepath.Join("envs/dev", containerUseDir, containerUseFile), "more than one line"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, role := containerUseRunner(t, `touch "$BEES_SESSION_DIR/ran"`)
			worktree := t.TempDir()
			if tc.body != "" {
				writeContainerUseEnvironment(t, worktree, "envs/dev", tc.body)
			}
			_, err := r.Run(context.Background(), Request{Name: "cue", Role: role, WorkDir: worktree})
			if err == nil {
				t.Fatal("the session ran")
			}
			for _, want := range append([]string{"developer", "container_use_environment"}, tc.want...) {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not mention %q", err, want)
				}
			}
			assertContainerUseNothingStarted(t, r)
		})
	}
}

// A build that fails fails the session, naming the definition, the engine
// command and where its output is, and starts neither the server nor the
// agent.
func TestContainerUseEnvironmentBuildFailureFailsTheSession(t *testing.T) {
	r, role := containerUseRunner(t, `touch "$BEES_SESSION_DIR/ran"`)
	worktree := t.TempDir()
	path := writeContainerUseEnvironment(t, worktree, "envs/dev", containerUseFixture)
	fakeDir := filepath.Dir(r.DockerBin)
	if err := os.WriteFile(filepath.Join(fakeDir, "fail-build"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := r.Run(context.Background(), Request{Name: "cue", Role: role, WorkDir: worktree})
	if err == nil {
		t.Fatal("the session ran")
	}
	for _, want := range []string{"developer", "container_use_environment", "envs/dev", path, "docker build --tag " + containerUseRepo + ":", containerUseBuildLog} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	entries, _ := os.ReadDir(r.SessionsDir)
	if len(entries) != 1 {
		t.Fatalf("session directories: %d", len(entries))
	}
	log, err := os.ReadFile(filepath.Join(r.SessionsDir, entries[0].Name(), containerUseBuildLog))
	if err != nil || !strings.Contains(string(log), "did not complete successfully") {
		t.Errorf("build log: %q, %v", log, err)
	}
	assertContainerUseNothingStarted(t, r)
}

// assertContainerUseNothingStarted checks that no session directory of the
// runner saw the built-in server or the agent start.
func assertContainerUseNothingStarted(t *testing.T, r *Runner) {
	t.Helper()
	entries, _ := os.ReadDir(r.SessionsDir)
	for _, e := range entries {
		for _, file := range []string{"ran", "server-pid.txt"} {
			if _, err := os.Stat(filepath.Join(r.SessionsDir, e.Name(), file)); err == nil {
				t.Errorf("%s was written: something started after the image failed", file)
			}
		}
	}
}
