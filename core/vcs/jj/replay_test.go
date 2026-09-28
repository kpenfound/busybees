package jj

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/core/vcs/internal/vcstest"
)

// newStack is vcstest's repository with jj colocated in it: the branches
// are bookmarks, and the working copy is a new change on top of the child.
func newStack(t *testing.T, conflict bool) *vcstest.Stack {
	t.Helper()
	if _, err := exec.LookPath("jj"); err != nil {
		// The test runtime dagger check runs in has jj and says so: there
		// a missing jj fails rather than skips. Reading the variable also
		// keeps go's test cache from replaying a skip recorded without it.
		if os.Getenv("BUSYBEES_REQUIRE_JJ") != "" {
			t.Fatalf("BUSYBEES_REQUIRE_JJ is set and jj is not installed: %v", err)
		}
		t.Skip("jj is not installed")
	}
	cfg := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(cfg, []byte("user.name = \"Test\"\nuser.email = \"test@example.com\"\nui.paginate = \"never\"\nui.color = \"never\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JJ_CONFIG", cfg)
	s := vcstest.NewStack(t, conflict)
	run(t, s, "git", "init", "--colocate")
	return s
}

// run runs jj in the stack's directory and returns its stdout.
func run(t *testing.T, s *vcstest.Stack, args ...string) string {
	t.Helper()
	cmd := exec.Command("jj", args...)
	cmd.Dir = s.Dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("jj %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func logOf(t *testing.T, s *vcstest.Stack, rev, template string) string {
	t.Helper()
	return run(t, s, "log", "--no-graph", "-r", rev, "-T", template)
}

const authorAndMessage = `author.name() ++ " <" ++ author.email() ++ ">\n" ++ description`

// The child is duplicated from its old base, the parent's tip, onto the
// new upstream that has the parent squashed: the parent's commits do not
// come back, the child's two revisions stay two with their descriptions
// and author, and no bookmark moves.
func TestReplayFromTheOldBaseOntoASquashedParent(t *testing.T) {
	s := newStack(t, false)
	ctx := context.Background()
	r, err := Replayer{}.Replay(ctx, vcs.Directory(s.Dir), vcs.ReplayRequest{OldBase: "parent", Head: "child", Onto: "main"})
	if err != nil {
		t.Fatal(err)
	}
	if r.InProgress || r.Candidate == "" || len(r.Conflicts) != 0 {
		t.Fatalf("replay: %+v", r)
	}
	if want := (vcs.ReplayRequest{OldBase: s.OldBase, Head: s.Head, Onto: s.Onto}); r.Request != want {
		t.Errorf("request %+v, want it resolved to %+v", r.Request, want)
	}
	if len(r.Commits) != 2 || r.Commits[0].Original != s.Child[0] || r.Commits[1].Original != s.Child[1] || r.Commits[1].Replayed != r.Candidate {
		t.Fatalf("commits: %+v", r.Commits)
	}
	if got := strings.Fields(logOf(t, s, s.Onto+".."+r.Candidate, `commit_id ++ "\n"`)); !slices.Equal(got, []string{r.Candidate, r.Commits[0].Replayed}) {
		t.Errorf("revisions on the upstream: %v, want the two replayed", got)
	}
	for _, p := range s.Parent {
		if got := logOf(t, s, p+" & ::"+r.Candidate, "commit_id"); got != "" {
			t.Errorf("the parent's commit %s is under the candidate", p)
		}
	}
	for i, c := range r.Commits {
		if got, want := logOf(t, s, c.Replayed, authorAndMessage), logOf(t, s, c.Original, authorAndMessage); got != want || !strings.HasPrefix(got, vcstest.ChildAuthor) {
			t.Errorf("replayed revision %d: %q, want the original's %q", i, got, want)
		}
	}
	if got := s.Git(t, "show", r.Candidate+":feature.txt"); got != "one\ntwo" {
		t.Errorf("feature.txt at the candidate: %q", got)
	}
	for bookmark, want := range map[string]string{"child": s.Head, "parent": s.OldBase, "main": s.Onto} {
		if got := logOf(t, s, bookmark, "commit_id"); got != want {
			t.Errorf("bookmark %s moved to %s from %s", bookmark, got, want)
		}
	}
	if got := logOf(t, s, "@-", "commit_id"); got != r.Candidate {
		t.Errorf("the working copy is on %s, want the candidate %s", got, r.Candidate)
	}
	if st, err := (Replayer{}).ReplayStatus(ctx, vcs.Directory(s.Dir)); err != nil || st.InProgress {
		t.Errorf("status after a finished replay: %+v, %v", st, err)
	}
}

// A replay that conflicts stops on the first revision that does, with its
// paths sorted, and stays in progress across a restart: a new Replayer
// reads it, keeps it stopped while a path still conflicts, goes on to the
// next revision that conflicts, and finishes after the last.
func TestAConflictingReplayIsResumedAfterARestart(t *testing.T) {
	s := newStack(t, true)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	req := vcs.ReplayRequest{OldBase: s.OldBase, Head: s.Head, Onto: s.Onto}
	r, err := Replayer{}.Replay(ctx, ws, req)
	if err != nil {
		t.Fatal(err)
	}
	if !r.InProgress || r.Candidate != "" || !slices.Equal(r.Conflicts, []string{"a.txt", "b.txt"}) {
		t.Fatalf("replay: %+v, want it stopped on a.txt and b.txt", r)
	}
	if !strings.Contains(s.Read(t, "a.txt"), "<<<<<<<") {
		t.Errorf("a.txt holds no conflict markers:\n%s", s.Read(t, "a.txt"))
	}
	if _, err := (Replayer{}).Replay(ctx, ws, req); !errors.Is(err, vcs.ErrReplayInProgress) {
		t.Errorf("a second replay: %v, want ErrReplayInProgress", err)
	}

	// A new process.
	st, err := Replayer{}.ReplayStatus(ctx, ws)
	if err != nil || !st.InProgress || !slices.Equal(st.Conflicts, []string{"a.txt", "b.txt"}) {
		t.Fatalf("status: %+v, %v", st, err)
	}
	s.Write(t, "a.txt", "a resolved\n")
	if r, err := (Replayer{}).Continue(ctx, ws); !errors.Is(err, vcs.ErrUnresolved) || !slices.Equal(r.Conflicts, []string{"b.txt"}) {
		t.Fatalf("continue with b.txt unresolved: %+v, %v", r, err)
	}
	s.Write(t, "b.txt", "b resolved\n")
	r, err = Replayer{}.Continue(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !r.InProgress || !slices.Equal(r.Conflicts, []string{"c.txt"}) {
		t.Fatalf("after the first resolution: %+v, want it stopped on c.txt", r)
	}
	s.Write(t, "c.txt", "c resolved\n")
	r, err = Replayer{}.Continue(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if r.InProgress || r.Candidate == "" || r.Commits[1].Replayed != r.Candidate {
		t.Fatalf("after the last resolution: %+v", r)
	}
	for file, want := range map[string]string{"a.txt": "a resolved", "b.txt": "b resolved", "c.txt": "c resolved", "later.txt": "later", "child.txt": "uses one and two"} {
		if got := s.Git(t, "show", r.Candidate+":"+file); got != want {
			t.Errorf("%s at the candidate: %q, want %q", file, got, want)
		}
	}
	if got := logOf(t, s, "conflicts() & "+s.Onto+"::", "commit_id"); got != "" {
		t.Errorf("revisions still conflict: %s", got)
	}
	if got := strings.Fields(logOf(t, s, s.Onto+".."+r.Candidate, `commit_id ++ "\n"`)); len(got) != 2 {
		t.Errorf("revisions on the upstream: %v, want the child's 2", got)
	}
	for i, c := range r.Commits {
		if got, want := logOf(t, s, c.Replayed, authorAndMessage), logOf(t, s, c.Original, authorAndMessage); got != want {
			t.Errorf("replayed revision %d: %q, want %q", i, got, want)
		}
	}
	if got := logOf(t, s, "child", "commit_id"); got != s.Head {
		t.Errorf("the child bookmark moved to %s", got)
	}
	if _, err := (Replayer{}).Continue(ctx, ws); !errors.Is(err, vcs.ErrNoReplay) {
		t.Errorf("continue after the end: %v, want ErrNoReplay", err)
	}
}

// Abort abandons what the replay created and returns the working copy to
// where it was.
func TestAbortReturnsTheWorkingCopy(t *testing.T) {
	s := newStack(t, true)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	if r, err := (Replayer{}).Replay(ctx, ws, vcs.ReplayRequest{OldBase: s.OldBase, Head: s.Head, Onto: s.Onto}); err != nil || !r.InProgress {
		t.Fatalf("replay: %+v, %v", r, err)
	}
	if err := (Replayer{}).Abort(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := logOf(t, s, "@-", "commit_id"); got != s.Head {
		t.Errorf("the working copy is on %s after abort, want the child %s", got, s.Head)
	}
	if got := logOf(t, s, s.Onto+":: ~ "+s.Onto, "commit_id"); got != "" {
		t.Errorf("revisions left on the upstream after abort: %s", got)
	}
	if err := (Replayer{}).Abort(ctx, ws); !errors.Is(err, vcs.ErrNoReplay) {
		t.Errorf("abort again: %v, want ErrNoReplay", err)
	}
}

// A replay takes an old base that is an ancestor of the head and a range
// with no merge.
func TestReplayRefuses(t *testing.T) {
	s := newStack(t, false)
	ctx := context.Background()
	for name, req := range map[string]vcs.ReplayRequest{
		"old base not an ancestor": {OldBase: s.Onto, Head: s.Head, Onto: s.Onto},
		"unknown revision":         {OldBase: "nope", Head: s.Head, Onto: s.Onto},
	} {
		if _, err := (Replayer{}).Replay(ctx, vcs.Directory(s.Dir), req); err == nil {
			t.Errorf("%s: replayed", name)
		}
	}
	run(t, s, "new", "child", "main", "-m", "merge")
	merged := logOf(t, s, "@", "commit_id")
	run(t, s, "new", "child")
	if _, err := (Replayer{}).Replay(ctx, vcs.Directory(s.Dir), vcs.ReplayRequest{OldBase: s.OldBase, Head: merged, Onto: s.Onto}); err == nil || !strings.Contains(err.Error(), "merge") {
		t.Errorf("a range with a merge: %v", err)
	}
}
