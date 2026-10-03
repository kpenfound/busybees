package agent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/agent/procs"
)

// echoEngine stands in for a Dagger engine on a Unix socket: it answers
// every line with the same line. The socket lives under /tmp, where its path
// is short enough for every platform's limit.
func echoEngine(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "engine")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	sock := filepath.Join(dir, "engine.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer func() { _ = c.Close() }()
				in := bufio.NewScanner(c)
				for in.Scan() {
					if _, err := c.Write([]byte(in.Text() + "\n")); err != nil {
						return
					}
				}
			}()
		}
	}()
	return sock
}

// echo sends one line through a connection and returns the answer.
func echo(t *testing.T, addr, line string) (string, error) {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return "", err
	}
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write([]byte(line + "\n")); err != nil {
		return "", err
	}
	got, err := bufio.NewReader(c).ReadString('\n')
	return strings.TrimSpace(got), err
}

// A session whose profile asks for Dagger gets the Dagger CLI installed in
// the sandbox at the profile's release before its command runs, and the
// host's engine: a socket forwarded from a port on the host's loopback for
// the session's lifetime, which the session reaches at host.docker.internal
// through the variable the Dagger CLI reads, passed by name.
func TestSandboxDaggerSession(t *testing.T) {
	sock := echoEngine(t)
	claude := fakeClaude(t, `
env > "$TASK_SESSION_DIR/agent-env.txt"
i=0
while [ ! -f "$TASK_SESSION_DIR/connected" ] && [ $i -lt 100 ]; do sleep 0.1; i=$((i+1)); done
echo '{"type":"result","subtype":"success","result":"ok"}'
`)
	r := newRunner(t, claude)
	r.SbxBin = fakeSbx(t)
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	dir, err := r.NewSessionDir("dagger")
	if err != nil {
		t.Fatal(err)
	}
	role := Profile{Name: "builder", Timeout: time.Minute, Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "unix://" + sock, Version: "v0.20.5"}}
	type run struct {
		res *Result
		err error
	}
	done := make(chan run, 1)
	go func() {
		res, err := r.Run(context.Background(), Request{Name: "dagger", SessionDir: dir, Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}})
		done <- run{res, err}
	}()

	// While the session runs, its engine address leads to the host's engine.
	var addr string
	deadline := time.Now().Add(10 * time.Second)
	for addr == "" && time.Now().Before(deadline) {
		for _, kv := range readLines(filepath.Join(dir, "agent-env.txt")) {
			if v, ok := strings.CutPrefix(kv, EnvDaggerRunnerHost+"="); ok {
				addr = v
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	port, ok := strings.CutPrefix(addr, "tcp://"+containerHostAlias+":")
	if !ok {
		t.Fatalf("%s = %q, want tcp://%s:<port>", EnvDaggerRunnerHost, addr, containerHostAlias)
	}
	local := net.JoinHostPort("127.0.0.1", port)
	if got, err := echo(t, local, "ping"); err != nil || got != "ping" {
		t.Errorf("through the forward: %q, %v; want the engine's answer", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "connected"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	got := <-done
	if got.err != nil || got.res.IsError {
		t.Fatalf("run: %+v, %v", got.res, got.err)
	}

	// The forward ends with the session.
	if _, err := echo(t, local, "late"); err == nil {
		t.Error("the engine is still forwarded after the session ended")
	}

	// The CLI was installed by the install script at the profile's release.
	setup := strings.Join(lines(t, filepath.Join(filepath.Dir(r.SbxBin), "sbx-setup.txt")), " ")
	name := lines(t, filepath.Join(filepath.Dir(r.SbxBin), "sbx-create.txt"))[3]
	want := "exec " + name + " sudo env BIN_DIR=/usr/local/bin DAGGER_VERSION=0.20.5 sh -c curl -fsSL " + DaggerInstallScript + " | sh"
	if setup != want {
		t.Errorf("setup commands:\n%s\nwant\n%s", setup, want)
	}
	// The sandbox was asked for its CLI first, and had none.
	if probe := lines(t, filepath.Join(filepath.Dir(r.SbxBin), "sbx-dagger-probe.txt")); !slices.Equal(probe, []string{"exec " + name + " dagger version"}) {
		t.Errorf("Dagger CLI probes: %v", probe)
	}
	// The sandbox alone was allowed the forward's port and the server's,
	// and both rules were removed with it.
	wantPolicy := []string{
		"policy allow network --sandbox " + name + " localhost:" + port,
		"policy allow network --sandbox " + name + " localhost:45678",
		"policy rm network --sandbox " + name + " --resource localhost:" + port + " --force",
		"policy rm network --sandbox " + name + " --resource localhost:45678 --force",
	}
	if got := lines(t, filepath.Join(filepath.Dir(r.SbxBin), "sbx-policy.txt")); !slices.Equal(got, wantPolicy) {
		t.Errorf("sbx policy calls:\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(wantPolicy, "\n"))
	}
	// The address reaches the session by name, with its value in the
	// client's environment.
	execArgs := strings.Join(lines(t, filepath.Join(dir, "sbx-exec-args.txt")), "\n") + "\n"
	if !strings.Contains(execArgs, "--env\n"+EnvDaggerRunnerHost+"\n") {
		t.Errorf("sbx exec does not pass %s by name:\n%s", EnvDaggerRunnerHost, execArgs)
	}
	if env := lines(t, filepath.Join(dir, "sbx-exec-env.txt")); !slices.Contains(env, EnvDaggerRunnerHost+"="+addr) {
		t.Errorf("the client's environment lacks %s=%s", EnvDaggerRunnerHost, addr)
	}
}

// readLines is lines for a file that may not be written yet.
func readLines(path string) []string {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A session without Dagger installs nothing and is given no engine.
func TestSandboxWithoutDaggerInstallsNothing(t *testing.T) {
	r := newRunner(t, fakeClaude(t, `env > "$TASK_SESSION_DIR/agent-env.txt"
echo '{"type":"result","subtype":"success","result":"ok"}'`))
	r.SbxBin = fakeSbx(t)
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	res, err := r.Run(context.Background(), Request{Name: "plain", Profile: Profile{Name: "builder", Sandbox: SandboxSbx}, Workspace: fakeWorkspace{dir: t.TempDir()}})
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(r.SbxBin), "sbx-setup.txt")); err == nil {
		t.Error("a setup command ran in a sandbox without Dagger")
	}
	for _, kv := range lines(t, filepath.Join(res.SessionDir, "agent-env.txt")) {
		if strings.HasPrefix(kv, EnvDaggerRunnerHost+"=") {
			t.Errorf("a session without Dagger was given an engine: %s", kv)
		}
	}
}

// A failed install stops the session before its command runs: the sandbox
// is removed, and neither the server nor the forward is left running.
func TestSandboxDaggerInstallFailure(t *testing.T) {
	sock := echoEngine(t)
	r := newRunner(t, fakeClaude(t, `touch "$TASK_SESSION_DIR/ran"`))
	r.SbxBin = fakeSbx(t)
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(filepath.Dir(r.SbxBin), "fail-setup"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	dir, err := r.NewSessionDir("dagger")
	if err != nil {
		t.Fatal(err)
	}
	role := Profile{Name: "builder", Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "unix://" + sock, Version: "0.20.5"}}
	_, err = r.Run(context.Background(), Request{Name: "dagger", SessionDir: dir, Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}})
	if err == nil {
		t.Fatal("the session ran without the Dagger CLI")
	}
	for _, want := range []string{"install the Dagger CLI 0.20.5", "dl.dagger.io"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
	for _, absent := range []string{"ran", "server-pid.txt", procs.SandboxNameFile} {
		if _, err := os.Stat(filepath.Join(dir, absent)); err == nil {
			t.Errorf("%s exists after a failed install", absent)
		}
	}
	if rm := lines(t, filepath.Join(filepath.Dir(r.SbxBin), "sbx-rm.txt")); len(rm) != 3 {
		t.Errorf("sandbox not removed exactly once: %v", rm)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(r.SbxBin), "sbx-policy.txt")); err == nil {
		t.Error("a network policy rule was added or removed for a sandbox whose install failed")
	}
}

// A sandbox whose template reports the profile's release, with or without
// the leading v, is not given the CLI again; one without the CLI, with
// another release or whose probe answers something else is, at the
// profile's release. Either way the session gets the engine and its rule.
func TestSandboxDaggerTemplateRelease(t *testing.T) {
	for _, c := range []struct {
		name, template, want string
		install              bool
	}{
		{"same release", "v0.20.5", "0.20.5", false},
		{"same release without v", "0.20.5", "v0.20.5", false},
		{"no CLI", "", "0.20.5", true},
		{"another release", "v0.19.0", "0.20.5", true},
		{"unreadable answer", "", "0.20.5", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := &Runner{SbxBin: fakeSbx(t), Logger: slog.Default()}
			here := filepath.Dir(r.SbxBin)
			switch {
			case c.name == "unreadable answer":
				// The CLI runs but does not print a release.
				if err := os.WriteFile(filepath.Join(here, "dagger-version"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			case c.template != "":
				if err := os.WriteFile(filepath.Join(here, "dagger-version"), []byte(c.template), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			engine := "tcp://127.0.0.1:1234"
			s := &sandbox{container: container{r: r, name: "box", turn: &Turn{DaggerEngine: engine}, req: Request{Profile: Profile{Dagger: &Dagger{Engine: engine, Version: c.want}}}}}
			if err := s.startDagger(context.Background()); err != nil {
				t.Fatal(err)
			}
			if probe := readLines(filepath.Join(here, "sbx-dagger-probe.txt")); !slices.Equal(probe, []string{"exec box dagger version"}) {
				t.Errorf("Dagger CLI probes: %v", probe)
			}
			setup := strings.Join(readLines(filepath.Join(here, "sbx-setup.txt")), " ")
			if !c.install && setup != "" {
				t.Errorf("installed over the template's release: %s", setup)
			}
			if c.install && !strings.Contains(setup, " DAGGER_VERSION=0.20.5 ") {
				t.Errorf("not installed at the profile's release: %q", setup)
			}
			if got := readLines(filepath.Join(here, "sbx-policy.txt")); !slices.Equal(got, []string{"policy allow network --sandbox box localhost:1234"}) {
				t.Errorf("policy calls %v", got)
			}
			if !slices.Contains(s.vars, envVar{EnvDaggerRunnerHost, "tcp://" + containerHostAlias + ":1234"}) {
				t.Errorf("the session was not given the engine: %v", s.vars)
			}
		})
	}
}

// Only an engine the sandbox reaches at host.docker.internal is given a
// network policy rule: any other is left to the policy the operator keeps.
func TestSandboxDaggerEngineRule(t *testing.T) {
	for engine, want := range map[string][]string{
		"tcp://127.0.0.1:1234":      {"allow network --sandbox box localhost:1234"},
		"tcp://engine.example:1234": nil,
	} {
		r := &Runner{SbxBin: fakeSbx(t), Logger: slog.Default()}
		s := &sandbox{container: container{r: r, name: "box", turn: &Turn{DaggerEngine: engine}, req: Request{Profile: Profile{Dagger: &Dagger{Engine: engine, Version: "0.20.5"}}}}}
		if err := s.startDagger(context.Background()); err != nil {
			t.Fatalf("%s: %v", engine, err)
		}
		var got []string
		for _, l := range readLines(filepath.Join(filepath.Dir(r.SbxBin), "sbx-policy.txt")) {
			if l != "" {
				got = append(got, strings.TrimPrefix(l, "policy "))
			}
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: policy calls %v, want %v", engine, got, want)
		}
	}
}

// A session granted a docker-container engine is allowed only the
// forward's port, as for any other engine reached at host.docker.internal,
// and is given nothing else of the container engine: no DOCKER_HOST and no
// path of the engine's own executable.
func TestSandboxDaggerContainerEngineRule(t *testing.T) {
	engine := "docker-container://dagger-engine-v0.20.5"
	docker := fakeContainerEngine(t)
	r := &Runner{SbxBin: fakeSbx(t), DockerBin: docker, Logger: slog.Default()}
	s := &sandbox{container: container{r: r, name: "box", turn: &Turn{DaggerEngine: engine}, req: Request{Profile: Profile{Dagger: &Dagger{Engine: engine, Version: "0.20.5"}}}}}
	if err := s.startDagger(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer s.forward.close()
	_, port, _ := net.SplitHostPort(s.forward.addr())
	want := []string{"policy allow network --sandbox box localhost:" + port}
	if got := readLines(filepath.Join(filepath.Dir(r.SbxBin), "sbx-policy.txt")); !slices.Equal(got, want) {
		t.Errorf("policy calls %v, want %v", got, want)
	}
	for _, v := range s.vars {
		if v.name == "DOCKER_HOST" || strings.Contains(v.value, docker) {
			t.Errorf("the session was given the container engine: %+v", v)
		}
	}
}

// Two sessions granted the same docker-container engine each get a forward
// of their own, on different ports, while both are open at once.
func TestSandboxDaggerContainerSessionsGetDistinctPorts(t *testing.T) {
	engine := "docker-container://dagger-engine-v0.20.5"
	r := &Runner{SbxBin: fakeSbx(t), DockerBin: fakeContainerEngine(t), Logger: slog.Default()}
	var addrs []string
	for _, name := range []string{"box1", "box2"} {
		s := &sandbox{container: container{r: r, name: name, turn: &Turn{DaggerEngine: engine}, req: Request{Profile: Profile{Dagger: &Dagger{Engine: engine, Version: "0.20.5"}}}}}
		if err := s.startDagger(context.Background()); err != nil {
			t.Fatal(err)
		}
		defer s.forward.close()
		for _, v := range s.vars {
			if v.name == EnvDaggerRunnerHost {
				addrs = append(addrs, v.value)
			}
		}
	}
	if len(addrs) != 2 || addrs[0] == addrs[1] {
		t.Errorf("two concurrent sessions got %v, want two different addresses", addrs)
	}
}

// A TCP engine on the host's loopback is reached at host.docker.internal,
// and any other at its own address; a socket is forwarded from the
// loopback, whatever address the runner's own server listens on: the
// forward has no token, and the engine is root on the host.
func TestSandboxDaggerAddress(t *testing.T) {
	s := &sandbox{container: container{r: &Runner{}}}
	for engine, want := range map[string]string{
		"tcp://127.0.0.1:1234":      "tcp://host.docker.internal:1234",
		"tcp://localhost:1234":      "tcp://host.docker.internal:1234",
		"tcp://[::1]:1234":          "tcp://host.docker.internal:1234",
		"tcp://engine.example:1234": "tcp://engine.example:1234",
		"tcp://10.0.0.5:8080":       "tcp://10.0.0.5:8080",
	} {
		got, err := s.daggerAddress(engine)
		if err != nil || got != want {
			t.Errorf("%s: %q, %v; want %q", engine, got, err, want)
		}
	}
	if s.forward != nil {
		t.Error("a TCP engine was forwarded")
	}
	sock := echoEngine(t)
	s.r.ContainerListen = "0.0.0.0:0"
	got, err := s.daggerAddress("unix://" + sock)
	if err != nil {
		t.Fatal(err)
	}
	if s.forward == nil {
		t.Fatal("a socket was not forwarded")
	}
	defer s.forward.close()
	_, port, _ := net.SplitHostPort(s.forward.addr())
	if host, _, _ := net.SplitHostPort(s.forward.addr()); host != "127.0.0.1" {
		t.Errorf("the forward listens on %s, want the loopback", s.forward.addr())
	}
	if want := "tcp://host.docker.internal:" + port; got != want {
		t.Errorf("socket engine: %q, want %q", got, want)
	}

	c := &sandbox{container: container{r: &Runner{DockerBin: fakeContainerEngine(t), ContainerListen: "0.0.0.0:0"}}}
	got, err = c.daggerAddress("docker-container://dagger-engine-v0.20.5")
	if err != nil {
		t.Fatal(err)
	}
	if c.forward == nil {
		t.Fatal("a container was not forwarded")
	}
	defer c.forward.close()
	host, port, _ := net.SplitHostPort(c.forward.addr())
	if host != "127.0.0.1" {
		t.Errorf("the container forward listens on %s, want the loopback", c.forward.addr())
	}
	if want := "tcp://host.docker.internal:" + port; got != want {
		t.Errorf("container engine: %q, want %q", got, want)
	}
}

// fakeContainerEngine stands in for docker: `exec -i <name> buildctl
// dial-stdio` echoes its stdin, as an engine answering every line would,
// and records its arguments and pid beside itself.
func fakeContainerEngine(t *testing.T) string {
	t.Helper()
	return agenttest.Script(t, "docker", `here=$(dirname "$0")
echo "$*" >> "$here/docker-args.txt"
echo $$ >> "$here/docker-pids.txt"
exec cat
`)
}

// A docker-container engine is reached through `docker exec -i <name>
// buildctl dial-stdio`, one process per connection, and once the forward
// is closed it is not reached at all: the connection still open ends, its
// process is gone and the port takes no more.
func TestForwardContainer(t *testing.T) {
	docker := fakeContainerEngine(t)
	f, err := forwardContainer("127.0.0.1:0", docker, "dagger-engine-v0.20.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := echo(t, f.addr(), "ping"); err != nil || got != "ping" {
		t.Fatalf("echo: %q, %v", got, err)
	}
	c, err := net.Dial("tcp", f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	if line, err := r.ReadString('\n'); err != nil || line != "hi\n" {
		t.Fatalf("echo: %q, %v", line, err)
	}
	here := filepath.Dir(docker)
	want := "exec -i dagger-engine-v0.20.5 buildctl dial-stdio"
	if args := readLines(filepath.Join(here, "docker-args.txt")); !slices.Equal(args, []string{want, want}) {
		t.Errorf("docker calls %v, want two of %q", args, want)
	}
	f.close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadString('\n'); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the connection outlived the forward: %v", err)
	}
	if _, err := net.DialTimeout("tcp", f.addr(), time.Second); err == nil {
		t.Error("the forward's port still takes connections")
	}
	for _, line := range readLines(filepath.Join(here, "docker-pids.txt")) {
		pid, err := strconv.Atoi(line)
		if err != nil {
			t.Fatalf("pid %q: %v", line, err)
		}
		if err := syscall.Kill(pid, 0); !errors.Is(err, syscall.ESRCH) {
			t.Errorf("process %d outlived the forward: %v", pid, err)
		}
	}
}

// A container engine that will not start is logged with the engine, and
// the forward goes on accepting.
func TestForwardContainerLogsAFailedDial(t *testing.T) {
	var buf safeBuffer
	docker := agenttest.Script(t, "docker", "echo 'Error: No such container: gone' >&2\nexit 1\n")
	f, err := forwardContainer("127.0.0.1:0", docker, "gone", slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	for i := 0; i < 2; i++ {
		if _, err := echo(t, f.addr(), "ping"); err == nil {
			t.Fatal("a connection to a missing container was answered")
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "No such container") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if out := buf.String(); !strings.Contains(out, "No such container") {
		t.Errorf("log does not carry the engine's error: %q", out)
	}
}

// A connection whose process exits non-zero is closed on its own: a
// connection already open to a working process keeps working, and the
// forward goes on accepting new connections.
func TestForwardContainerFailureLeavesOtherConnectionsWorking(t *testing.T) {
	docker := agenttest.Script(t, "docker", `here=$(dirname "$0")
n=$(($(cat "$here/docker-count.txt" 2>/dev/null || echo 0) + 1))
echo "$n" > "$here/docker-count.txt"
if [ "$n" = 2 ]; then
	echo boom >&2
	exit 1
fi
exec cat
`)
	f, err := forwardContainer("127.0.0.1:0", docker, "dagger-engine-v0.20.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()

	first, err := net.Dial("tcp", f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Close() }()
	if _, err := first.Write([]byte("a\n")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(first)
	if line, err := r.ReadString('\n'); err != nil || line != "a\n" {
		t.Fatalf("first connection: %q, %v", line, err)
	}

	if _, err := echo(t, f.addr(), "ping"); err == nil {
		t.Error("the failing connection was answered")
	}

	if _, err := first.Write([]byte("b\n")); err != nil {
		t.Fatal(err)
	}
	if line, err := r.ReadString('\n'); err != nil || line != "b\n" {
		t.Fatalf("first connection after the second one failed: %q, %v", line, err)
	}

	if got, err := echo(t, f.addr(), "pong"); err != nil || got != "pong" {
		t.Fatalf("a new connection after the failure: %q, %v", got, err)
	}
}

// What the process writes to stderr never reaches the connection, only what
// it writes to stdout does.
func TestForwardContainerStderrNeverReachesConnection(t *testing.T) {
	docker := agenttest.Script(t, "docker", `echo clean-stdout
echo dirty-stderr >&2
`)
	f, err := forwardContainer("127.0.0.1:0", docker, "dagger-engine-v0.20.5", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	c, err := net.Dial("tcp", f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	got, err := io.ReadAll(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "clean-stdout\n" {
		t.Errorf("connection carried %q, want only the process's stdout", got)
	}
}

// Closing a forward ends the connections it carries, not only new ones.
func TestForwardCloseEndsOpenConnections(t *testing.T) {
	f, err := forwardUnix("127.0.0.1:0", echoEngine(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	c, err := net.Dial("tcp", f.addr())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if _, err := c.Write([]byte("hi\n")); err != nil {
		t.Fatal(err)
	}
	r := bufio.NewReader(c)
	if line, err := r.ReadString('\n'); err != nil || line != "hi\n" {
		t.Fatalf("echo: %q, %v", line, err)
	}
	f.close()
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, err := r.ReadString('\n'); err == nil || errors.Is(err, os.ErrDeadlineExceeded) {
		t.Errorf("the connection outlived the forward: %v", err)
	}
}

// The engine is given only in sbx, only to a profile that asks for it, and
// only the engine granted; the turn carries it.
func TestSandboxBoundaryDagger(t *testing.T) {
	work := t.TempDir()
	d := &Dagger{Engine: "unix:///run/dagger/engine.sock", Version: "v0.20.5"}
	for _, tc := range []struct {
		name    string
		sandbox string
		dagger  *Dagger
		grant   string
		want    error // nil: accepted
		words   []string
	}{
		{"asked and granted", SandboxSbx, d, d.Engine, nil, nil},
		{"asked, not granted", SandboxSbx, d, "", ErrNotGranted, []string{"profile asks for the Dagger engine unix:///run/dagger/engine.sock"}},
		{"asked, another granted", SandboxSbx, d, "tcp://127.0.0.1:1234", ErrNotGranted, []string{"profile asks for the Dagger engine"}},
		{"granted, not asked", SandboxSbx, nil, d.Engine, ErrUnsupported, []string{"granted and the profile does not ask for it"}},
		{"granted to a container", SandboxContainer, nil, d.Engine, ErrUnsupported, []string{"granted and the profile does not ask for it"}},
		{"asked in a container", SandboxContainer, d, d.Engine, ErrUnsupported, []string{`sandbox "container" cannot give a session the Dagger engine`}},
		{"asked on the host", SandboxNone, d, d.Engine, ErrUnsupported, []string{`sandbox "none" cannot give a session the Dagger engine`}},
		{"neither", SandboxSbx, nil, "", nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := grantAll(Request{Workspace: fakeWorkspace{dir: work}, SessionDir: work, Profile: Profile{Sandbox: tc.sandbox, SandboxImage: "img", Dagger: tc.dagger}})
			req.Grants.DaggerEngine = tc.grant
			turn, err := (&Runner{}).Verify(req)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				if tc.dagger != nil && turn.DaggerEngine != tc.grant {
					t.Errorf("turn engine %q, want %q", turn.DaggerEngine, tc.grant)
				}
				if tc.dagger == nil && turn.DaggerEngine != "" {
					t.Errorf("turn engine %q for a profile that did not ask", turn.DaggerEngine)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
			for _, w := range tc.words {
				if !strings.Contains(err.Error(), w) {
					t.Errorf("refusal %q does not say %q", err, w)
				}
			}
		})
	}
}

// Dagger is an sbx option with an engine address and a release the install
// script takes; every agent has a template.
func TestDaggerProfileValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    Profile
		want string
	}{
		{"socket", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "unix:///run/dagger.sock", Version: "v0.20.5"}}, ""},
		{"tcp", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "tcp://127.0.0.1:1234", Version: "0.20.5-rc.1"}}, ""},
		{"container", Profile{Sandbox: SandboxContainer, SandboxImage: "img", Dagger: &Dagger{Engine: "unix:///s", Version: "v0.20.5"}}, `sandbox "sbx" only`},
		{"relative socket", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "unix://run/s", Version: "v0.20.5"}}, "unix://<absolute path"},
		{"no scheme", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "/run/s", Version: "v0.20.5"}}, "unix://<absolute path"},
		{"no port", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "tcp://127.0.0.1", Version: "v0.20.5"}}, "tcp://<host>:<port>"},
		{"named port", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "tcp://127.0.0.1:dagger", Version: "v0.20.5"}}, "tcp://<host>:<port>"},
		{"path after the port", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "tcp://h:1234/path", Version: "v0.20.5"}}, "tcp://<host>:<port>"},
		{"port out of range", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "tcp://h:99999", Version: "v0.20.5"}}, "tcp://<host>:<port>"},
		{"port zero", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "tcp://h:0", Version: "v0.20.5"}}, "tcp://<host>:<port>"},
		{"docker container", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://dagger-engine-v0.20.5", Version: "v0.20.5"}}, ""},
		{"minimal container name", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://a1", Version: "v0.20.5"}}, ""},
		{"container name with dot underscore and dash", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://x.y_z-0", Version: "v0.20.5"}}, ""},
		{"empty container name", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"one-character container name", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://a", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name like a flag", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://-it", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a leading dot", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://.hidden", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a leading underscore", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://_hidden", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a leading slash", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container:///hidden", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a space", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://dagger engine", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a slash", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://dagger/engine", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a shell", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://e;rm", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a dollar sign", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://e$cho", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a port", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://dagger-engine:1234", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a query", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://dagger-engine?q=1", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with a fragment", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://dagger-engine#f", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"container name with userinfo", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-container://user@dagger-engine", Version: "v0.20.5"}}, "docker-container://<container name>"},
		{"docker image", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "docker-image://registry.dagger.io/engine", Version: "v0.20.5"}}, "unix://"},
		{"no version", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "unix:///s"}}, "Dagger CLI version"},
		{"shell in version", Profile{Sandbox: SandboxSbx, Dagger: &Dagger{Engine: "unix:///s", Version: "0.20.5; rm -rf /"}}, "Dagger CLI version"},
	} {
		err := tc.p.Validate()
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: refused: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: error %v does not mention %q", tc.name, err, tc.want)
		case tc.want != "" && tc.p.Dagger != nil && strings.HasPrefix(tc.p.Dagger.Engine, "docker-container://") && !strings.Contains(err.Error(), tc.p.Dagger.Engine):
			t.Errorf("%s: error %v does not name the address %q", tc.name, err, tc.p.Dagger.Engine)
		}
	}
	// Every agent but pi, which sbx has no template for: Validate
	// refuses pi in sbx for that reason.
	for _, a := range Agents {
		if a == AgentPi {
			if SbxTemplates[a] != "" {
				t.Errorf("agent pi has an sbx template %q, and the pi sandbox rules say it has none", SbxTemplates[a])
			}
			continue
		}
		if SbxTemplates[a] == "" {
			t.Errorf("agent %s has no sbx template", a)
		}
	}
}

