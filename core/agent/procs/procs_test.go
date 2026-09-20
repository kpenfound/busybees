package procs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// psLine renders a process table line the way a agent session appears: the
// session marker plus the system prompt path that attributes it to a
// factory's sessions directory.
func psLine(pid, pgid int, sessionsDir, name string) string {
	return fmt.Sprintf("  %d   %d /usr/local/bin/claude -p --append-system-prompt-file %s/20260829-%s/system-prompt.md --name agent-%s",
		pid, pgid, sessionsDir, name, name)
}

// psTable is a process table holding one factory's sessions among other
// processes: what parsePS keeps and what commandsOf reads are the two
// answers taken from it.
func psTable(scope string) string {
	return strings.Join([]string{
		psLine(100, 100, scope, "developer-issue-1-r1"),
		"  101   100 npx -y some-mcp",
		"  200   200 agent kill",
		"  300   300 vim --name agent-foo.txt",
		"  400   400 claude --add-dir /a/.agent --append-system-prompt-file /a/.agent/sessions/20260829-qa/system-prompt.md --name agent-qa-0829",
		"  500   500 /bin/zsh -c ./claude -p --append-system-prompt-file /a/.agent/sessions/20260829-x/system-prompt.md --name agent-x",
		"  600   600 claude-desktop --append-system-prompt-file /a/.agent/sessions/20260829-x/system-prompt.md --name agent-x",
		"  700   700 /usr/bin/node /opt/claude/bin/claude -p --append-system-prompt-file /a/.agent/sessions/20260829-reviewer-pr-3/system-prompt.md --name agent-reviewer-pr-3",
		// Another project's factory, and a sibling directory of this one.
		psLine(800, 800, "/b/.agent/sessions", "developer-issue-9-r1"),
		psLine(900, 900, "/a/.agent/sessions-old", "developer-issue-2-r1"),
		// This factory, but with no pid file: an orphan of a crashed run.
		psLine(1000, 1000, scope, "developer-issue-3-r1"),
		// A codex session: no --name, marked and scoped by the override that
		// hands the built-in MCP server the session directory.
		`  1100   1100 /usr/local/bin/codex exec --json -c mcp_servers.agent.env.AGENT_SESSION_DIR="/a/.agent/sessions/20260829-developer-issue-4-r1" -`,
		// The same override on another factory's codex session, and on a
		// process that is not codex at all.
		`  1200   1200 codex exec --json -c mcp_servers.agent.env.AGENT_SESSION_DIR="/b/.agent/sessions/20260829-developer-issue-4-r1" -`,
		`  1300   1300 grep mcp_servers.agent.env.AGENT_SESSION_DIR=/a/.agent/sessions/20260829-developer-issue-4-r1`,
		// An opencode session: no --name and no session-directory override in
		// its argv (it gets one through OPENCODE_CONFIG, an environment
		// variable the ps scan cannot see), so it carries no marker and is
		// never matched here. It is found through its pid file, which the
		// table says names an agent (TestFindKeepsTheSessionOfAnUnmarkedAgent).
		`  1400   1400 opencode run --format json --auto --title agent-developer-issue-5-r1`,
	}, "\n") + "\n"
}

func TestParsePS(t *testing.T) {
	scope := "/a/.agent/sessions"
	got := parsePS(psTable(scope), 300, scope)
	var pids []int
	for _, p := range got {
		pids = append(pids, p.PID)
	}
	want := []int{100, 400, 700, 1000, 1100}
	if !slices.Equal(pids, want) {
		t.Fatalf("parsePS: got pids %v want %v (%+v)", pids, want, got)
	}
}

// commandsOf keeps every process the table lists, whatever it runs and
// whichever factory it belongs to: it is what a pid file the scan did not
// match is read against, and the unmarked agent is the process only it has.
func TestCommandsOf(t *testing.T) {
	got := commandsOf(psTable("/a/.agent/sessions"))
	for pid, want := range map[int]string{
		1400: "opencode run --format json --auto --title agent-developer-issue-5-r1",
		300:  "vim --name agent-foo.txt",
		101:  "npx -y some-mcp",
	} {
		if got[pid] != want {
			t.Errorf("commandsOf[%d] = %q, want %q", pid, got[pid], want)
		}
	}
	// The pid and pgid columns are not part of a command line.
	for pid, command := range got {
		if strings.HasPrefix(command, strconv.Itoa(pid)) {
			t.Errorf("commandsOf[%d] = %q, want the command without its columns", pid, command)
		}
	}
}

