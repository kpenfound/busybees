package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/agent/procs"
	"github.com/kpenfound/busybees/core/vcs"
)

// fakeSbx stands in for the Docker Sandboxes CLI: `create` and `rm` record
// their arguments beside the script, `exec` records its arguments and the
// client's environment in the session directory and runs the command after
// the sandbox name on the host.
func fakeSbx(t *testing.T) string { return agenttest.Sbx(t, "TASK_SESSION_DIR") }

// A sandbox session is `sbx create` with the worktree, the repository's .git
// and the state directory as workspaces at their host paths, the claude
// command inside `sbx exec` with the session's variables handed over by
// name, the built-in server on the host reached over HTTP with the
// session's token, the sandbox's name recorded in the session directory for
// as long as the session runs, and `sbx rm` at the end.
func TestSandboxSessionRunsInsideSbx(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-host")
	t.Setenv("HOME", "/Users/operator")
	claude := fakeClaude(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
cat > "$TASK_SESSION_DIR/stdin.txt"
[ -f "$TASK_SESSION_DIR/sandbox-name" ] && cp "$TASK_SESSION_DIR/sandbox-name" "$TASK_SESSION_DIR/name-while-running"
[ -f "$TASK_SESSION_DIR/mcp-server-pid" ] && cp "$TASK_SESSION_DIR/mcp-server-pid" "$TASK_SESSION_DIR/server-pid-while-running"
echo '{"type":"result","subtype":"success","is_error":false,"result":"boxed","session_id":"abc","num_turns":2,"total_cost_usd":0.1}'
printf '{"status":"submitted","work":{"key":"task/7","tags":{"ticket":"seven"}}}' > "$TASK_SESSION_DIR/outcome.json"
`)
	metadata, worktree := workspaceFixture(t)
	r := newRunner(t, claude)
	r.SbxBin = fakeSbx(t)
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	role := Profile{Name: "builder", Model: "opus", MaxTurns: 5, Timeout: time.Minute,
		Sandbox: SandboxSbx, SandboxImage: "ghcr.io/acme/task:1",
		Shell: "/bin/sh", Env: map[string]string{"FACTORY_TOKEN": "abc"}}
	res, err := r.Run(context.Background(), Request{Name: "boxed_1", Profile: role, Workspace: fakeWorkspace{dir: worktree, access: &vcs.Access{Mounts: []string{metadata}}}, SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{EnvIssue: "12"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "boxed" || !res.HasOutcome || res.Outcome.Work.Key != "task/7" {
		t.Fatalf("result: %+v", res)
	}
	dir := res.SessionDir
	engine := filepath.Dir(r.SbxBin)

	// The sandbox was created with every granted directory as a workspace,
	// the worktree first, at its host path and at its real one where the
	// two differ, from the profile's template and without the shared
	// skills store; sbx's name rules are met.
	create := lines(t, filepath.Join(engine, "sbx-create.txt"))
	if len(create) < 7 || strings.Join(create[:2], " ") != "create --quiet" || create[2] != "--name" || create[4] != "--skills" || create[5] != "off" || create[6] != "--template" || create[7] != "ghcr.io/acme/task:1" || create[8] != "claude" {
		t.Fatalf("sbx create args: %v", create)
	}
	name := create[3]
	if !strings.HasPrefix(name, "task-boxed-1-") || strings.ContainsAny(name, "_/ ") {
		t.Errorf("sandbox name %q is not the session's, made acceptable to sbx", name)
	}
	workspaces := create[9:]
	if workspaces[0] != worktree {
		t.Errorf("the primary workspace is %q, want the worktree %s", workspaces[0], worktree)
	}
	want := map[string]bool{}
	for _, p := range []string{worktree, metadata, dir, r.StateDir} {
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			t.Fatal(err)
		}
		want[p], want[real] = true, true
	}
	got := map[string]bool{}
	for _, w := range workspaces {
		got[w] = true
	}
	for w := range want {
		if !got[w] {
			t.Errorf("workspaces %v lack %s", workspaces, w)
		}
	}
	for w := range got {
		if !want[w] {
			t.Errorf("workspace %s was not granted", w)
		}
	}
	if !slices.IsSorted(workspaces[1:]) {
		t.Errorf("workspaces after the primary are not in path order: %v", workspaces)
	}

	// The command ran through `sbx exec` in the worktree, with the
	// session's variables by name and no value on the command line; the
	// host's HOME, credential and PATH stay out, the sandbox has its own.
	execArgs := lines(t, filepath.Join(dir, "sbx-exec-args.txt"))
	joined := strings.Join(execArgs, " ")
	if !strings.HasPrefix(joined, "exec --interactive --workdir "+worktree+" --env ") {
		t.Errorf("sbx exec args: %s", joined)
	}
	for _, want := range []string{"--env ACCESS_TOKEN ", "--env FACTORY_TOKEN ", "--env SHELL ", "--env TASK_SESSION_DIR ", "--env TASK_ISSUE ", "--env " + EnvMCPToken + " ", " " + name + " " + claude + " -p "} {
		if !strings.Contains(joined+" ", want) {
			t.Errorf("sbx exec args missing %q:\n%s", want, joined)
		}
	}
	for _, absent := range []string{"--env HOME", "--env ANTHROPIC_API_KEY", "--env PATH", "--env TASK_BIN", "--user", "--mount"} {
		if strings.Contains(joined, absent) {
			t.Errorf("sbx exec args carry %q: %s", absent, joined)
		}
	}
	for _, secret := range []string{"secret", "sk-host", "abc"} {
		if strings.Contains(joined, secret) {
			t.Errorf("sbx exec args carry a value (%q): %s", secret, joined)
		}
	}
	// The values travel in the client's environment, which keeps the
	// host's HOME for the client's own configuration.
	clientEnv := lines(t, filepath.Join(dir, "sbx-exec-env.txt"))
	for _, want := range []string{"ACCESS_TOKEN=secret", "FACTORY_TOKEN=abc", "TASK_ROLE=builder", "TASK_ISSUE=12", "HOME=/Users/operator"} {
		if !slices.Contains(clientEnv, want) {
			t.Errorf("client env missing %s", want)
		}
	}

	// The built-in server ran on the host, on the loopback, with a token
	// the session reaches it by at the host's alias.
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
	if got := strings.Join(lines(t, filepath.Join(dir, "server-args.txt")), " "); got != "mcp serve --listen 127.0.0.1:0" {
		t.Errorf("server args: %q", got)
	}
	builtin := readMCPConfig(t, dir)["tools"]
	if builtin.Type != "http" || builtin.URL != "http://host.docker.internal:45678/mcp" || builtin.Headers["Authorization"] != "Bearer ${"+EnvMCPToken+"}" {
		t.Errorf("built-in server entry: %+v", builtin)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(lines(t, filepath.Join(dir, "server-pid.txt"))[0]))
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

	// The sandbox's name was recorded while the session ran, and the
	// sandbox is removed and forgotten after it.
	if b, err := os.ReadFile(filepath.Join(dir, "name-while-running")); err != nil || strings.TrimSpace(string(b)) != name {
		t.Errorf("sandbox name while running: %q, %v; want %s", b, err, name)
	}
	if procs.SandboxName(dir) != "" {
		t.Error("sandbox name file left behind after the session")
	}
	if _, err := os.Stat(filepath.Join(dir, procs.ServerPIDFile)); err == nil {
		t.Error("server pid file left behind after the session")
	}
	if rm := lines(t, filepath.Join(engine, "sbx-rm.txt")); len(rm) != 3 || strings.Join(rm, " ") != "rm --force "+name {
		t.Errorf("sbx rm: %v, want one removal of %s", rm, name)
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

// The workspaces are the turn's binds: the working directory first, then
// the rest in path order, read-only ones marked so. A path sbx cannot take,
// or a working directory the binds do not cover, is refused.
func TestSandboxWorkspacesFollowTheTurn(t *testing.T) {
	binds := []Bind{
		{Source: "/private/state", Destination: "/state", Access: ReadWrite},
		{Source: "/private/state", Destination: "/private/state", Access: ReadWrite},
		{Source: "/private/w", Destination: "/w", Access: ReadWrite},
		{Source: "/private/w", Destination: "/private/w", Access: ReadWrite},
		{Source: "/cache", Destination: "/cache", Access: ReadOnly},
	}
	s := &sandbox{container: container{req: Request{Workspace: fakeWorkspace{dir: "/w"}}, turn: &Turn{Binds: binds}}}
	got, err := s.workspaces()
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"/w", "/cache:ro", "/private/state", "/private/w", "/state"}; !slices.Equal(got, want) {
		t.Errorf("workspaces = %v, want %v", got, want)
	}

	// The primary workspace is the working directory itself: one inside a
	// bind without being one is refused.
	for _, dir := range []string{"/elsewhere", "/w/sub"} {
		s.req.Workspace = fakeWorkspace{dir: dir}
		if _, err := s.workspaces(); !errors.Is(err, ErrNotGranted) {
			t.Errorf("working directory %s: %v", dir, err)
		}
	}
}

// A HOME the session sets reaches the sandbox by value: the client keeps
// the operator's own HOME to find its configuration, so passed by name it
// would be the operator's.
func TestSandboxSessionPassesTheSessionsHOME(t *testing.T) {
	t.Setenv("HOME", "/Users/operator")
	r := newRunner(t, fakeClaude(t, `echo '{"type":"result","subtype":"success","result":"ok"}'`))
	r.SbxBin = fakeSbx(t)
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	role := Profile{Name: "builder", Sandbox: SandboxSbx, Env: map[string]string{"HOME": "/home/role"}}
	res, err := r.Run(context.Background(), Request{Name: "home", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}})
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	args := strings.Join(lines(t, filepath.Join(res.SessionDir, "sbx-exec-args.txt")), "\n") + "\n"
	if !strings.Contains(args, "--env\nHOME=/home/role\n") {
		t.Errorf("sbx exec args do not pass the session's HOME by value:\n%s", args)
	}
	if strings.Contains(args, "--env\nHOME\n") {
		t.Errorf("sbx exec args pass HOME by name, which the client holds as the operator's:\n%s", args)
	}
	if env := lines(t, filepath.Join(res.SessionDir, "sbx-exec-env.txt")); !slices.Contains(env, "HOME=/Users/operator") {
		t.Error("the sbx client lost its own HOME")
	}
}

// A refusal of the sandbox boundary names the sandbox and what sbx cannot
// take, not a container's: sbx is handed each workspace as one argument,
// path[:ro], so a colon is refused and a comma or a quote is not, and the
// host's root is refused as a container's is.
func TestSandboxBoundaryRefusesWhatSbxCannotTake(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := func(name string) string {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	work, state := dir("work"), dir("state")
	for _, tc := range []struct {
		name     string
		mount    Mount
		sandbox  []string // nil: the sandbox accepts it
		inBox    []string // what the container boundary says of the same grant; nil: it accepts it
		mountDir bool     // the mount is a directory the runner needs read-write
	}{
		{"colon", Mount{Path: dir("a:b"), Access: ReadOnly}, []string{"sbx", "workspace", "colon"}, nil, false},
		{"comma", Mount{Path: dir("a,b"), Access: ReadOnly}, nil, []string{"docker's --mount"}, false},
		{"quote", Mount{Path: dir(`a"b`), Access: ReadOnly}, nil, []string{"docker's --mount"}, false},
		{"root", Mount{Path: "/", Access: ReadOnly}, []string{"a sandbox cannot be given the host's root"}, []string{"a container cannot be given the host's root"}, false},
		{"read-only runner directory", Mount{Path: state, Access: ReadOnly}, []string{"writable in the sandbox"}, []string{"writable in the container"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := grantAll(Request{Workspace: fakeWorkspace{dir: work}, SessionDir: work, Profile: Profile{Sandbox: SandboxSbx}})
			req.Grants.Mounts = append(req.Grants.Mounts, tc.mount)
			var mountDirs []string
			if tc.mountDir {
				mountDirs = []string{tc.mount.Path}
			}
			_, err := SandboxBoundary{MountDirs: mountDirs}.Verify(req)
			check(t, "sandbox", err, tc.sandbox)
			req.Profile.Sandbox = SandboxContainer
			_, err = ContainerBoundary{MountDirs: mountDirs}.Verify(req)
			check(t, "container", err, tc.inBox)
		})
	}
}

