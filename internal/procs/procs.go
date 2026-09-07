// Package procs finds and stops agent sessions started by bees, for
// `bees kill` after a crash.
//
// Sessions are found two ways: the pid file the runner writes in each
// session directory, and a scan of the process table for claude and codex
// processes carrying a session marker: the `--name bees-…` argument every
// claude session is started with, or the override that hands a codex
// session its session directory. A session in the container sandbox has a
// third: the agent runs in the container, so the container is what is
// found and stopped, from the id file and the label the runner leaves
// (see container.go); what the process table shows of it is the engine
// client that started it, and the built-in MCP server the runner started
// for it on the host is a fourth thing to stop, recorded in a pid file of
// its own.
//
// Every source is scoped to one factory: a process only counts when its
// command line also references this state directory's sessions directory
// (a claude session's argv carries `--append-system-prompt-file
// <sessions dir>/<session>/system-prompt.md`, a codex session's the
// session directory in the same override, an engine client both the
// container's label and its id file), and a container only when the
// session directory its label carries lies under it. Another project's
// sessions are therefore never reported, however many factories share a
// machine.
package procs

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PIDFile is the file a running session's pid is written to.
const PIDFile = "pid"

// SessionMarker is the argv fragment that identifies a claude session of
// bees: the --name every one is started with.
const SessionMarker = "--name bees-"

// CodexSessionMarker is the argv fragment that identifies a codex session
// of bees: codex has no --name, so the override that gives the built-in
// MCP server the session's directory is what marks one, and its value is
// what scopes it to a factory.
const CodexSessionMarker = "mcp_servers.bees.env.BEES_SESSION_DIR="

// Proc is a process that looks like a bees session.
type Proc struct {
	PID     int
	PGID    int
	Command string
	// Source is "pidfile", "ps" or "container".
	Source string
	// SessionDir is set for sessions found through a pid file or through
	// their container, whose label carries it.
	SessionDir string
	// Container is the id of the container a session runs in, for a
	// session in the container sandbox. Stopping such a session means
	// removing the container: the agent runs inside it and outlives the
	// engine client PID names.
	Container string
	// Server is the pid of the built-in MCP server running on the host for
	// a session in the container sandbox, which has no bees binary inside.
	// It is a process group of its own, so stopping the session means
	// stopping it too (see ServerPIDFile).
	Server int
}

// WritePID records the pid of a running session.
func WritePID(sessionDir string, pid int) error {
	return os.WriteFile(filepath.Join(sessionDir, PIDFile), []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

// RemovePID deletes a session's pid file.
func RemovePID(sessionDir string) { _ = os.Remove(filepath.Join(sessionDir, PIDFile)) }

// Alive reports whether a process exists.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil || errors.Is(syscall.Kill(pid, 0), syscall.EPERM)
}

// DefaultGrace is how long a session is given to exit after SIGTERM before
// it is killed outright. Every caller that stops a session uses it: `bees
// kill` as its --grace default, and the live view's kill key.
const DefaultGrace = 5 * time.Second

// FromPIDFile returns the live session recorded in one session directory:
// the process its pid file names, the container it runs in and the
// host-side MCP server it left, the last two for a container-backed session
// only.
//
// It reports false when the directory records none of the three, so a
// session directory whose main process is gone but whose server is still
// running is still reported — with PID 0, because there is no process to
// signal and the server is stopped in its own right. That is what a crash
// leaves behind.
func FromPIDFile(dir string, known map[int]Proc) (Proc, bool) {
	server := liveServer(dir)
	pid, ok := livePID(dir, known)
	if !ok && server == 0 {
		return Proc{}, false
	}
	var pgid int
	if pid > 0 {
		pgid, _ = syscall.Getpgid(pid)
	}
	return Proc{PID: pid, PGID: pgid, Source: "pidfile", SessionDir: dir, Container: ContainerID(dir), Server: server}, true
}

// livePID returns the pid of a session's main command from its pid file.
// It reports false when the directory holds no pid file, when the process
// the file names is gone — in which case the stale file is deleted — and,
// when known is non-nil (the ps scan), when the pid is alive but is not an
// agent session: a pid reused by an unrelated process after a reboot, which
// must never be killed.
func livePID(dir string, known map[int]Proc) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, PIDFile))
	if err != nil {
		return 0, false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if !Alive(pid) {
		RemovePID(dir)
		return 0, false
	}
	if known != nil {
		if _, ok := known[pid]; !ok {
			RemovePID(dir) // alive, but not an agent session: pid reused
			return 0, false
		}
	}
	return pid, true
}