// A session started through a symlinked state directory reports the resolved
// path in its argv (macOS answers /private/var for /var); it must still be
// attributed to the sessions directory as configured.
func TestParsePSResolvesSymlinkedScope(t *testing.T) {
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	linked := filepath.Join(link, "sessions")
	if err := os.MkdirAll(filepath.Join(real, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	// What the session's argv carries when agent run resolved the path
	// itself (on macOS /var/… is reported as /private/var/…).
	resolved, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}

	text := psLine(100, 100, resolved, "developer-issue-1-r1") + "\n" +
		psLine(200, 200, filepath.Join(t.TempDir(), "sessions"), "developer-issue-2-r1") + "\n"

	got := parsePS(text, 1, linked)
	if len(got) != 1 || got[0].PID != 100 {
		t.Fatalf("scope %q should match the resolved argv: %+v", linked, got)
	}
	// The scope as given must keep matching too.
	got = parsePS(psLine(300, 300, linked, "developer-issue-1-r1")+"\n", 1, linked)
	if len(got) != 1 || got[0].PID != 300 {
		t.Fatalf("scope %q should match its own form: %+v", linked, got)
	}
}

// An empty scope attributes nothing, rather than matching every path.
func TestParsePSWithoutScopeMatchesNothing(t *testing.T) {
	if got := parsePS(psLine(100, 100, "/a/.agent/sessions", "qa")+"\n", 1, ""); len(got) != 0 {
		t.Fatalf("empty scope: %+v", got)
	}
}

func TestPIDFilesAndKill(t *testing.T) {
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "20260829-developer-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A process group leader with a child, like claude + an MCP server.
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	if err := WritePID(dir, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	// A stale pid file for a dead process is cleaned up.
	stale := filepath.Join(sessions, "20260829-qa-y")
	_ = os.MkdirAll(stale, 0o755)
	_ = WritePID(stale, 999999)

	procs, err := FromPIDFiles(sessions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 1 || procs[0].PID != cmd.Process.Pid || procs[0].PGID != cmd.Process.Pid {
		t.Fatalf("procs: %+v", procs)
	}
	if _, err := os.Stat(filepath.Join(stale, PIDFile)); !os.IsNotExist(err) {
		t.Fatal("stale pid file should have been removed")
	}
	// A pid that is alive but absent from the scoped process table — a pid
	// reused by an unrelated process, or another factory's claude — is
	// treated as stale and dropped rather than killed.
	reused := filepath.Join(sessions, "20260829-pm-z")
	_ = os.MkdirAll(reused, 0o755)
	_ = WritePID(reused, os.Getpid())
	scan := &Scan{Sessions: map[int]Proc{cmd.Process.Pid: {PID: cmd.Process.Pid}}}
	fromFiles, err := FromPIDFiles(sessions, scan)
	if err != nil || len(fromFiles) != 1 || fromFiles[0].PID != cmd.Process.Pid {
		t.Fatalf("cross-check: %+v %v", fromFiles, err)
	}
	if _, err := os.Stat(filepath.Join(reused, PIDFile)); !os.IsNotExist(err) {
		t.Fatal("reused pid file should have been removed")
	}
	// Find with the real ps: our sh test process is not claude, so it only
	// survives through the pid file when ps is unavailable; here it must
	// not be reported as a session by the ps scan. The scan is scoped to
	// this sessions directory, so no other factory's session is reported
	// either.
	found, err := Find(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range found {
		if p.PID == os.Getpid() {
			t.Fatal("test binary reported as a session")
		}
	}
	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }() // reap, as init would for an orphan
	if err := Kill(procs[0], 2*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("process still alive")
	}
	if _, err := os.Stat(filepath.Join(dir, PIDFile)); !os.IsNotExist(err) {
		t.Fatal("pid file should be removed after kill")
	}
}

// agentProcess starts a live process the process table shows running the
// named agent executable: a shell reached through a symbolic link of that
// name, which is what ps reports and what actually runs. No agent is
// installed or started.
func agentProcess(t *testing.T, name string, argv ...string) *exec.Cmd {
	t.Helper()
	bin := filepath.Join(t.TempDir(), name)
	if err := os.Symlink(shPath(t), bin); err != nil {
		t.Skipf("cannot name a shell %s to stand in for an agent: %v", name, err)
	}
	cmd := exec.Command(bin, append([]string{"-c", "sleep 60 & wait"}, argv...)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	return cmd
}

// An opencode session carries no marker the ps scan can match, so its pid
// file is all that records it. Find keeps it, because the process table
// says the pid runs an agent, and leaves the file where it is: a session
// bees kill did not find would be stopped by nothing, and CheckInterrupted
// would read a running session as interrupted once the file was gone.
func TestFindKeepsTheSessionOfAnUnmarkedAgent(t *testing.T) {
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "20260920-developer-issue-5-r1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := agentProcess(t, "opencode", "run", "--format", "json", "--auto", "--title", "agent-developer-issue-5-r1")
	if err := WritePID(dir, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	// A live process that is no agent at all, recorded in another session
	// directory: the pid a reboot handed to something else.
	reused := filepath.Join(sessions, "20260920-qa-z")
	if err := os.MkdirAll(reused, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WritePID(reused, os.Getpid()); err != nil {
		t.Fatal(err)
	}

	if _, err := FromPS(context.Background(), sessions); err != nil {
		t.Skipf("no process table to scan: %v", err)
	}
	found, err := Find(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].PID != cmd.Process.Pid || found[0].SessionDir != dir {
		t.Fatalf("Find: %+v, want the opencode session alone", found)
	}
	if _, err := os.Stat(filepath.Join(dir, PIDFile)); err != nil {
		t.Errorf("the pid file of a live agent must survive Find: %v", err)
	}
	if _, err := os.Stat(filepath.Join(reused, PIDFile)); !os.IsNotExist(err) {
		t.Errorf("the pid file of a live process that is no agent must be deleted: %v", err)
	}
}

// What the process table says of a pid decides it: the command line, not
// the pid file, is what tells an agent from a process that reused its pid,
// and only the agent executable itself counts.
func TestAPIDFileIsTrustedForAnAgentCommandAlone(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command string
		want    bool
	}{
		{"opencode", "opencode run --format json --auto --title agent-qa-1", true},
		// All a pi session shows: it renames its process as it starts.
		{"pi", "pi", true},
		{"through an interpreter", "/usr/bin/node /opt/claude/bin/claude -p", true},
		{"a shell naming one", "/bin/zsh -c opencode", false},
		// The check reads the first two words only, so a command naming an
		// agent as its bare first argument passes it (isAgentCommand).
		{"an agent as a bare argument", "/usr/bin/vim opencode", true},
		{"grep", "grep -r opencode /src", false},
		{"unlisted by the table", "", false},
		{"another program", "/usr/bin/vim notes.md", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := t.TempDir()
			dir := filepath.Join(sessions, "20260920-developer-issue-5-r1")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			pid := os.Getpid()
			if err := WritePID(dir, pid); err != nil {
				t.Fatal(err)
			}
			scan := &Scan{Sessions: map[int]Proc{}, Commands: map[int]string{}}
			if tc.command != "" {
				scan.Commands[pid] = tc.command
			}
			got, err := FromPIDFiles(sessions, scan)
			if err != nil {
				t.Fatal(err)
			}
			if (len(got) == 1) != tc.want {
				t.Fatalf("FromPIDFiles for %q: %+v, want kept=%v", tc.command, got, tc.want)
			}
			_, statErr := os.Stat(filepath.Join(dir, PIDFile))
			if kept := statErr == nil; kept != tc.want {
				t.Errorf("pid file kept=%v for %q, want %v", kept, tc.command, tc.want)
			}
		})
	}
}