// check asserts err is nil when want is, and otherwise a refusal naming
// every word of want.
func check(t *testing.T, box string, err error, want []string) {
	t.Helper()
	if want == nil {
		if err != nil {
			t.Errorf("%s refused: %v", box, err)
		}
		return
	}
	if !errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrNotGranted) {
		t.Fatalf("%s accepted, or refused for another reason: %v", box, err)
	}
	for _, w := range want {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("%s refusal %q does not say %q", box, err, w)
		}
	}
	if box == "sandbox" && strings.Contains(err.Error(), "container") {
		t.Errorf("sandbox refusal speaks of a container: %v", err)
	}
}

// A profile with skills needs the skills directory granted: refused when it
// is not, and a read-only workspace of the sandbox when it is.
func TestSandboxBoundaryGrantsTheSkills(t *testing.T) {
	work, skills := t.TempDir(), t.TempDir()
	req := grantAll(Request{Workspace: fakeWorkspace{dir: work}, SessionDir: work, Profile: Profile{Sandbox: SandboxSbx, Skills: []string{"https://example.com/skills"}}})
	b := SandboxBoundary{SkillMountDirs: []string{skills}}
	if _, err := b.Verify(req); !errors.Is(err, ErrNotGranted) || !strings.Contains(err.Error(), "skill directory") {
		t.Errorf("an ungranted skills directory: %v", err)
	}
	// The runner hands its skills directories to the sandbox boundary.
	r := &Runner{Skills: &preparedSkills{dir: skills}, SkillMountDirs: []string{skills}}
	if _, err := r.Verify(req); !errors.Is(err, ErrNotGranted) || !strings.Contains(err.Error(), "skill directory") {
		t.Errorf("the runner verified an sbx request without its skills directory: %v", err)
	}
	req.Grants.Mounts = append(req.Grants.Mounts, Mount{Path: skills, Access: ReadOnly})
	turn, err := b.Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	got, err := (&sandbox{container: container{req: req, turn: turn}}).workspaces()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, skills+":ro") {
		t.Errorf("workspaces %v lack the skills directory read-only", got)
	}
}

