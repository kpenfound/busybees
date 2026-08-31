// Package procs finds and stops Claude Code sessions started by bees, for
// `bees kill` after a crash.
//
// Sessions are found two ways: the pid file the runner writes in each
// session directory, and a scan of the process table for claude processes
// carrying the `--name bees-…` argument every session is started with.
//
// Both sources are scoped to one factory: a process only counts when its
// command line also references this state directory's sessions directory
// (every session's argv carries `--append-system-prompt-file
// <sessions dir>/<session>/system-prompt.md`). Another project's sessions
// are therefore never reported, however many factories share a machine.
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

// SessionMarker is the argv fragment that identifies a bees session.
const SessionMarker = "--name bees-"

// Proc is a process that looks like a bees session.
type Proc struct {
	PID     int
	PGID    int
	Command string
	// Source is "pidfile" or "ps".
	Source string
	// SessionDir is set for processes found through a pid file.
	SessionDir string
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

// FromPIDFile returns the live session recorded in one session directory.
// It reports false when the directory holds no pid file, when the process
// the file names is gone — in which case the stale file is deleted — and,
// when known is non-nil (the ps scan), when the pid is alive but is not a
// claude session: a pid reused by an unrelated process after a reboot, which
// must never be killed.
func FromPIDFile(dir string, known map[int]Proc) (Proc, bool) {
	b, err := os.ReadFile(filepath.Join(dir, PIDFile))
	if err != nil {
		return Proc{}, false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if !Alive(pid) {
		RemovePID(dir)
		return Proc{}, false
	}
	if known != nil {
		if _, ok := known[pid]; !ok {
			RemovePID(dir) // alive, but not a claude session: pid reused
			return Proc{}, false
		}
	}
	pgid, _ := syscall.Getpgid(pid)
	return Proc{PID: pid, PGID: pgid, Source: "pidfile", SessionDir: dir}, true
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
// executable is claude, whose arguments carry the session marker and whose
// command line references sessionsDir, the sessions directory of this
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

// parsePS keeps the claude processes of the factory whose sessions live in
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
		// Only the claude executable itself (or an interpreter running a
		// claude script), never a shell or editor whose command line merely
		// mentions the marker.
		if !isClaude(fields[2:]) || !strings.Contains(command, " "+SessionMarker) {
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

// isClaude reports whether argv starts the claude executable, directly or
// through an interpreter (node/bun/sh script).
func isClaude(argv []string) bool {
	for i, a := range argv[:min(2, len(argv))] {
		if filepath.Base(a) == "claude" {
			return true
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
	fromFiles, err := FromPIDFiles(sessionsDir, known)
	if err != nil {
		return nil, err
	}
	for _, p := range fromFiles {
		byPID[p.PID] = p
	}
	for _, p := range fromPS {
		if existing, ok := byPID[p.PID]; ok {
			existing.Command = p.Command
			byPID[p.PID] = existing
			continue
		}
		byPID[p.PID] = p
	}
	out := make([]Proc, 0, len(byPID))
	for _, p := range byPID {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// Kill terminates a process and its process group: SIGTERM, then SIGKILL
// after grace if it is still alive.
func Kill(p Proc, grace time.Duration) error {
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
	if p.SessionDir != "" {
		RemovePID(p.SessionDir)
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
