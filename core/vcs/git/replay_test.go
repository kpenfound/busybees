package git

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/core/vcs/internal/vcstest"
)

// show formats one commit.
func show(t *testing.T, s *vcstest.Stack, commit, format string) string {
	t.Helper()
	return s.Git(t, "show", "--no-patch", "--format="+format, commit)
}

// The child is replayed from its old base, the parent's tip, onto the new
// upstream that has the parent squashed: the parent's commits do not come
// back, the child's two commits stay two with their messages and author,
// no branch moves, and the same replay again gives the same commits.
func TestReplayFromTheOldBaseOntoASquashedParent(t *testing.T) {
	s := vcstest.NewStack(t, false)
	ctx := context.Background()
	req := vcs.ReplayRequest{OldBase: "parent", Head: "child", Onto: "main"}
	r, err := Replayer{}.Replay(ctx, vcs.Directory(s.Dir), req)
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
		t.Fatalf("commits: %+v, want the child's two with the candidate last", r.Commits)
	}
	// The candidate is the child's two commits on the new upstream, and
	// nothing of the parent's history.
	if got := strings.Fields(s.Git(t, "rev-list", "--reverse", s.Onto+".."+r.Candidate)); !slices.Equal(got, []string{r.Commits[0].Replayed, r.Candidate}) {
		t.Errorf("commits on the upstream: %v, want the two replayed", got)
	}
	for _, p := range s.Parent {
		if succeeds(s, "merge-base", "--is-ancestor", p, r.Candidate) {
			t.Errorf("the parent's commit %s is under the candidate", p)
		}
	}
	for i, c := range r.Commits {
		if got, want := show(t, s, c.Replayed, "%an <%ae> %ad%n%B"), show(t, s, c.Original, "%an <%ae> %ad%n%B"); got != want {
			t.Errorf("replayed commit %d: %q, want the original's author and message %q", i, got, want)
		}
		if got, want := show(t, s, c.Replayed, "%cn <%ce> %cd"), show(t, s, c.Original, "%cn <%ce> %cd"); got != want {
			t.Errorf("replayed commit %d committer: %q, want the original's %q", i, got, want)
		}
	}
	if got := s.Git(t, "show", r.Candidate+":feature.txt"); got != "one\ntwo" {
		t.Errorf("feature.txt at the candidate: %q", got)
	}
	// No branch moved; the working tree is at the candidate.
	for branch, want := range map[string]string{"child": s.Head, "parent": s.OldBase, "main": s.Onto} {
		if got := s.Git(t, "rev-parse", branch); got != want {
			t.Errorf("branch %s moved to %s from %s", branch, got, want)
		}
	}
	if got := s.Git(t, "rev-parse", "HEAD"); got != r.Candidate {
		t.Errorf("HEAD %s, want the candidate %s", got, r.Candidate)
	}
	if st, err := (Replayer{}).ReplayStatus(ctx, vcs.Directory(s.Dir)); err != nil || st.InProgress {
		t.Errorf("status after a finished replay: %+v, %v", st, err)
	}

	again, err := Replayer{}.Replay(ctx, vcs.Directory(s.Dir), req)
	if err != nil {
		t.Fatal(err)
	}
	if again.Candidate != r.Candidate || !slices.Equal(again.Commits, r.Commits) {
		t.Errorf("the same replay again: %+v, want %+v", again.Commits, r.Commits)
	}
}

// succeeds runs git and reports whether it succeeded.
func succeeds(s *vcstest.Stack, args ...string) bool {
	_, err := Replayer{}.run(context.Background(), s.Dir, nil, args...)
	return err == nil
}

