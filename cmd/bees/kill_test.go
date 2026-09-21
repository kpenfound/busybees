package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent/procs"
	"github.com/kpenfound/busybees/internal/statemigrate"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

// A live workspace must never look leftover: bees kill would RemoveAll the
// worktree of a running session (#133).
func TestIsLeftoverWorkspaceKeepsALiveWorkspace(t *testing.T) {
	ctx := context.Background()
	_, clone := testutil.SetupRepos(t)
	root := filepath.Join(t.TempDir(), "ws")
	m := workspace.NewManager(clone, root)
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	ws, err := m.Detached(ctx, "qa", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Remove(ctx, ws) }()

	if isLeftoverWorkspace(ws.Root) {
		t.Fatalf("live workspace %s (worktree %s) reported as leftover", ws.Root, ws.RepoDir)
	}
}

func TestIsLeftoverWorkspace(t *testing.T) {
	root := t.TempDir()
	mkdir := func(parts ...string) string {
		t.Helper()
		p := filepath.Join(append([]string{root}, parts...)...)
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	write := func(p, content string) {
		t.Helper()
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	empty := mkdir("empty")

	// A worktree whose leaf is named after the workspace (today's layout).
	named := mkdir("developer-1-123", "developer-1-123")
	write(filepath.Join(named, ".git"), "gitdir: /somewhere/.git/worktrees/developer-1-123\n")

	// A worktree named "repo" (a workspace left behind by an older bees).
	old := mkdir("developer-2-456", "repo")
	write(filepath.Join(old, ".git"), "gitdir: /somewhere/.git/worktrees/repo\n")

	// The worktree was removed but the temp dir kept some other content.
	stale := mkdir("developer-3-789", "logs")
	write(filepath.Join(stale, "run.txt"), "x")

	file := filepath.Join(root, "notadir")
	write(file, "x")

	tests := []struct {
		name string
		path string
		want bool
	}{
		{"empty dir", empty, true},
		{"live workspace", filepath.Dir(named), false},
		{"live workspace of an older layout", filepath.Dir(old), false},
		{"worktree gone, other content left", filepath.Dir(stale), true},
		{"not a directory", file, false},
		{"missing", filepath.Join(root, "nope"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLeftoverWorkspace(tt.path); got != tt.want {
				t.Fatalf("isLeftoverWorkspace(%s) = %v, want %v", tt.path, got, tt.want)
			}
		})
	}
}

// What `bees kill` says it is stopping. A container-backed session whose
// engine client is gone has no process to name, and "pid 0" would name the
// wrong thing entirely.
func TestKillTarget(t *testing.T) {
	long := "0123456789abcdef0123456789abcdef"
	for _, tc := range []struct {
		name string
		p    procs.Proc
		want string
	}{
		{"a session on the host", procs.Proc{PID: 4321}, "pid 4321"},
		{"a container session", procs.Proc{PID: 4321, Container: long}, "pid 4321 and container 0123456789ab"},
		{"a container whose client is gone", procs.Proc{Container: long}, "container 0123456789ab"},
		{"a container session with its server", procs.Proc{PID: 4321, Container: long, Server: 99}, "pid 4321, container 0123456789ab and MCP server pid 99"},
		{"a server a crash orphaned", procs.Proc{Server: 99}, "MCP server pid 99"},
	} {
		if got := killTarget(tc.p); got != tc.want {
			t.Errorf("%s: killTarget = %q, want %q", tc.name, got, tc.want)
		}
	}
}

func TestKillStopsOnStatusMigrationFailure(t *testing.T) {
	path := writeProject(t, "owner/repo", "")
	dir := filepath.Join(filepath.Dir(path), ".bees")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "schema.json"), []byte("{broken"), 0644); err != nil {
		t.Fatal(err)
	}
	// No external cleanup command should run, even for --dry-run.
	t.Setenv("PATH", t.TempDir())
	cmd := newKillCmd(&globalFlags{config: path})
	cmd.SetArgs([]string{"--dry-run"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "invalid state schema") {
		t.Fatalf("kill ignored status migration failure: %v", err)
	}
}

// `bees kill --dry-run` is an inspection: it must leave the state directory
// as it found it, and say what it would do rather than what it did. Session
// discovery deletes the stale pid, container-id and server-pid files it
// reads, so a dry run over a running session used to delete that session's
// pid file and report "no leftover sessions" (#840).
func TestKillDryRunWritesNothingAndSpeaksInTheConditional(t *testing.T) {
	path := writeProject(t, "owner/repo", "")
	sessions := filepath.Join(filepath.Dir(path), ".bees", "sessions")
	live := filepath.Join(sessions, "20260920-developer-issue-1-r1")
	gone := filepath.Join(sessions, "20260920-qa-2")
	for _, dir := range []string{live, gone} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// State migration refuses to upgrade a state directory whose session pid
	// files name live processes, and it is no part of what this test is
	// about: migrate the empty directory first, so kill finds it current.
	if err := statemigrate.Ensure(filepath.Join(filepath.Dir(path), ".bees")); err != nil {
		t.Fatal(err)
	}
	// A live process for the pid file to name. The PATH below holds no ps,
	// so there is no process table to cross-check against and the pid file
	// is trusted on its own: no agent is started, or needed.
	sleep := exec.Command("/bin/sh", "-c", "sleep 60")
	if err := sleep.Start(); err != nil {
		t.Fatal(err)
	}
	reaped := make(chan struct{})
	go func() { _ = sleep.Wait(); close(reaped) }() // reap, as init would for an orphan
	t.Cleanup(func() { _ = sleep.Process.Kill(); <-reaped })
	if err := procs.WritePID(live, sleep.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := procs.WritePID(gone, 999999); err != nil {
		t.Fatal(err)
	}
	if err := procs.WriteServerPID(gone, 999999); err != nil {
		t.Fatal(err)
	}
	// No ps, no git and no container engine: nothing but the pid files is
	// read, and no real command runs.
	t.Setenv("PATH", t.TempDir())

	out := captureStdout(t, func() {
		cmd := newKillCmd(&globalFlags{config: path})
		cmd.SetArgs([]string{"--dry-run"})
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
	})

	want := fmt.Sprintf("would kill pid %d (pidfile)", sleep.Process.Pid)
	if !strings.Contains(out, want) {
		t.Errorf("bees kill --dry-run printed:\n%s\nwant a line %q", out, want)
	}
	for _, verb := range []string{"killing ", "removed "} {
		if strings.Contains(out, verb) {
			t.Errorf("bees kill --dry-run printed %q, which reads as a real run:\n%s", verb, out)
		}
	}
	for _, f := range []string{
		filepath.Join(live, procs.PIDFile),
		filepath.Join(gone, procs.PIDFile),
		filepath.Join(gone, procs.ServerPIDFile),
	} {
		if _, err := os.Stat(f); err != nil {
			t.Errorf("bees kill --dry-run deleted %s: %v", f, err)
		}
	}
	if !procs.Alive(sleep.Process.Pid) {
		t.Error("bees kill --dry-run stopped the session it only reported on")
	}

	// The real run is what cleans up: the same fixture, now emptied of its
	// stale files.
	cmd := newKillCmd(&globalFlags{config: path})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{
		filepath.Join(live, procs.PIDFile),
		filepath.Join(gone, procs.PIDFile),
		filepath.Join(gone, procs.ServerPIDFile),
	} {
		if _, err := os.Stat(f); !os.IsNotExist(err) {
			t.Errorf("bees kill kept %s: %v", f, err)
		}
	}
}
