package procs

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"
)

// psLine renders a process table line the way a bees session appears: the
// session marker plus the system prompt path that attributes it to a
// factory's sessions directory.
func psLine(pid, pgid int, sessionsDir, name string) string {
	return fmt.Sprintf("  %d   %d /usr/local/bin/claude -p --append-system-prompt-file %s/20260829-%s/system-prompt.md --name bees-%s",
		pid, pgid, sessionsDir, name, name)
}

func TestParsePS(t *testing.T) {
	scope := "/a/.bees/sessions"
	text := strings.Join([]string{
		psLine(100, 100, scope, "developer-issue-1-r1"),
		"  101   100 npx -y some-mcp",
		"  200   200 bees kill",
		"  300   300 vim --name bees-foo.txt",
		"  400   400 claude --add-dir /a/.bees --append-system-prompt-file /a/.bees/sessions/20260829-qa/system-prompt.md --name bees-qa-0829",
		"  500   500 /bin/zsh -c ./claude -p --append-system-prompt-file /a/.bees/sessions/20260829-x/system-prompt.md --name bees-x",
		"  600   600 claude-desktop --append-system-prompt-file /a/.bees/sessions/20260829-x/system-prompt.md --name bees-x",
		"  700   700 /usr/bin/node /opt/claude/bin/claude -p --append-system-prompt-file /a/.bees/sessions/20260829-reviewer-pr-3/system-prompt.md --name bees-reviewer-pr-3",
		// Another project's factory, and a sibling directory of this one.
		psLine(800, 800, "/b/.bees/sessions", "developer-issue-9-r1"),
		psLine(900, 900, "/a/.bees/sessions-old", "developer-issue-2-r1"),
		// This factory, but with no pid file: an orphan of a crashed run.
		psLine(1000, 1000, scope, "developer-issue-3-r1"),
	}, "\n") + "\n"

	got := parsePS(text, 300, scope)
	var pids []int
	for _, p := range got {
		pids = append(pids, p.PID)
	}
	want := []int{100, 400, 700, 1000}
	if !slices.Equal(pids, want) {
		t.Fatalf("parsePS: got pids %v want %v (%+v)", pids, want, got)
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
	// What the session's argv carries when bees run resolved the path
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
	if got := parsePS(psLine(100, 100, "/a/.bees/sessions", "qa")+"\n", 1, ""); len(got) != 0 {
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
	fromFiles, err := FromPIDFiles(sessions, map[int]Proc{cmd.Process.Pid: {PID: cmd.Process.Pid}})
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
