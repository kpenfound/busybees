package procs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	ossignal "os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeEngine writes a shell script standing in for the container engine and
// points Engine at it for the test: `ps` prints the lines it is given, `rm`
// records its arguments, and anything else fails. A real engine is never
// run, on a machine that has one or one that does not.
func fakeEngine(t *testing.T, psOutput string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "docker")
	script := `#!/bin/sh
case "$1" in
ps) cat "$(dirname "$0")/ps.txt" ;;
rm) shift; echo "$@" >> "$(dirname "$0")/rm.txt" ;;
*) echo "unexpected: $@" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ps.txt"), []byte(psOutput), 0o644); err != nil {
		t.Fatal(err)
	}
	old := Engine
	Engine = bin
	t.Cleanup(func() { Engine = old })
	return dir
}

// removals is what the fake engine was told to remove.
func removals(t *testing.T, engineDir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(engineDir, "rm.txt"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func writeContainerID(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ContainerIDFile), []byte(id+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// A container-backed session is found through its container, which the
// engine is asked for by label: the agent runs in the container's own pid
// namespace, where the pid file and the process table do not reach it. The
// label's value is the session directory, which is also what scopes a
// container to one factory.
func TestFromContainers(t *testing.T) {
	sessions := t.TempDir()
	mine := filepath.Join(sessions, "20260906-developer-issue-1-r1")
	writeContainerID(t, mine, "aaa111")
	// A session of this factory whose container has ended: --rm removed it
	// when the agent exited, and the id file it left behind is stale.
	ended := filepath.Join(sessions, "20260906-qa-2")
	writeContainerID(t, ended, "ccc333")

	other := filepath.Join(t.TempDir(), "20260906-developer-issue-9-r1")
	engine := fakeEngine(t, "aaa111\t"+mine+"\nbbb222\t"+other+"\n")

	got, err := FromContainers(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Container != "aaa111" || got[0].SessionDir != mine || got[0].Source != "container" {
		t.Fatalf("FromContainers: %+v, want the one container of this factory", got)
	}
	if _, err := os.Stat(filepath.Join(ended, ContainerIDFile)); !os.IsNotExist(err) {
		t.Error("the id file of a session whose container has ended was not removed")
	}
	if _, err := os.Stat(filepath.Join(mine, ContainerIDFile)); err != nil {
		t.Errorf("the id file of a running container was removed: %v", err)
	}
	if got := removals(t, engine); got != nil {
		t.Errorf("finding sessions removed containers: %v", got)
	}
}

// No sessions directory to scope by attributes nothing, rather than every
// container on the machine.
func TestFromContainersWithoutScopeMatchesNothing(t *testing.T) {
	fakeEngine(t, "aaa111\t/a/.bees/sessions/20260906-qa-1\n")
	got, err := FromContainers(context.Background(), "")
	if err != nil || len(got) != 0 {
		t.Fatalf("empty scope: %+v %v", got, err)
	}
}

// There is no engine to ask on a machine without one, which is also the
// answer to whether it runs container sessions. Find must still report the
// sessions it finds on the host.
func TestFindWithoutAnEngine(t *testing.T) {
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "20260906-qa-1")
	writeContainerID(t, dir, "aaa111")
	old := Engine
	Engine = filepath.Join(t.TempDir(), "no-such-engine")
	t.Cleanup(func() { Engine = old })

	if _, err := FromContainers(context.Background(), sessions); err == nil {
		t.Error("FromContainers reported success without an engine to ask")
	}
	if _, err := Find(context.Background(), sessions); err != nil {
		t.Fatalf("Find: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ContainerIDFile)); err != nil {
		t.Error("an unanswerable engine removed a session's container id file")
	}
}

// Stopping a container-backed session removes its container and stops the
// engine client that started it: killing the client alone leaves the
// container, and the agent in it, running.
func TestKillRemovesTheContainer(t *testing.T) {
	engine := fakeEngine(t, "")
	dir := t.TempDir()
	writeContainerID(t, dir, "aaa111")

	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	if err := WritePID(dir, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}

	p, ok := FromPIDFile(dir, nil)
	if !ok || p.Container != "aaa111" {
		t.Fatalf("FromPIDFile: %+v %v, want the session's container", p, ok)
	}
	if err := Kill(p, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if got := removals(t, engine); len(got) != 1 || got[0] != "--force aaa111" {
		t.Errorf("engine calls: %v, want the container removed", got)
	}
	done := make(chan struct{})
	go func() { _, _ = cmd.Process.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the engine client is still running after the kill")
	}
	if _, err := os.Stat(filepath.Join(dir, ContainerIDFile)); !os.IsNotExist(err) {
		t.Error("the container id file should be removed after the kill")
	}
	if _, err := os.Stat(filepath.Join(dir, PIDFile)); !os.IsNotExist(err) {
		t.Error("the pid file should be removed after the kill")
	}
}

// A container found without a live engine client has no process to signal.
// Signalling pid 0 is signalling the caller's own process group, so the
// test asks to be told about a SIGTERM rather than trusting it not to
// arrive.
func TestKillingAContainerAloneSignalsNothing(t *testing.T) {
	engine := fakeEngine(t, "")
	sigs := make(chan os.Signal, 1)
	ossignal.Notify(sigs, syscall.SIGTERM)
	t.Cleanup(func() { ossignal.Stop(sigs) })

	dir := t.TempDir()
	writeContainerID(t, dir, "aaa111")
	if err := Kill(Proc{Container: "aaa111", SessionDir: dir, Source: "container"}, time.Second); err != nil {
		t.Fatal(err)
	}
	if got := removals(t, engine); len(got) != 1 || got[0] != "--force aaa111" {
		t.Errorf("engine calls: %v, want the container removed", got)
	}
	select {
	case s := <-sigs:
		t.Fatalf("killing a container alone signalled this process group (%v)", s)
	case <-time.After(200 * time.Millisecond):
	}
	if _, err := os.Stat(filepath.Join(dir, ContainerIDFile)); !os.IsNotExist(err) {
		t.Error("the container id file should be removed after the kill")
	}
}

// A container whose engine client has gone — killed on its own, or lost
// with the machine — is a session of its own for `bees kill` to stop, and
// it names the session directory it belongs to so the session can be
// marked as stopped.
func TestFindReportsAContainerWhoseClientIsGone(t *testing.T) {
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "20260906-developer-issue-1-r1")
	writeContainerID(t, dir, "aaa111")
	// A pid file naming a process that has gone, as a crash leaves behind.
	if err := WritePID(dir, 999999); err != nil {
		t.Fatal(err)
	}
	fakeEngine(t, "aaa111\t"+dir+"\n")

	found, err := Find(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 {
		t.Fatalf("Find: %+v, want the orphaned container", found)
	}
	if found[0].Container != "aaa111" || found[0].SessionDir != dir || found[0].PID != 0 {
		t.Errorf("Find: %+v, want the container with no process", found[0])
	}
}

// The engine client of a container-backed session is what the process table
// shows of it. It counts as a session — so a pid file naming it is not
// discarded as a reused pid — and only with the label bees puts on a
// session's container, so another container of the machine is left alone.
func TestParsePSFindsTheEngineClient(t *testing.T) {
	scope := "/a/.bees/sessions"
	client := func(pid int, sessionsDir, name string) string {
		dir := sessionsDir + "/20260906-" + name
		return fmt.Sprintf("  %d   %d /usr/local/bin/docker run --rm --interactive", pid, pid) +
			" --name bees-" + name + "-ab12 --cidfile " + dir + "/container-id" +
			" --label bees.session=" + dir + " ghcr.io/acme/bees:1 claude -p --name bees-" + name
	}
	text := strings.Join([]string{
		client(100, scope, "developer-issue-1-r1"),
		// Another project's factory.
		client(200, "/b/.bees/sessions", "developer-issue-9-r1"),
		// The engine running something that is not a bees session, with a
		// session directory of this factory on its command line all the
		// same.
		"  300   300 docker run --rm --name bees-x -v /a/.bees/sessions/x:/s alpine sh",
	}, "\n") + "\n"

	got := parsePS(text, 1, scope)
	if len(got) != 1 || got[0].PID != 100 {
		t.Fatalf("parsePS: %+v, want the engine client of this factory alone", got)
	}
}