// FromPIDFiles returns live sessions recorded under sessionsDir and deletes
// pid files of processes that no longer exist, by asking FromPIDFile about
// every session directory in turn.
func FromPIDFiles(sessionsDir string, known map[int]Proc) ([]Proc, error) {
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Proc
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if p, ok := FromPIDFile(filepath.Join(sessionsDir, e.Name()), known); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// FromPS scans the process table for bees sessions: processes whose
// executable is claude or codex, whose arguments carry a session marker and
// whose command line references sessionsDir, the sessions directory of this
// factory's state directory.
func FromPS(ctx context.Context, sessionsDir string) ([]Proc, error) {
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=,command=")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return parsePS(stdout.String(), os.Getpid(), sessionsDir), nil
}

// parsePS keeps the agent processes of the factory whose sessions live in
// scope. scope is matched as a path prefix (with a trailing separator), so
// a sibling directory such as `<state>/sessions-old` is not a hit. Because
// macOS reports /private/var for /var and a state directory may be reached
// through a symlink, both the path as given and its resolved form are
// accepted.
func parsePS(text string, self int, scope string) []Proc {
	prefixes := scopePrefixes(scope)
	var out []Proc
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		pgid, err2 := strconv.Atoi(fields[1])
		if err1 != nil || err2 != nil || pid == self {
			continue
		}
		command := strings.Join(fields[2:], " ")
		// Only the agent executable itself (or an interpreter running a
		// claude script), never a shell or editor whose command line merely
		// mentions the marker.
		if !isSessionProcess(fields[2:], command) || !hasMarker(command) {
			continue
		}
		if !inScope(command, prefixes) {
			continue
		}
		out = append(out, Proc{PID: pid, PGID: pgid, Command: command, Source: "ps"})
	}
	return out
}

// scopePrefixes returns the path prefixes that attribute a command line to
// this factory: the sessions directory as given and, when it differs, its
// symlink-resolved form, each with a trailing separator.
func scopePrefixes(sessionsDir string) []string {
	if sessionsDir == "" {
		return nil // no scope, no attribution: match nothing rather than everything
	}
	sep := string(os.PathSeparator)
	out := []string{strings.TrimSuffix(sessionsDir, sep) + sep}
	if resolved, err := filepath.EvalSymlinks(sessionsDir); err == nil {
		if p := strings.TrimSuffix(resolved, sep) + sep; p != out[0] {
			out = append(out, p)
		}
	}
	return out
}

// inScope reports whether command references one of the prefixes.
func inScope(command string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.Contains(command, p) {
			return true
		}
	}
	return false
}

// hasMarker reports whether a command line carries one of the session
// markers as an argument of its own.
func hasMarker(command string) bool {
	return strings.Contains(command, " "+SessionMarker) || strings.Contains(command, " "+CodexSessionMarker)
}

// isSessionProcess reports whether argv starts a process that runs a
// session: the claude or codex executable, directly or through an
// interpreter (node/bun/sh script), or the container engine running a
// container-backed session, whose agent is in the container and never in
// the process table. The engine counts only when the command line carries
// the label bees puts on a session's container, so another container of
// the machine is never taken for one.
func isSessionProcess(argv []string, command string) bool {
	engine := filepath.Base(Engine)
	for i, a := range argv[:min(2, len(argv))] {
		switch base := filepath.Base(a); base {
		case "claude", "codex":
			return true
		case engine:
			return strings.Contains(command, " "+containerMarker)
		}
		if i == 0 && strings.HasPrefix(a, "-") {
			return false
		}
	}
	return false
}

