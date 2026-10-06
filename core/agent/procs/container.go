package procs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
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
// When the caller supplies a host MCP server, the session leaves a third
// record for that process. The caller binary runs outside the container
// (see ServerPIDFile).

// ContainerIDFile is the file in a session directory the engine writes the
// container's id to (--cidfile) when a container-backed session starts. It
// is removed when the session ends, so a session directory holding one is a
// session whose container may still be running.
const ContainerIDFile = "container-id"

// ContainerLabel is the label every container-backed session's container
// carries, with the session directory as its value, so `docker ps --filter
// label=agent.session` lists one factory's sessions and the value says which
// session each one is.
const ContainerLabel = "agent.session"

// ServerPIDFile is the file in a session directory the runner writes the
// pid of the optional caller-supplied MCP server it started on the host for a
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
// the process table: the caller server is deliberately not one of the
// executables the scan counts (it is no agent session), so the scan can say
// nothing about it.
//
// A read-only finder deletes nothing: the stale file stays and the server
// is still reported as gone.
func (f Finder) liveServer(dir string) int {
	pid := ServerPID(dir)
	if pid <= 0 {
		return 0
	}
	if !Alive(pid) {
		if !f.ReadOnly {
			RemoveServerPID(dir)
		}
		return 0
	}
	return pid
}

// Engine is the container engine command asked which containers are running
// and told to remove one. It is a variable so a test can answer for a
// machine that has no container engine; nothing but a test changes it.
var Engine = "docker"

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

// SandboxNameFile is the file in a session directory the runner writes the
// name of the Docker Sandbox (sbx) a sandbox-backed session runs in. It is
// removed with the sandbox when the session ends, so a session directory
// holding one is a session whose sandbox may still exist. CleanSandboxes and
// CleanSandboxDirs read it to find that sandbox and remove it with `sbx rm
// --force <name>`, which also drops the sandbox's network policy rules: sbx
// removes those with the sandbox itself, so cleanup makes no separate `sbx
// policy rm` call.
const SandboxNameFile = "sandbox-name"

// WriteSandboxName records the name of a session's Docker Sandbox.
func WriteSandboxName(dir, name string) error {
	return os.WriteFile(filepath.Join(dir, SandboxNameFile), []byte(name+"\n"), 0o644)
}

// SandboxName returns the name of the Docker Sandbox a session directory
// records, empty when the session does not run in one.
func SandboxName(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, SandboxNameFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// RemoveSandboxName deletes a session's sandbox name file.
func RemoveSandboxName(dir string) { _ = os.Remove(filepath.Join(dir, SandboxNameFile)) }

// SandboxWorkspaceFile is the file in a session directory the runner writes
// the absolute path of a sandbox-backed session's primary workspace (the
// directory made under os.TempDir() in place of the working directory, when
// the working directory itself cannot be the primary workspace) to, once
// that directory exists. It is removed with the workspace when the session
// ends, so a session directory holding one is a session whose primary
// workspace may still exist on disk. CleanSandboxes and CleanSandboxDirs
// read it and remove that directory tree, subject to the workspace path
// rule (see validWorkspacePath).
const SandboxWorkspaceFile = "sandbox-workspace"

// SandboxWorkspace returns the absolute path of the primary workspace a
// session directory records, empty when the session recorded none.
func SandboxWorkspace(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, SandboxWorkspaceFile))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// sandboxRemoveTimeout is how long sbx is given to remove a sandbox.
const sandboxRemoveTimeout = 60 * time.Second

// CleanSandboxes removes the orphaned Docker Sandbox, and the orphaned
// primary workspace, of every session directory held in sessionsDir, by
// collecting its directory entries and asking CleanSandboxDirs about them,
// the way FromPIDFiles asks FromPIDFile about every entry in turn. sbxBin is
// the sbx binary to run — a caller's Runner.SbxBin, or agent.SandboxCLI by
// default — because this package cannot import core/agent. It must run at
// startup, before any session starts, so that every sandbox-name or
// primary-workspace record it finds belongs to a session an earlier process
// died without closing, never one still in use.
func CleanSandboxes(ctx context.Context, sbxBin, sessionsDir string) error {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	var dirs []string
	for _, e := range entries {
		if e.IsDir() {
			dirs = append(dirs, filepath.Join(sessionsDir, e.Name()))
		}
	}
	return CleanSandboxDirs(ctx, sbxBin, dirs)
}

