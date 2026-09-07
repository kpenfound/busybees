package procs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// A session in the container sandbox is found differently from one on the
// host: its agent runs in the container's own pid namespace, where neither
// a pid file nor the process table reaches it, and the process the host
// does see — the engine client — leaves the container running when it is
// killed. So the container itself is what is found and stopped, from the
// two records the runner leaves, mirroring the two a session on the host
// leaves: the id file in the session directory (the pid file's counterpart)
// and the label the container carries (the session marker's), whose value
// is the session directory and so scopes a container to one factory.
//
// Such a session leaves a third record, for a third thing to stop: the
// built-in MCP server the runner starts on the host for it, because the
// bees binary is not in the container (see ServerPIDFile).

// ContainerIDFile is the file in a session directory the engine writes the
// container's id to (--cidfile) when a container-backed session starts. It
// is removed when the session ends, so a session directory holding one is a
// session whose container may still be running.
const ContainerIDFile = "container-id"

// ContainerLabel is the label every container-backed session's container
// carries, with the session directory as its value, so `docker ps --filter
// label=bees.session` lists one factory's sessions and the value says which
// session each one is.
const ContainerLabel = "bees.session"

// ServerPIDFile is the file in a session directory the runner writes the
// pid of the built-in MCP server it started on the host for a
// container-backed session to. The server runs in a process group of its
// own, outside the session's and outside the scheduler's, so nothing else
// reaches it: a crash that takes the scheduler down without running its
// deferred cleanup leaves the server running, holding its port and serving
// the factory's tools, and this file is the only record of it.
//
// It is a file of its own rather than PIDFile, which names the session's
// main command: a container session leaves both.
const ServerPIDFile = "mcp-server-pid"

// WriteServerPID records the pid of the host-side MCP server of a
// container-backed session.
func WriteServerPID(dir string, pid int) error {
	return os.WriteFile(filepath.Join(dir, ServerPIDFile), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// ServerPID returns the pid of the host-side MCP server a session directory
// records, 0 when the session runs none.
func ServerPID(dir string) int {
	b, err := os.ReadFile(filepath.Join(dir, ServerPIDFile))
	if err != nil {
		return 0
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	return pid
}

// RemoveServerPID deletes a session's server pid file.
func RemoveServerPID(dir string) { _ = os.Remove(filepath.Join(dir, ServerPIDFile)) }

// liveServer returns the pid of the host-side MCP server a session
// directory records, when that process is still running; a file naming a
// process that has gone is deleted, as a stale pid file is.
//
// Unlike the session's main pid, the server's is not cross-checked against
// the process table: `bees mcp serve` is deliberately not one of the
// executables the scan counts (it is no agent session), so the scan can say
// nothing about it.
func liveServer(dir string) int {
	pid := ServerPID(dir)
	if pid <= 0 {
		return 0
	}
	if !Alive(pid) {
		RemoveServerPID(dir)
		return 0
	}
	return pid
}

// containerMarker is the argv fragment that identifies the engine client of
// a container-backed session in the process table.
const containerMarker = "--label " + ContainerLabel + "="

// Engine is the container engine command asked which containers are running
// and told to remove one. It is a variable so a test can answer for a
// machine that has no container engine; nothing but a test changes it.
var Engine = config.ContainerEngine

// containerStop is how long the engine is given to remove a container.
const containerStop = 30 * time.Second

// ContainerID returns the id of the container a session directory records,
// empty when the session does not run in one.
func ContainerID(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, ContainerIDFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// RemoveContainerID deletes a session's container id file.
func RemoveContainerID(dir string) { _ = os.Remove(filepath.Join(dir, ContainerIDFile)) }

// FromContainers returns the container-backed sessions of the factory whose
// sessions live in sessionsDir, by asking the engine which of its running
// containers carry ContainerLabel with a session directory of this factory.
// The engine is the authority the process table is for a session on the
// host: a session directory recording a container the engine does not list
// is a session that has ended, and its stale id file is deleted the way
// FromPIDFile deletes the pid file of a process that is gone.
//
// It fails when there is no engine to ask, which is also the answer to
// "are any container sessions running": none that can be found or stopped.
func FromContainers(ctx context.Context, sessionsDir string) ([]Proc, error) {
	running, err := runningContainers(ctx, sessionsDir)
	if err != nil {
		return nil, err
	}
	removeStaleContainerIDs(sessionsDir, running)
	out := make([]Proc, 0, len(running))
	for dir, id := range running {
		out = append(out, Proc{Container: id, SessionDir: dir, Source: "container"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionDir < out[j].SessionDir })
	return out, nil
}

// runningContainers asks the engine which of its running containers are
// sessions of this factory, as session directory -> container id. A
// container labelled with another factory's session directory is not this
// factory's and is left alone, however many factories share a machine.
func runningContainers(ctx context.Context, sessionsDir string) (map[string]string, error) {
	prefixes := scopePrefixes(sessionsDir)
	if len(prefixes) == 0 {
		return nil, nil // no scope, no attribution: match nothing rather than everything
	}
	out, err := exec.CommandContext(ctx, Engine, "ps", "--no-trunc",
		"--filter", "label="+ContainerLabel,
		"--format", "{{.ID}}\t{{.Label \""+ContainerLabel+"\"}}").Output()
	if err != nil {
		return nil, fmt.Errorf("list the container sessions (%s ps): %w", Engine, err)
	}
	found := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		id, dir, ok := strings.Cut(strings.TrimSpace(line), "\t")
		if !ok || id == "" || dir == "" {
			continue
		}
		if !underScope(dir, prefixes) {
			continue
		}
		found[filepath.Clean(dir)] = id
	}
	return found, nil
}

// removeStaleContainerIDs deletes the container id file of every session
// directory whose container the engine no longer lists.
func removeStaleContainerIDs(sessionsDir string, running map[string]string) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(sessionsDir, e.Name())
		if ContainerID(dir) == "" {
			continue
		}
		if _, ok := containerFor(running, dir); !ok {
			RemoveContainerID(dir)
		}
	}
}

// containerFor returns the running container of a session directory.
func containerFor(running map[string]string, dir string) (string, bool) {
	for d, id := range running {
		if sameDir(d, dir) {
			return id, true
		}
	}
	return "", false
}

// underScope reports whether a session directory lies under one of the
// prefixes that attribute it to this factory.
func underScope(dir string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(dir, p) {
			return true
		}
	}
	return false
}

// sameDir reports whether two paths name the same directory, as given or
// resolved: a container's label carries the session directory as bees knew
// it, which need not be the path the caller of Find gave.
func sameDir(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// removeContainer stops and removes a session's container, as the runner
// does when it stops a session of its own. A container that is already gone
// is not an error: the session ended between finding it and stopping it.
func removeContainer(id string) error {
	ctx, cancel := context.WithTimeout(context.Background(), containerStop)
	defer cancel()
	out, err := exec.CommandContext(ctx, Engine, "rm", "--force", id).CombinedOutput()
	if err == nil || strings.Contains(strings.ToLower(string(out)), "no such container") {
		return nil
	}
	return fmt.Errorf("%s rm --force %s: %w: %s", Engine, id, err, strings.TrimSpace(string(out)))
}
