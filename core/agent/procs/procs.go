// Package procs finds and stops agent sessions started by a caller, for
// orphan cleanup after a crash.
//
// Sessions are found two ways: the pid file the runner writes in each
// session directory, and a scan of the process table for claude and codex
// processes carrying a session marker: the `--name agent-…` argument every
// claude session is started with, or the override that hands a codex
// session its session directory. A session in the container sandbox has a
// third: the agent runs in the container, so the container is what is
// found and stopped, from the id file and the label the runner leaves
// (see container.go); what the process table shows of it is the engine
// client that started it. When the caller supplies a host MCP server,
// that optional process is a fourth thing to stop, recorded in its own
// pid file.
//
// An opencode or a pi session is found through its pid file alone. opencode
// is given its session directory through OPENCODE_CONFIG, an environment
// variable, so its argv carries no path-bearing token to scope a ps-scan
// match to one factory's state directory the way the claude and codex
// markers do, and reading a process's environment to recover it is not
// portable across the platforms this package runs on; pi renames its
// process to "pi" as it starts, which on Linux and macOS overwrites the
// command line a ps scan would read, so it carries nothing at all. Either
// pid file is trusted because the process it names runs an agent
// executable (AgentExecutables), which the process table does say; a pid
// file naming anything else is a pid reused by an unrelated process and is
// deleted. A crashed session of either agent with no live pid file is not
// found by orphan cleanup.
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
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// PIDFile is the file a running session's pid is written to.
const PIDFile = "pid"

// SessionMarker is the argv fragment that identifies a claude session of
// the default runner: the --name every one is started with.
const SessionMarker = "--name agent-"

// CodexSessionMarker is the argv fragment that identifies a codex session
// using the default namespace. The shell environment override identifies
// the session even when the caller supplies no MCP servers.
const CodexSessionMarker = "shell_environment_policy.set.SESSION_DIR="

// CodexMarker returns the session-directory override for a caller's namespace.
func CodexMarker(prefix string) string {
	return "shell_environment_policy.set." + prefix + "SESSION_DIR="
}

// AgentExecutables are the basenames of the agent commands a session runs.
// A process running one of them is an agent session of some factory, which
// is what tells a pid file whose session the ps scan cannot match (an
// opencode session carries no marker, and an agent that rewrites its argv
// carries nothing at all) from a pid reused by an unrelated process.
//
// The runner's agent list is this one (agent.Agents), so an agent added
// there is recognized here.
var AgentExecutables = []string{"claude", "codex", "opencode", "pi"}

// Proc is a process that looks like an agent session.
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
	// Server is the pid of the caller-supplied MCP server running on the host for
	// a session in the container sandbox, which has no caller binary inside.
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

// DefaultGrace is the time allowed for exit after SIGTERM before SIGKILL.
const DefaultGrace = 5 * time.Second

// Scan is what one reading of the process table said: the sessions of this
// factory found in it, keyed by pid, and the command line of every process
// it listed. The commands answer what the sessions cannot: whether the
// process a pid file names is an agent the scan's markers do not match.
//
// A nil *Scan means no reading of the process table was available, and a
// pid file is then trusted on its own.
type Scan struct {
	Sessions map[int]Proc
	Commands map[int]string
}

// isAgent reports whether the process table showed pid running an agent
// executable. A pid the table did not list is not one.
func (s *Scan) isAgent(pid int) bool {
	if s == nil {
		return false
	}
	return isAgentCommand(strings.Fields(s.Commands[pid]))
}

