package procs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestParsePS(t *testing.T) {
	text := `  100   100 /usr/local/bin/claude -p --name bees-developer-issue-1-r1
  101   100 npx -y some-mcp
  200   200 bees kill
  300   300 vim --name bees-foo.txt
  400   400 claude --name bees-qa-0829
  500   500 /bin/zsh -c ./claude -p --name bees-x
  600   600 claude-desktop --name bees-x
  700   700 /usr/bin/node /opt/claude/bin/claude -p --name bees-reviewer-pr-3
`
	got := parsePS(text, 300)
	if len(got) != 3 || got[0].PID != 100 || got[1].PID != 400 || got[2].PID != 700 {
		t.Fatalf("parsePS: %+v", got)
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
	// With a process table that does not list the pid, the pid file is
	// treated as reused and dropped rather than killed.
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
	// not be reported as a session by the ps scan.
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