// A connection that ends is forgotten at once, not kept until the session
// ends.
func TestForwardForgetsClosedConnections(t *testing.T) {
	f, err := forwardUnix("127.0.0.1:0", echoEngine(t), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	for i := 0; i < 3; i++ {
		if got, err := echo(t, f.addr(), "ping"); err != nil || got != "ping" {
			t.Fatalf("echo %d: %q, %v", i, got, err)
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for f.open() > 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := f.open(); n != 0 {
		t.Errorf("%d closed connections are still held", n)
	}
}

// A connection the engine's socket refuses is logged with the socket's
// path, and the forward goes on accepting.
func TestForwardLogsARefusedSocket(t *testing.T) {
	var buf safeBuffer
	missing := filepath.Join(t.TempDir(), "gone.sock")
	f, err := forwardUnix("127.0.0.1:0", missing, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer f.close()
	if _, err := echo(t, f.addr(), "ping"); err == nil {
		t.Fatal("a connection to a missing socket was answered")
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), missing) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if out := buf.String(); !strings.Contains(out, "Dagger engine") || !strings.Contains(out, missing) {
		t.Errorf("log does not name the socket: %q", out)
	}
}

// safeBuffer is a bytes.Buffer the forward's goroutine and the test share.
type safeBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *safeBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *safeBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