// sessions returns the sessions the scan found, ordered by pid; none when
// there was no scan.
func (s *Scan) sessions() []Proc {
	if s == nil {
		return nil
	}
	out := make([]Proc, 0, len(s.Sessions))
	for _, p := range s.Sessions {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out
}

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
func FromPIDFile(dir string, scan *Scan) (Proc, bool) {
	server := liveServer(dir)
	pid, ok := livePID(dir, scan)
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
// when scan is non-nil (the ps scan), when the pid is alive but is neither
// a session the scan matched nor a process running an agent executable: a
// pid reused by an unrelated process after a reboot, which must never be
// killed.
func livePID(dir string, scan *Scan) (int, bool) {
	b, err := os.ReadFile(filepath.Join(dir, PIDFile))
	if err != nil {
		return 0, false
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if !Alive(pid) {
		RemovePID(dir)
		return 0, false
	}
	if scan != nil {
		_, matched := scan.Sessions[pid]
		if !matched && !scan.isAgent(pid) {
			RemovePID(dir) // alive, but not an agent session: pid reused
			return 0, false
		}
	}
	return pid, true
}

// FromPIDFiles returns live sessions recorded under sessionsDir and deletes
// pid files of processes that no longer exist, by asking FromPIDFile about
// every session directory in turn.
func FromPIDFiles(sessionsDir string, scan *Scan) ([]Proc, error) {
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
		if p, ok := FromPIDFile(filepath.Join(sessionsDir, e.Name()), scan); ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// FromPS scans the process table for agent sessions: processes whose
// executable is one of AgentExecutables, whose arguments carry a session
// marker and whose command line references sessionsDir, the sessions
// directory of this factory's state directory.
func FromPS(ctx context.Context, sessionsDir string, markers ...Markers) ([]Proc, error) {
	scan, err := scanPS(ctx, sessionsDir, markers...)
	if err != nil {
		return nil, err
	}
	return scan.sessions(), nil
}

// scanPS reads the process table once and keeps both of the answers it
// holds: the sessions of this factory, and the command line of every
// process, which is how a pid file naming an unmarked agent is told from
// one naming a reused pid.
func scanPS(ctx context.Context, sessionsDir string, markers ...Markers) (*Scan, error) {
	cmd := exec.CommandContext(ctx, "ps", "-axo", "pid=,pgid=,command=")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	scan := &Scan{Sessions: map[int]Proc{}, Commands: commandsOf(stdout.String())}
	for _, p := range parsePS(stdout.String(), os.Getpid(), sessionsDir, markers...) {
		scan.Sessions[p.PID] = p
	}
	return scan, nil
}

// commandsOf reads the command line of every process the table lists,
// whatever it runs and whichever factory it belongs to.
func commandsOf(text string) map[int]string {
	out := map[int]string{}
	for _, line := range strings.Split(text, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err := strconv.Atoi(fields[0])
		if err != nil {
			continue
		}
		out[pid] = strings.Join(fields[2:], " ")
	}
	return out
}

// parsePS keeps the agent processes of the factory whose sessions live in
// scope. scope is matched as a path prefix (with a trailing separator), so
// a sibling directory such as `<state>/sessions-old` is not a hit. Because
// macOS reports /private/var for /var and a state directory may be reached
// through a symlink, both the path as given and its resolved form are
// accepted.
func parsePS(text string, self int, scope string, markers ...Markers) []Proc {
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
		if !isSessionProcess(fields[2:], command, markers...) || !hasMarker(command, markers...) {
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
func hasMarker(command string, markers ...Markers) bool {
	m := markerSet(markers)
	for _, marker := range []string{m.Session, m.Codex, m.LegacyCodex} {
		if marker != "" && strings.Contains(command, " "+marker) {
			return true
		}
	}
	return false
}

// isSessionProcess reports whether argv starts a process that runs a
// session: an agent executable, directly or through an interpreter
// (node/bun/sh script), or the container engine running a container-backed
// session, whose agent is in the container and never in the process table.
// The engine counts only when the command line carries the label the caller
// puts on a session's container, so another container of the machine is
// never taken for one.
func isSessionProcess(argv []string, command string, markers ...Markers) bool {
	engine := filepath.Base(Engine)
	for i, a := range argv[:min(2, len(argv))] {
		switch base := filepath.Base(a); {
		case slices.Contains(AgentExecutables, base):
			return true
		case base == engine:
			return strings.Contains(command, " --label "+markerSet(markers).Container+"=")
		}
		if i == 0 && strings.HasPrefix(a, "-") {
			return false
		}
	}
	return false
}

// isAgentCommand reports whether argv runs an agent executable, directly or
// through an interpreter: the same discrimination isSessionProcess makes,
// without the markers and the container engine, which is all a pid file
// needs. A shell or an editor whose arguments merely name an agent is not
// one.
func isAgentCommand(argv []string) bool {
	for i, a := range argv[:min(2, len(argv))] {
		if slices.Contains(AgentExecutables, filepath.Base(a)) {
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
func Find(ctx context.Context, sessionsDir string, markers ...Markers) ([]Proc, error) {
	byPID := map[int]Proc{}
	scan, err := scanPS(ctx, sessionsDir, markers...)
	if err != nil {
		scan = nil // no process table to cross-check against
	}
	// The engine's containers are asked for before the pid files, because
	// asking clears the id file of a session whose container has gone. A
	// machine with no container engine has no container session either, so
	// the error is the empty answer.
	fromContainers, _ := FromContainers(ctx, sessionsDir, markers...)
	fromFiles, err := FromPIDFiles(sessionsDir, scan)
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
	for _, p := range scan.sessions() {
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
// process group, and the caller-supplied MCP server on the host when it left one —
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

// Markers describes the caller's session command markers and container label.
type Markers struct {
	Session, Codex, Container string
	// LegacyCodex optionally recognizes sessions launched before the standalone runner.
	LegacyCodex string
}

func markerSet(markers []Markers) Markers {
	if len(markers) > 0 {
		return markers[0]
	}
	return Markers{Session: SessionMarker, Codex: CodexSessionMarker, Container: ContainerLabel, LegacyCodex: "mcp_servers.agent.env.AGENT_SESSION_DIR="}
}