// The sandbox boundary binds what the container boundary binds and builds
// the environment from the session's variables alone: the host's credential
// is not forwarded even when granted (the sandbox's proxy supplies it) and
// no HOME is set (the sandbox has its own). Without VCS the executables are
// denied by name.
func TestSandboxBoundaryEnvironmentAndBinds(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "host-credential")
	t.Setenv("HOST_ONLY", "host-value")
	work, state := t.TempDir(), t.TempDir()
	for _, vcsGranted := range []bool{true, false} {
		req := grantAll(Request{Workspace: fakeWorkspace{dir: work}, SessionDir: t.TempDir(), Profile: Profile{Sandbox: SandboxSbx, Env: map[string]string{"EXPANDED": "$HOST_ONLY"}, Shell: "/bin/bash", VCSAccess: vcsGranted}, Env: map[string]string{"CONTEXT": "caller"}, ContainerEnv: map[string]string{"CONTEXT": "container"}}, "USER")
		req.Grants.VCS = vcsGranted
		req.Grants.Mounts = append(req.Grants.Mounts, Mount{Path: state, Access: ReadWrite})
		turn, err := SandboxBoundary{MountDirs: []string{state}}.Verify(req)
		if err != nil {
			t.Fatal(err)
		}
		env := map[string]string{}
		for _, kv := range turn.Env {
			k, v, _ := strings.Cut(kv, "=")
			env[k] = v
		}
		for key, want := range map[string]string{"EXPANDED": "host-value", "SHELL": "/bin/bash", "CONTEXT": "container"} {
			if env[key] != want {
				t.Errorf("vcs=%v: %s=%q, want %q", vcsGranted, key, env[key], want)
			}
		}
		for _, key := range []string{"HOME", "ANTHROPIC_API_KEY", "HOST_ONLY", "PATH", "USER"} {
			if _, ok := env[key]; ok {
				t.Errorf("vcs=%v: %s reached the sandbox", vcsGranted, key)
			}
		}
		if denied := len(turn.DeniedExecutables) > 0; denied == vcsGranted {
			t.Errorf("vcs=%v: denied executables %v", vcsGranted, turn.DeniedExecutables)
		}
		real, _ := filepath.EvalSymlinks(state)
		if !slices.ContainsFunc(turn.Binds, func(b Bind) bool { return b.Destination == real && b.Access == ReadWrite }) {
			t.Errorf("vcs=%v: binds %v lack the state directory", vcsGranted, turn.Binds)
		}
	}
	// A directory the runner needs that no grant covers is refused.
	req := grantAll(Request{Workspace: fakeWorkspace{dir: work}, SessionDir: t.TempDir(), Profile: Profile{Sandbox: SandboxSbx}})
	if _, err := (SandboxBoundary{MountDirs: []string{state}}).Verify(req); !errors.Is(err, ErrNotGranted) {
		t.Errorf("an ungranted mount directory: %v", err)
	}
	// A profile that names a variable outside the allowlist is refused.
	req = grantAll(Request{Workspace: fakeWorkspace{dir: work}, SessionDir: t.TempDir(), Profile: Profile{Sandbox: SandboxSbx}}, "USER")
	req.ContainerEnv = map[string]string{"UNLISTED": "x"}
	if _, err := (SandboxBoundary{}).Verify(req); !errors.Is(err, ErrNotGranted) {
		t.Errorf("an ungranted variable: %v", err)
	}
}