// A replay that conflicts stops with the conflicting paths sorted and
// stays in progress across a restart: a new Replayer reads it, refuses a
// resolution that still has markers, goes on to the next conflict after a
// resolution, and finishes after the last.
func TestAConflictingReplayIsResumedAfterARestart(t *testing.T) {
	s := vcstest.NewStack(t, true)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	r, err := Replayer{}.Replay(ctx, ws, vcs.ReplayRequest{OldBase: s.OldBase, Head: s.Head, Onto: s.Onto})
	if err != nil {
		t.Fatal(err)
	}
	if !r.InProgress || r.Candidate != "" || !slices.Equal(r.Conflicts, []string{"a.txt", "b.txt"}) {
		t.Fatalf("replay: %+v, want it stopped on a.txt and b.txt", r)
	}
	if _, err := os.Stat(filepath.Join(s.Dir, ".git", StateFile)); err != nil {
		t.Fatalf("no state for a replay in progress: %v", err)
	}
	if _, err := (Replayer{}).Replay(ctx, ws, vcs.ReplayRequest{OldBase: s.OldBase, Head: s.Head, Onto: s.Onto}); !errors.Is(err, vcs.ErrReplayInProgress) {
		t.Errorf("a second replay: %v, want ErrReplayInProgress", err)
	}

	// A new process.
	st, err := Replayer{}.ReplayStatus(ctx, ws)
	if err != nil || !st.InProgress || !slices.Equal(st.Conflicts, []string{"a.txt", "b.txt"}) || st.Commits[0].Replayed != "" {
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
	if !r.InProgress || !slices.Equal(r.Conflicts, []string{"c.txt"}) || r.Commits[0].Replayed == "" {
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
	if got := s.Git(t, "rev-list", "--count", s.Onto+".."+r.Candidate); got != "2" {
		t.Errorf("%s commits on the upstream, want the child's 2", got)
	}
	for i, c := range r.Commits {
		if got, want := show(t, s, c.Replayed, "%an <%ae>%n%B"), show(t, s, c.Original, "%an <%ae>%n%B"); got != want {
			t.Errorf("replayed commit %d: %q, want %q", i, got, want)
		}
	}
	if got := s.Git(t, "rev-parse", "child"); got != s.Head {
		t.Errorf("the child branch moved to %s", got)
	}
	if _, err := (Replayer{}).Continue(ctx, ws); !errors.Is(err, vcs.ErrNoReplay) {
		t.Errorf("continue after the end: %v, want ErrNoReplay", err)
	}
}

// Abort returns the working tree to the branch it had and forgets the
// replay.
func TestAbortReturnsTheWorkingTree(t *testing.T) {
	s := vcstest.NewStack(t, true)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	if r, err := (Replayer{}).Replay(ctx, ws, vcs.ReplayRequest{OldBase: s.OldBase, Head: s.Head, Onto: s.Onto}); err != nil || !r.InProgress {
		t.Fatalf("replay: %+v, %v", r, err)
	}
	if err := (Replayer{}).Abort(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if got := s.Git(t, "symbolic-ref", "HEAD"); got != "refs/heads/child" {
		t.Errorf("HEAD after abort: %s", got)
	}
	if got := s.Git(t, "status", "--porcelain"); got != "" {
		t.Errorf("working tree after abort:\n%s", got)
	}
	if st, err := (Replayer{}).ReplayStatus(ctx, ws); err != nil || st.InProgress {
		t.Errorf("status after abort: %+v, %v", st, err)
	}
	if err := (Replayer{}).Abort(ctx, ws); !errors.Is(err, vcs.ErrNoReplay) {
		t.Errorf("abort again: %v, want ErrNoReplay", err)
	}
	if r, err := (Replayer{}).Replay(ctx, ws, vcs.ReplayRequest{OldBase: s.OldBase, Head: s.Head, Onto: s.Onto}); err != nil || !r.InProgress {
		t.Errorf("replay after abort: %+v, %v", r, err)
	}
}

// A process that stopped after a commit was replayed and before its state
// said so goes on from that commit instead of replaying it twice.
func TestContinueAfterAStopBetweenCommits(t *testing.T) {
	s := vcstest.NewStack(t, true)
	ctx := context.Background()
	ws := vcs.Directory(s.Dir)
	if _, err := (Replayer{}).Replay(ctx, ws, vcs.ReplayRequest{OldBase: s.OldBase, Head: s.Head, Onto: s.Onto}); err != nil {
		t.Fatal(err)
	}
	s.Write(t, "a.txt", "a resolved\n")
	s.Write(t, "b.txt", "b resolved\n")
	s.Git(t, "add", "--all")
	s.Git(t, "-c", "user.name=x", "-c", "user.email=x@x", "commit", "--quiet", "--no-edit")
	r, err := Replayer{}.Continue(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	if !r.InProgress || !slices.Equal(r.Conflicts, []string{"c.txt"}) {
		t.Fatalf("continue: %+v, want it stopped on c.txt", r)
	}
	if got := s.Git(t, "rev-list", "--count", s.Onto+"..HEAD"); got != "1" {
		t.Errorf("%s commits replayed, want 1", got)
	}
}

// A replay takes an old base that is an ancestor of the head and a range
// with no merge, and a working tree with no changes to tracked files.
func TestReplayRefuses(t *testing.T) {
	ctx := context.Background()
	s := vcstest.NewStack(t, false)
	for name, req := range map[string]vcs.ReplayRequest{
		"old base not an ancestor": {OldBase: s.Onto, Head: s.Head, Onto: s.Onto},
		"unknown revision":         {OldBase: "nope", Head: s.Head, Onto: s.Onto},
	} {
		if _, err := (Replayer{}).Replay(ctx, vcs.Directory(s.Dir), req); err == nil {
			t.Errorf("%s: replayed", name)
		}
	}

	s.Git(t, "checkout", "--quiet", "-b", "merged", s.Head)
	s.Git(t, "-c", "user.name=x", "-c", "user.email=x@x", "merge", "--quiet", "--no-ff", "--no-edit", "main")
	if _, err := (Replayer{}).Replay(ctx, vcs.Directory(s.Dir), vcs.ReplayRequest{OldBase: s.OldBase, Head: "merged", Onto: s.Onto}); err == nil || !strings.Contains(err.Error(), "merge") {
		t.Errorf("a range with a merge: %v", err)
	}

	s.Git(t, "checkout", "--quiet", "child")
	s.Write(t, "a.txt", "dirty\n")
	if _, err := (Replayer{}).Replay(ctx, vcs.Directory(s.Dir), vcs.ReplayRequest{OldBase: s.OldBase, Head: s.Head, Onto: s.Onto}); err == nil || !strings.Contains(err.Error(), "changes") {
		t.Errorf("a changed working tree: %v", err)
	}
}
