// Package vcstest builds the local git repository the replay tests of every
// backend share: a dependent branch whose parent was integrated upstream as
// a squash.
package vcstest

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ChildAuthor is the author of the child branch's commits, which a replay
// keeps.
const ChildAuthor = "Child Author <child@example.com>"

// Stack is the repository: main, a parent branch of two commits started
// from it, a child branch of two commits started from the parent, and main
// again with the parent squashed onto it and one more commit.
type Stack struct {
	Dir string
	// OldBase is the parent's tip, where the child was started from; Head
	// the child's tip; Onto main's tip, the new upstream.
	OldBase, Head, Onto string
	// Parent and Child are the commits of each branch, oldest first.
	Parent, Child []string
	// ChildMessages are the child's commit messages, oldest first.
	ChildMessages []string
}

// NewStack builds the repository in a temporary directory, with the child
// branch checked out. With conflict, main's last commit changes a.txt and
// b.txt, which the child's first commit changes too, and c.txt, which its
// second commit changes.
func NewStack(t *testing.T, conflict bool) *Stack {
	t.Helper()
	dir := t.TempDir()
	s := &Stack{Dir: dir}
	date := 1700000000
	git := func(args ...string) string {
		t.Helper()
		stamp := "@" + strconv.Itoa(date) + " +0000"
		return s.git(t, []string{"GIT_COMMITTER_NAME=Committer", "GIT_COMMITTER_EMAIL=committer@example.com", "GIT_AUTHOR_DATE=" + stamp, "GIT_COMMITTER_DATE=" + stamp}, args...)
	}
	write := func(files map[string]string) {
		t.Helper()
		for name, content := range files {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	commit := func(author, msg string, files map[string]string) string {
		t.Helper()
		write(files)
		git("add", "--all")
		date++
		git("commit", "--quiet", "--author", author, "-m", msg)
		return git("rev-parse", "HEAD")
	}
	upstream := "Upstream <upstream@example.com>"
	git("init", "--quiet", "--initial-branch=main")
	commit(upstream, "init", map[string]string{"README.md": "# test\n", "a.txt": "a\n", "b.txt": "b\n", "c.txt": "c\n"})

	git("checkout", "--quiet", "-b", "parent")
	s.Parent = append(s.Parent,
		commit("Parent Author <parent@example.com>", "feature: first half", map[string]string{"feature.txt": "one\n"}),
		commit("Parent Author <parent@example.com>", "feature: second half", map[string]string{"feature.txt": "one\ntwo\n"}))
	s.OldBase = s.Parent[1]

	git("checkout", "--quiet", "-b", "child")
	s.ChildMessages = []string{"child: use the feature\n\nChange-Id: I1111", "child: finish"}
	s.Child = append(s.Child,
		commit(ChildAuthor, s.ChildMessages[0], map[string]string{"child.txt": "uses one\n", "a.txt": "a child\n", "b.txt": "b child\n"}),
		commit(ChildAuthor, s.ChildMessages[1], map[string]string{"child.txt": "uses one and two\n", "c.txt": "c child\n"}))
	s.Head = s.Child[1]

	git("checkout", "--quiet", "main")
	commit(upstream, "feature (#1)", map[string]string{"feature.txt": "one\ntwo\n"})
	later := map[string]string{"later.txt": "later\n"}
	if conflict {
		later["a.txt"], later["b.txt"], later["c.txt"] = "a upstream\n", "b upstream\n", "c upstream\n"
	}
	s.Onto = commit(upstream, "later", later)
	git("checkout", "--quiet", "child")
	return s
}

// Git runs git in the stack's directory, with no configuration of the
// machine's, and returns its output.
func (s *Stack) Git(t *testing.T, args ...string) string {
	t.Helper()
	return s.git(t, nil, args...)
}

func (s *Stack) git(t *testing.T, env []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = s.Dir
	cmd.Env = append(append(os.Environ(), "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_CONFIG_NOSYSTEM=1"), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// Read reads a file of the working tree.
func (s *Stack) Read(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(s.Dir, name))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// Write writes a file of the working tree.
func (s *Stack) Write(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(s.Dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