// Find merges pid-file and ps results, de-duplicated by pid, for the factory
// whose sessions live in sessionsDir. Pid files are cross-checked against
// the process table when it is available.
func Find(ctx context.Context, sessionsDir string) ([]Proc, error) {
	byPID := map[int]Proc{}
	fromPS, psErr := FromPS(ctx, sessionsDir)
	var known map[int]Proc
	if psErr == nil {
		known = map[int]Proc{}
		for _, p := range fromPS {
			known[p.PID] = p
		}
	}
	// The engine's containers are asked for before the pid files, because
	// asking clears the id file of a session whose container has gone. A
	// machine with no container engine has no container session either, so
	// the error is the empty answer.
	fromContainers, _ := FromContainers(ctx, sessionsDir)
	fromFiles, err := FromPIDFiles(sessionsDir, known)
	if err != nil {
		return nil, err
	}
	// A session directory whose main process is gone but which still
	// records a live MCP server has no pid to key on, and two of them would
	// collide on 0: they are their own entries.
	var serverOnly []Proc
	for _, p := range fromFiles {
		if p.PID > 0 {
			byPID[p.PID] = p
			continue
		}
		serverOnly = append(serverOnly, p)
	}
	for _, p := range fromPS {
		if existing, ok := byPID[p.PID]; ok {
			existing.Command = p.Command
			byPID[p.PID] = existing
			continue
		}
		byPID[p.PID] = p
	}
	out := make([]Proc, 0, len(byPID)+len(serverOnly)+len(fromContainers))
	// A running container belongs to the session whose directory it is
	// labelled with; one whose engine client is gone — killed on its own,
	// or lost with the machine — is a session of its own to stop.
	for _, c := range fromContainers {
		attached := false
		for pid, p := range byPID {
			if !sameDir(p.SessionDir, c.SessionDir) {
				continue
			}
			p.Container = c.Container
			byPID[pid] = p
			attached = true
		}
		for i, p := range serverOnly {
			if !sameDir(p.SessionDir, c.SessionDir) {
				continue
			}
			serverOnly[i].Container = c.Container
			attached = true
		}
		if !attached {
			out = append(out, c)
		}
	}
	out = append(out, serverOnly...)
	for _, p := range byPID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].PID != out[j].PID {
			return out[i].PID < out[j].PID
		}
		if out[i].Container != out[j].Container {
			return out[i].Container < out[j].Container
		}
		return out[i].SessionDir < out[j].SessionDir
	})
	return out, nil
}

// Kill stops a session: its container, when it runs in one, its process and
// process group, and the built-in MCP server on the host when it left one —
// SIGTERM, then SIGKILL after grace if it is still alive. The container is
// removed first, because it outlives the engine client that started it and
// the agent is inside it; the server goes last, so the tools it serves stay
// answerable until the session using them is gone. A session found through
// its container alone has no process left to signal.
func Kill(p Proc, grace time.Duration) error {
	var errs []error
	if p.Container != "" {
		if err := removeContainer(p.Container); err != nil {
			errs = append(errs, err)
		}
	}
	if p.PID > 0 {
		errs = append(errs, killProcess(p, grace))
	}
	if p.Server > 0 {
		// Its own process group (the runner starts it with one), so the
		// group is what is signalled, as it is for a session.
		pgid, _ := syscall.Getpgid(p.Server)
		errs = append(errs, killProcess(Proc{PID: p.Server, PGID: pgid}, grace))
	}
	if p.SessionDir != "" {
		RemovePID(p.SessionDir)
		RemoveContainerID(p.SessionDir)
		RemoveServerPID(p.SessionDir)
	}
	return errors.Join(errs...)
}

// killProcess terminates a process and its process group: SIGTERM, then
// SIGKILL after grace if it is still alive.
func killProcess(p Proc, grace time.Duration) error {
	if err := signal(p, syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !Alive(p.PID) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if Alive(p.PID) {
		if err := signal(p, syscall.SIGKILL); err != nil {
			return err
		}
	}
	return nil
}

// signal sends sig to the process group when p leads one, falling back to
// the process alone when the group cannot be signalled (for example when
// the leader is already a zombie). A vanished process is not an error.
func signal(p Proc, sig syscall.Signal) error {
	if p.PGID > 1 && p.PGID == p.PID {
		err := syscall.Kill(-p.PGID, sig)
		if err == nil || errors.Is(err, syscall.ESRCH) {
			return nil
		}
	}
	err := syscall.Kill(p.PID, sig)
	if err == nil || errors.Is(err, syscall.ESRCH) || errors.Is(err, syscall.EPERM) {
		return nil
	}
	return err
}