// Without VCS the session's command runs behind a shell that puts the
// stand-ins first on the sandbox's PATH.
func TestSandboxDeniesVCSExecutablesByName(t *testing.T) {
	dir := t.TempDir()
	for _, granted := range []bool{true, false} {
		s := &sandbox{container: container{r: &Runner{}, sessionDir: dir, req: Request{Workspace: fakeWorkspace{dir: "/w"}}, turn: &Turn{}}, created: true}
		s.name = "task-x"
		if !granted {
			s.turn.DeniedExecutables = slices.Clone(VCSExecutables)
		}
		bin, args, err := s.command(context.Background(), "claude", []string{"-p"})
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(args, " ")
		wrapped := strings.Contains(joined, " task-x /bin/sh -c ")
		if bin != SandboxCLI || wrapped == granted || !strings.HasSuffix(joined, " claude -p") {
			t.Errorf("vcs=%v: %s %s", granted, bin, joined)
		}
		if !granted {
			if _, err := os.Stat(filepath.Join(dir, deniedBinDir, "git")); err != nil {
				t.Errorf("no stand-in for git: %v", err)
			}
		}
	}
}

// A stopped session's sandbox is removed, once.
func TestStoppedSandboxSessionIsRemoved(t *testing.T) {
	claude := fakeClaude(t, `sleep 5`)
	r := newRunner(t, claude)
	r.SbxBin = fakeSbx(t)
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	role := Profile{Name: "auditor", Model: "opus", MaxTurns: 1, Timeout: 200 * time.Millisecond, Sandbox: SandboxSbx}
	res, err := r.Run(context.Background(), Request{Name: "slow", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut {
		t.Fatalf("result: %+v", res)
	}
	rm := lines(t, filepath.Join(filepath.Dir(r.SbxBin), "sbx-rm.txt"))
	if len(rm) != 3 || !strings.HasPrefix(strings.Join(rm, " "), "rm --force task-slow-") {
		t.Errorf("sandbox not removed exactly once: %v", rm)
	}
}

// A failed `sbx create` starts nothing: no server, no removal, no record.
func TestSandboxCreateFailureStartsNothing(t *testing.T) {
	r := newRunner(t, fakeClaude(t, `echo '{"type":"result","subtype":"success","result":"ok"}'`))
	r.SbxBin = fakeSbx(t)
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(filepath.Dir(r.SbxBin), "fail-create"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := r.NewSessionDir("boxed")
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), Request{Name: "boxed", SessionDir: dir, Profile: Profile{Name: "builder", Sandbox: SandboxSbx}, Workspace: fakeWorkspace{dir: t.TempDir()}})
	if err == nil {
		t.Fatal("the session ran without a sandbox")
	}
	for _, want := range []string{"sbx create", "underscores"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	for _, left := range []string{filepath.Join(dir, "server-pid.txt"), filepath.Join(filepath.Dir(r.SbxBin), "sbx-rm.txt"), filepath.Join(dir, procs.SandboxNameFile)} {
		if _, err := os.Stat(left); err == nil {
			t.Errorf("%s exists after a failed create", filepath.Base(left))
		}
	}
}

// The sandbox CLI and the agent inside it go through the same guard as
// every other executable: a test binary never runs a real one.
func TestSandboxGuard(t *testing.T) {
	real, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	for name, r := range map[string]Runner{
		"cli":   {SbxBin: real},
		"agent": {ClaudeBin: real, SbxBin: agenttest.Sbx(t, "RUN_DIR")},
	} {
		dir := t.TempDir()
		_, err := r.Run(context.Background(), grantAll(Request{SessionDir: dir, Workspace: fakeWorkspace{dir: dir}, Env: map[string]string{"RUN_DIR": dir}, Profile: Profile{Sandbox: SandboxSbx}}))
		if !errors.Is(err, agentbin.ErrRealAgent) {
			t.Errorf("%s unguarded: %v", name, err)
		}
	}
}

// The sbx sandbox runs claude only, holds a session to its mounts by itself,
// and takes a template rather than a container-use environment.
func TestSandboxProfileValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Profile
		want string
	}{
		{"claude", Profile{Sandbox: SandboxSbx, Agent: AgentClaude}, ""},
		{"default agent", Profile{Sandbox: SandboxSbx}, ""},
		{"template", Profile{Sandbox: SandboxSbx, SandboxImage: "acme/template:1"}, ""},
		{"codex", Profile{Sandbox: SandboxSbx, Agent: AgentCodex}, "codex"},
		{"opencode", Profile{Sandbox: SandboxSbx, Agent: AgentOpenCode}, "opencode"},
		{"confine", Profile{Sandbox: SandboxSbx, Confine: true}, "confine"},
		{"container-use", Profile{Sandbox: SandboxSbx, ContainerUseEnvironment: "dagger/env"}, "container_use_environment"},
	} {
		err := tc.p.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v does not mention %q", tc.name, err, tc.want)
		}
	}
}

// A sandbox name is what sbx accepts: letters, digits, hyphens and periods,
// starting with one of the first two.
func TestSandboxName(t *testing.T) {
	for in, want := range map[string]string{"task-boxed_1": "task-boxed-1", "_x": "x", "": "session", "a.b/c d": "a.b-c-d", "-.": "session"} {
		if got := sandboxName(in); got != want {
			t.Errorf("sandboxName(%q) = %q, want %q", in, got, want)
		}
	}
}
