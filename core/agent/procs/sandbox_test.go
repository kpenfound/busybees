package procs

import (
	"context"
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