// CleanSandboxDirs is CleanSandboxes for a caller whose sessions do not all
// live under one sessions directory: every directory given is attempted in
// turn, through cleanSandboxDir, and the per-directory errors are joined
// (errors.Join), each naming the session directory and the sandbox or
// record it concerns, so that one directory's failure never keeps the
// others from being cleaned up.
func CleanSandboxDirs(ctx context.Context, sbxBin string, dirs []string) error {
	var errs []error
	for _, dir := range dirs {
		if err := cleanSandboxDir(ctx, sbxBin, dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// cleanSandboxDir is the per-directory pass CleanSandboxDirs makes: it
// removes the session's sandbox (cleanSandboxName) and its primary
// workspace (cleanSandboxWorkspace) independently, so that a session
// directory holding a workspace record but no sandbox-name record (the
// process died before the sandbox was named) still has its workspace
// cleaned up, and a failure in one does not stop the other.
func cleanSandboxDir(ctx context.Context, sbxBin, dir string) error {
	return errors.Join(cleanSandboxName(ctx, sbxBin, dir), cleanSandboxWorkspace(dir))
}

// cleanSandboxName is the sandbox-name half of cleanSandboxDir. A directory
// holding no sandbox-name record is left untouched. One whose record is
// empty or starts with '-' is reported as an error and kept: sbx would read
// such a name as a flag, so it is never passed to sbx. Otherwise `sbx rm
// --force <name>` is run — the one sbx call this makes, which takes the
// sandbox's network policy rules with it — and the record is deleted only
// once that call succeeds, so a failure leaves it for a later call to
// retry.
func cleanSandboxName(ctx context.Context, sbxBin, dir string) error {
	if _, err := os.Stat(filepath.Join(dir, SandboxNameFile)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("session %s: read sandbox record: %w", dir, err)
	}
	name := SandboxName(dir)
	if name == "" || strings.HasPrefix(name, "-") {
		return fmt.Errorf("session %s: sandbox record %q is empty or starts with '-', which %s would read as a flag", dir, name, sbxBin)
	}
	rmCtx, cancel := context.WithTimeout(ctx, sandboxRemoveTimeout)
	defer cancel()
	out, err := agentbin.CommandContext(rmCtx, sbxBin, "rm", "--force", name).CombinedOutput()
	if err != nil {
		return fmt.Errorf("session %s: remove sandbox %s (%s rm --force %s): %w: %s", dir, name, sbxBin, name, err, strings.TrimSpace(string(out)))
	}
	RemoveSandboxName(dir)
	return nil
}

// validWorkspacePath reports whether p is safe for cleanSandboxWorkspace to
// remove: the workspace path rule. cleanup is not given the Runner's
// NamePrefix (it may be the default "agent-", a caller's own value, or
// empty), so the rule accepts any prefix before "sbx-primary-" and checks
// only what every core/agent primary workspace has in common: p is
// absolute; its cleaned parent directory is os.TempDir(); and its base name
// contains the literal substring "sbx-primary-" with at least one character
// following its last occurrence. A forged prefix gains an attacker nothing:
// the path must still name a directory sitting directly under os.TempDir()
// whose name contains "sbx-primary-".
func validWorkspacePath(p string) bool {
	if !filepath.IsAbs(p) {
		return false
	}
	clean := filepath.Clean(p)
	if filepath.Dir(clean) != filepath.Clean(os.TempDir()) {
		return false
	}
	b := filepath.Base(clean)
	i := strings.LastIndex(b, "sbx-primary-")
	return i >= 0 && len(b) > i+len("sbx-primary-")
}

// removeWorkspace removes a primary workspace's directory tree. It is a
// variable, the way Engine is, so a test can make it fail deterministically
// without depending on filesystem permissions a root test runner ignores;
// nothing but a test changes it.
var removeWorkspace = os.RemoveAll

// cleanSandboxWorkspace is the primary-workspace half of cleanSandboxDir. A
// directory holding no workspace record is left untouched. A record whose
// path fails validWorkspacePath is reported as an error and kept, and
// nothing is deleted. Otherwise the workspace directory tree is removed (an
// already-absent directory counts as removed) and the record is deleted
// only once that succeeds, so a failure leaves it for a later call to
// retry.
func cleanSandboxWorkspace(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, SandboxWorkspaceFile)); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("session %s: read workspace record: %w", dir, err)
	}
	path := SandboxWorkspace(dir)
	if !validWorkspacePath(path) {
		return fmt.Errorf("session %s: workspace record %q fails the workspace path rule", dir, path)
	}
	if err := removeWorkspace(path); err != nil {
		return fmt.Errorf("session %s: remove workspace %s: %w", dir, path, err)
	}
	_ = os.Remove(filepath.Join(dir, SandboxWorkspaceFile))
	return nil
}

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
func FromContainers(ctx context.Context, sessionsDir string, markers ...Markers) ([]Proc, error) {
	return withMarkers(markers).FromContainers(ctx, sessionsDir)
}

// FromContainers is FromContainers with the finder's options: a read-only
// finder deletes no stale container id file.
func (f Finder) FromContainers(ctx context.Context, sessionsDir string) ([]Proc, error) {
	running, err := runningContainers(ctx, sessionsDir, f.markers())
	if err != nil {
		return nil, err
	}
	f.removeStaleContainerIDs(sessionsDir, running)
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
func runningContainers(ctx context.Context, sessionsDir string, markers ...Markers) (map[string]string, error) {
	label := markerSet(markers).Container
	prefixes := scopePrefixes(sessionsDir)
	if len(prefixes) == 0 {
		return nil, nil // no scope, no attribution: match nothing rather than everything
	}
	out, err := agentbin.CommandContext(ctx, Engine, "ps", "--no-trunc",
		"--filter", "label="+label,
		"--format", "{{.ID}}\t{{.Label \""+label+"\"}}").Output()
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
// directory whose container the engine no longer lists. A read-only finder
// deletes none of them.
func (f Finder) removeStaleContainerIDs(sessionsDir string, running map[string]string) {
	if f.ReadOnly {
		return
	}
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
// resolved: a container's label carries the session directory as the caller knew
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
	out, err := agentbin.CommandContext(ctx, Engine, "rm", "--force", id).CombinedOutput()
	if err == nil || strings.Contains(strings.ToLower(string(out)), "no such container") {
		return nil
	}
	return fmt.Errorf("%s rm --force %s: %w: %s", Engine, id, err, strings.TrimSpace(string(out)))
}
