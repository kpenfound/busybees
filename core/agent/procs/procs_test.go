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

// codex app-server carries the same session-directory override exec did,
// on a command line whose subcommand is "app-server" instead of "exec":
// hasMarker matches the marker itself, not the subcommand next to it, so
// this is found exactly as the exec-era psLine 1100 in psTable is.
func TestParsePSFindsACodexAppServerSession(t *testing.T) {
	scope := "/a/.agent/sessions"
	dir := scope + "/20260920-developer-issue-6-r1"
	args := []string{"app-server", "-c", CodexMarker("") + `"` + dir + `"`}
	line := fmt.Sprintf("  1500   1500 /usr/local/bin/codex %s\n", strings.Join(args, " "))
	got := parsePS(line, 1, scope)
	if len(got) != 1 || got[0].PID != 1500 {
		t.Fatalf("parsePS did not find the codex app-server session: %+v", got)
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

// FromPIDFiles reports the live session a pid file names and deletes a
// stale pid file for a process that has already died, which is how a
// crashed session's directory stops being mistaken for a running one.
func TestFromPIDFilesFindsALiveSessionAndRemovesAStalePIDFile(t *testing.T) {
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "20260829-developer-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	if err := WritePID(dir, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(sessions, "20260829-qa-y")
	_ = os.MkdirAll(stale, 0o755)
	_ = WritePID(stale, 999999)

	procs, err := FromPIDFiles(sessions, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 1 || procs[0].PID != cmd.Process.Pid {
		t.Fatalf("FromPIDFiles: %+v, want the live session alone", procs)
	}
	if _, err := os.Stat(filepath.Join(stale, PIDFile)); !os.IsNotExist(err) {
		t.Error("stale pid file should have been removed")
	}
}

// A pid that is alive but absent from the scoped process table — a pid
// reused by an unrelated process, or another factory's claude — is treated
// as stale and dropped rather than kept as a session to kill.
func TestFromPIDFilesDropsAPIDTheScanDoesNotMatch(t *testing.T) {
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "20260829-developer-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	if err := WritePID(dir, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	reused := filepath.Join(sessions, "20260829-pm-z")
	_ = os.MkdirAll(reused, 0o755)
	_ = WritePID(reused, os.Getpid())
	scan := &Scan{Sessions: map[int]Proc{cmd.Process.Pid: {PID: cmd.Process.Pid}}}

	fromFiles, err := FromPIDFiles(sessions, scan)
	if err != nil || len(fromFiles) != 1 || fromFiles[0].PID != cmd.Process.Pid {
		t.Fatalf("FromPIDFiles cross-check: %+v %v, want the matched session alone", fromFiles, err)
	}
	if _, err := os.Stat(filepath.Join(reused, PIDFile)); !os.IsNotExist(err) {
		t.Error("reused pid file should have been removed")
	}
}

// Find's real ps scan never reports the test binary running it, or a
// session of a different factory, as one of this factory's sessions: the
// scope check (not a fake process table) is what is under test here, so it
// runs the real `ps` and skips when none is available rather than faking
// the answer it is meant to verify.
func TestFindDoesNotReportTheCallingProcessAsASession(t *testing.T) {
	sessions := t.TempDir()
	dir := filepath.Join(sessions, "20260829-developer-x")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	if err := WritePID(dir, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}

	if _, err := FromPS(context.Background(), sessions); err != nil {
		t.Skipf("no process table to scan: %v", err)
	}
	found, err := Find(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range found {
		if p.PID == os.Getpid() {
			t.Fatalf("Find reported the test binary (pid %d) as a session: %+v", os.Getpid(), found)
		}
	}
}

// Kill stops a session's process group and removes its pid file, reaping
// the process the way init would for an orphan so the caller's own wait
// does not hang.
func TestKillStopsTheProcessGroupAndRemovesThePIDFile(t *testing.T) {
	dir := t.TempDir()
	// A process group leader with a child, like claude + an MCP server:
	// killing the group, not the leader alone, is what is under test.
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	if err := WritePID(dir, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	p, ok := FromPIDFile(dir, nil)
	if !ok || p.PID != cmd.Process.Pid {
		t.Fatalf("FromPIDFile: %+v %v, want the started process", p, ok)
	}

	waited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(waited) }() // reap, as init would for an orphan
	if err := Kill(p, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	select {
	case <-waited:
	case <-time.After(5 * time.Second):
		t.Fatal("process still alive after Kill")
	}
	if _, err := os.Stat(filepath.Join(dir, PIDFile)); !os.IsNotExist(err) {
		t.Error("pid file should be removed after kill")
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

// Discovery deletes the stale files it reads, which is the cleanup a caller
// stopping sessions wants and a liability for one that only looks: a dry run
// that deleted a running session's pid file would leave the session
// unstoppable and read as interrupted. A read-only Finder reports the
// same sessions and writes nothing.
func TestReadOnlyDiscoveryDeletesNothing(t *testing.T) {
	sessions := t.TempDir()
	// A live session found through its pid file, so the answer under test is
	// not the empty one.
	live := filepath.Join(sessions, "20260920-developer-issue-1-r1")
	if err := os.MkdirAll(live, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := agentProcess(t, "opencode", "run", "--format", "json")
	if err := WritePID(live, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	// A pid file naming a process that has gone, and one naming a live
	// process that is no agent: the two a writing run deletes.
	gone := filepath.Join(sessions, "20260920-qa-2")
	if err := os.MkdirAll(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WritePID(gone, 999999); err != nil {
		t.Fatal(err)
	}
	reused := filepath.Join(sessions, "20260920-reviewer-pr-3")
	if err := os.MkdirAll(reused, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WritePID(reused, os.Getpid()); err != nil {
		t.Fatal(err)
	}
	// A server the crash left recorded but not running, and a container the
	// engine no longer lists.
	if err := WriteServerPID(gone, 999999); err != nil {
		t.Fatal(err)
	}
	container := filepath.Join(sessions, "20260920-product_manager-4")
	writeContainerID(t, container, "aaa111")
	fakeEngine(t, "") // an engine with no session container running

	if _, err := FromPS(context.Background(), sessions); err != nil {
		t.Skipf("no process table to scan: %v", err)
	}
	files := []string{
		filepath.Join(gone, PIDFile),
		filepath.Join(reused, PIDFile),
		filepath.Join(gone, ServerPIDFile),
		filepath.Join(container, ContainerIDFile),
	}

	readOnly, err := Finder{ReadOnly: true}.Find(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if len(readOnly) != 1 || readOnly[0].PID != cmd.Process.Pid || readOnly[0].SessionDir != live {
		t.Fatalf("read-only Find: %+v, want the live session alone", readOnly)
	}
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("read-only Find deleted %s: %v", f, err)
		}
	}

	writing, err := Find(context.Background(), sessions)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(writing, readOnly) {
		t.Errorf("Find: %+v, want what read-only discovery reported (%+v)", writing, readOnly)
	}
	for _, f := range files {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("Find kept the stale %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(live, PIDFile)); err != nil {
		t.Errorf("the pid file of a live session must survive either run: %v", err)
	}
}
