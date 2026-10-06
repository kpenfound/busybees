package procs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeSbx writes a shell script standing in for the sbx CLI: `rm --force
// <name>` records its arguments and, unless told to fail, succeeds; any
// other invocation fails the test outright, so a stray `sbx policy` call
// would be caught. A real sbx is never run.
func fakeSbx(t *testing.T, fail bool) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "sbx")
	script := `#!/bin/sh
echo "$@" >> "$(dirname "$0")/calls.txt"
case "$1" in
rm) [ -f "$(dirname "$0")/fail" ] && { echo "boom" >&2; exit 1; } || exit 0 ;;
*) echo "unexpected: $@" >&2; exit 2 ;;
esac
`
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if fail {
		if err := os.WriteFile(filepath.Join(dir, "fail"), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return bin
}

// sbxCalls is what the fake sbx was invoked with, one line per call.
func sbxCalls(t *testing.T, sbxBin string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(filepath.Dir(sbxBin), "calls.txt"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// allowSbxFailure flips the fake sbx this test binary names from failing to
// succeeding, the way a retried CleanSandboxes call finds a now-healthy sbx.
func allowSbxFailure(t *testing.T, sbxBin string) {
	t.Helper()
	if err := os.Remove(filepath.Join(filepath.Dir(sbxBin), "fail")); err != nil {
		t.Fatal(err)
	}
}

func writeSandboxName(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := WriteSandboxName(dir, name); err != nil {
		t.Fatal(err)
	}
}

// writeSandboxWorkspace records a session's primary-workspace path the way
// core/agent does: a plain WriteFile, since the package exports no writer.
func writeSandboxWorkspace(t *testing.T, dir, path string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, SandboxWorkspaceFile), []byte(path+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// CleanSandboxes and CleanSandboxDirs both run sbx with exactly `rm --force
// <name>` for every session directory holding a sandbox-name record, and
// make no other sbx call: no `sbx policy` call, and no network-rule record
// is read or written anywhere in the session directory.
func TestCleanSandboxesRemovesEverySandbox(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(sbxBin, sessions, one, two string) error
	}{
		{"a sessions directory", func(sbxBin, sessions, one, two string) error {
			return CleanSandboxes(context.Background(), sbxBin, sessions)
		}},
		{"a list of session directories", func(sbxBin, sessions, one, two string) error {
			noSandbox := filepath.Join(sessions, "20260906-reviewer-3")
			return CleanSandboxDirs(context.Background(), sbxBin, []string{one, two, noSandbox})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sessions := t.TempDir()
			one := filepath.Join(sessions, "20260906-developer-issue-1-r1")
			two := filepath.Join(sessions, "20260906-qa-2")
			writeSandboxName(t, one, "agent-developer-issue-1-r1-ab12")
			writeSandboxName(t, two, "agent-qa-2-cd34")
			// A session with no sandbox record, an unrelated file and a
			// non-directory entry, none of which are sbx's business.
			noSandbox := filepath.Join(sessions, "20260906-reviewer-3")
			if err := os.MkdirAll(noSandbox, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(sessions, "notes.txt"), []byte("hi\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			sbxBin := fakeSbx(t, false)
			if err := tc.run(sbxBin, sessions, one, two); err != nil {
				t.Fatalf("clean: %v", err)
			}
			calls := sbxCalls(t, sbxBin)
			want := []string{"rm --force agent-developer-issue-1-r1-ab12", "rm --force agent-qa-2-cd34"}
			if !sameSet(calls, want) {
				t.Fatalf("sbx calls: %v, want exactly %v", calls, want)
			}
			for _, call := range calls {
				if strings.Contains(call, "policy") {
					t.Errorf("a policy call was made: %q", call)
				}
			}
			for _, dir := range []string{one, two} {
				if SandboxName(dir) != "" {
					t.Errorf("%s: sandbox-name should be deleted after a successful removal", dir)
				}
			}
			if _, err := os.Stat(noSandbox); err != nil {
				t.Errorf("a session directory without a sandbox record was touched: %v", err)
			}
			if b, err := os.ReadFile(filepath.Join(sessions, "notes.txt")); err != nil || string(b) != "hi\n" {
				t.Errorf("an unrelated file in the sessions directory was touched: %v %q", err, b)
			}
			for _, name := range []string{"network-policy", "policy", "sandbox-network", "network-rules"} {
				if _, err := os.Stat(filepath.Join(one, name)); !os.IsNotExist(err) {
					t.Errorf("a network-rule record %q was written", name)
				}
			}
		})
	}
}

func sameSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := map[string]int{}
	for _, g := range got {
		seen[g]++
	}
	for _, w := range want {
		seen[w]--
	}
	for _, n := range seen {
		if n != 0 {
			return false
		}
	}
	return true
}

// On success the sandbox-name record is deleted. When sbx fails, the record
// is kept so the sandbox is not forgotten, and a second call with a now-
// succeeding sbx removes it and deletes the record.
func TestCleanSandboxesRetriesAfterAFailure(t *testing.T) {
	dir := t.TempDir()
	writeSandboxName(t, dir, "agent-developer-issue-1-r1-ab12")
	sbxBin := fakeSbx(t, true)

	if err := CleanSandboxDirs(context.Background(), sbxBin, []string{dir}); err == nil {
		t.Fatal("CleanSandboxDirs: want an error when sbx fails")
	}
	if SandboxName(dir) != "agent-developer-issue-1-r1-ab12" {
		t.Fatalf("sandbox-name should be kept after a failed removal, got %q", SandboxName(dir))
	}

	allowSbxFailure(t, sbxBin)
	if err := CleanSandboxDirs(context.Background(), sbxBin, []string{dir}); err != nil {
		t.Fatalf("CleanSandboxDirs on retry: %v", err)
	}
	if SandboxName(dir) != "" {
		t.Error("sandbox-name should be deleted once the retry succeeds")
	}
	calls := sbxCalls(t, sbxBin)
	if len(calls) != 2 || calls[0] != "rm --force agent-developer-issue-1-r1-ab12" || calls[1] != calls[0] {
		t.Fatalf("sbx calls across both attempts: %v", calls)
	}
}

// A sandbox-name that is empty or starts with '-' would be read by sbx as a
// flag, so it is rejected as an error, the record is kept, and sbx is never
// invoked with it.
func TestCleanSandboxesRejectsAnUnsafeName(t *testing.T) {
	for _, name := range []string{"", "-rf"} {
		t.Run("name "+name, func(t *testing.T) {
			dir := t.TempDir()
			writeSandboxName(t, dir, name)
			sbxBin := fakeSbx(t, false)

			err := CleanSandboxDirs(context.Background(), sbxBin, []string{dir})
			if err == nil {
				t.Fatal("want an error for an unsafe sandbox name")
			}
			if !strings.Contains(err.Error(), dir) {
				t.Errorf("error does not name the session directory: %v", err)
			}
			if calls := sbxCalls(t, sbxBin); calls != nil {
				t.Errorf("sbx was invoked with an unsafe name: %v", calls)
			}
			if SandboxName(dir) != name {
				t.Errorf("sandbox-name record should be kept, got %q", SandboxName(dir))
			}
		})
	}
}

// One session directory's failure does not stop the others, and the
// returned error names the failing one. Session directories without a
// sandbox record are left byte-for-byte unchanged.
func TestCleanSandboxesJoinsPerDirectoryErrors(t *testing.T) {
	sessions := t.TempDir()
	bad := filepath.Join(sessions, "20260906-bad")
	good := filepath.Join(sessions, "20260906-good")
	plain := filepath.Join(sessions, "20260906-plain")
	writeSandboxName(t, bad, "-rf")
	writeSandboxName(t, good, "agent-good-ab12")
	if err := os.MkdirAll(plain, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(plain, "transcript.jsonl"), []byte("line\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(filepath.Join(plain, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	beforeInfo, err := os.Stat(filepath.Join(plain, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	sbxBin := fakeSbx(t, false)
	err = CleanSandboxes(context.Background(), sbxBin, sessions)
	if err == nil {
		t.Fatal("want an error naming the failing session directory")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %v does not name the failing directory %s", err, bad)
	}
	if SandboxName(good) != "" {
		t.Error("the good session's sandbox should still be removed despite the bad one's failure")
	}
	if SandboxName(bad) != "-rf" {
		t.Error("the bad session's record should be kept")
	}
	after, err := os.ReadFile(filepath.Join(plain, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	afterInfo, err := os.Stat(filepath.Join(plain, "transcript.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) || afterInfo.ModTime() != beforeInfo.ModTime() {
		t.Error("a session directory without a sandbox record was not left byte-for-byte unchanged")
	}
}

// A sessions directory that does not exist yet (no session has ever run) is
// not an error: there is nothing to clean up.
func TestCleanSandboxesOfAMissingSessionsDirectory(t *testing.T) {
	sbxBin := fakeSbx(t, false)
	if err := CleanSandboxes(context.Background(), sbxBin, filepath.Join(t.TempDir(), "no-such-dir")); err != nil {
		t.Fatalf("CleanSandboxes of a missing sessions directory: %v", err)
	}
}

// mkPrimaryWorkspace makes a fixture under os.TempDir(), the way
// os.MkdirTemp("", namePrefix()+"sbx-primary-") does for a real session, and
// registers its removal so the test leaves nothing behind.
func mkPrimaryWorkspace(t *testing.T, prefix string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", prefix)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return dir
}

// Cleanup removes the directory a valid workspace record names and deletes
// the record, whether or not the session directory also holds
// sandbox-name, across the base names a default, a custom and an empty
// NamePrefix produce.
func TestCleanSandboxesRemovesTheWorkspace(t *testing.T) {
	for _, prefix := range []string{"agent-sbx-primary-", "myapp-sbx-primary-", "sbx-primary-"} {
		for _, withSandboxName := range []bool{true, false} {
			name := prefix
			if withSandboxName {
				name += " alongside sandbox-name"
			} else {
				name += " without sandbox-name"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				ws := mkPrimaryWorkspace(t, prefix)
				if err := os.WriteFile(filepath.Join(ws, "marker"), []byte("x"), 0o644); err != nil {
					t.Fatal(err)
				}
				writeSandboxWorkspace(t, dir, ws)
				sbxBin := fakeSbx(t, false)
				if withSandboxName {
					writeSandboxName(t, dir, "agent-session-ab12")
				}

				if err := CleanSandboxDirs(context.Background(), sbxBin, []string{dir}); err != nil {
					t.Fatalf("CleanSandboxDirs: %v", err)
				}
				if _, err := os.Stat(ws); !os.IsNotExist(err) {
					t.Errorf("workspace %s should have been removed, stat err = %v", ws, err)
				}
				if SandboxWorkspace(dir) != "" {
					t.Errorf("workspace record should be deleted, got %q", SandboxWorkspace(dir))
				}
				if withSandboxName && SandboxName(dir) != "" {
					t.Errorf("sandbox-name should also be deleted, got %q", SandboxName(dir))
				}
			})
		}
	}
}

// When removing the workspace directory fails, the record is kept so a
// later call can retry.
func TestCleanSandboxesKeepsTheWorkspaceRecordOnFailure(t *testing.T) {
	dir := t.TempDir()
	ws := mkPrimaryWorkspace(t, "agent-sbx-primary-")
	writeSandboxWorkspace(t, dir, ws)

	old := removeWorkspace
	removeWorkspace = func(string) error { return fmt.Errorf("boom") }
	t.Cleanup(func() { removeWorkspace = old })

	err := CleanSandboxDirs(context.Background(), fakeSbx(t, false), []string{dir})
	if err == nil {
		t.Fatal("want an error when removing the workspace fails")
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("error %v does not name the session directory", err)
	}
	if SandboxWorkspace(dir) != ws {
		t.Errorf("workspace record should be kept after a failed removal, got %q", SandboxWorkspace(dir))
	}
	if _, statErr := os.Stat(ws); statErr != nil {
		t.Errorf("workspace directory should still exist: %v", statErr)
	}
}

// A workspace record that fails the workspace path rule is reported as an
// error naming the session directory and the path, the path (where it
// exists) survives, and the record is kept.
func TestCleanSandboxesRejectsAnInvalidWorkspacePath(t *testing.T) {
	unique := filepath.Base(t.TempDir())
	tmp := filepath.Clean(os.TempDir())

	for _, tc := range []struct {
		name   string
		path   func() string // returns the recorded path, creating a real directory there when exists is true
		exists bool
	}{
		{"relative path", func() string {
			return filepath.Join("relative", "sbx-primary-"+unique)
		}, false},
		{"subdirectory of os.TempDir()", func() string {
			sub := filepath.Join(tmp, "orphan-cleanup-sub-"+unique)
			p := filepath.Join(sub, "sbx-primary-1")
			if err := os.MkdirAll(p, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(sub) })
			return p
		}, true},
		{"a `..` path that cleans to outside os.TempDir()", func() string {
			return filepath.Join(tmp, "..", "sbx-primary-"+unique)
		}, false},
		{"directly under os.TempDir() without sbx-primary-", func() string {
			p := filepath.Join(tmp, "agent-workspace-"+unique)
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(p) })
			return p
		}, true},
		{"directly under os.TempDir() ending in sbx-primary- with nothing after", func() string {
			p := filepath.Join(tmp, "agent-"+unique+"-sbx-primary-")
			if err := os.Mkdir(p, 0o755); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.RemoveAll(p) })
			return p
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := tc.path()
			dir := t.TempDir()
			writeSandboxWorkspace(t, dir, path)

			err := CleanSandboxDirs(context.Background(), fakeSbx(t, false), []string{dir})
			if err == nil {
				t.Fatal("want an error for an invalid workspace path")
			}
			if !strings.Contains(err.Error(), dir) || !strings.Contains(err.Error(), path) {
				t.Errorf("error %v does not name the session directory %s and path %s", err, dir, path)
			}
			if SandboxWorkspace(dir) != path {
				t.Errorf("workspace record should be kept, got %q", SandboxWorkspace(dir))
			}
			if tc.exists {
				if _, statErr := os.Stat(path); statErr != nil {
					t.Errorf("path %s should still exist: %v", path, statErr)
				}
			}
		})
	}
}

// A workspace failure in one session directory does not stop the others
// from being processed, and the joined error names the failing directory.
func TestCleanSandboxesJoinsWorkspaceErrorsAcrossDirectories(t *testing.T) {
	sessions := t.TempDir()
	bad := filepath.Join(sessions, "20260906-bad")
	good := filepath.Join(sessions, "20260906-good")
	writeSandboxWorkspace(t, bad, filepath.Join("relative", "sbx-primary-1"))
	goodWs := mkPrimaryWorkspace(t, "agent-sbx-primary-")
	writeSandboxWorkspace(t, good, goodWs)

	err := CleanSandboxes(context.Background(), fakeSbx(t, false), sessions)
	if err == nil {
		t.Fatal("want an error naming the failing session directory")
	}
	if !strings.Contains(err.Error(), bad) {
		t.Errorf("error %v does not name the failing directory %s", err, bad)
	}
	if SandboxWorkspace(good) != "" {
		t.Error("the good session's workspace should still be removed despite the bad one's failure")
	}
	if _, statErr := os.Stat(goodWs); !os.IsNotExist(statErr) {
		t.Errorf("the good session's workspace should have been removed: %v", statErr)
	}
	if SandboxWorkspace(bad) == "" {
		t.Error("the bad session's record should be kept")
	}
}
