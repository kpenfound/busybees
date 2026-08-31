package workspace_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

func TestBranchAndDetached(t *testing.T) {
	ctx := context.Background()
	_, clone := testutil.SetupRepos(t)
	m := workspace.NewManager(clone, filepath.Join(t.TempDir(), "ws"))
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}

	// New branch from origin/main.
	ws, err := m.Branch(ctx, "dev", "bees/issue-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if out, _ := workspace.Git(ctx, ws.RepoDir, "rev-parse", "--abbrev-ref", "HEAD"); out != "bees/issue-1" {
		t.Fatalf("branch: %s", out)
	}
	if err := os.WriteFile(filepath.Join(ws.RepoDir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "."}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "-m", "a"}, {"push", "-q", "-u", "origin", "HEAD"}} {
		if _, err := workspace.Git(ctx, ws.RepoDir, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := m.Remove(ctx, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(ws.Root); !os.IsNotExist(err) {
		t.Fatal("workspace root should be gone")
	}

	// Same branch again: exists locally and remotely, must be reused.
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}
	ws2, err := m.Branch(ctx, "dev", "bees/issue-1", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(ws2.RepoDir, "a.txt")); err != nil {
		t.Fatal("branch content not reused")
	}
	_ = m.Remove(ctx, ws2)

	// Detached checkout of main must not contain the branch's file.
	d, err := m.Detached(ctx, "qa", "main")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.RepoDir, "a.txt")); err == nil {
		t.Fatal("detached main should not have a.txt")
	}
	_ = m.Remove(ctx, d)

	out, _ := workspace.Git(ctx, clone, "worktree", "list")
	if strings.Count(out, "\n") != 0 {
		t.Fatalf("worktrees not cleaned up:\n%s", out)
	}
}

// TestConcurrentWorkspacesDoNotRace is the regression guard for #133: every
// workspace used to check out into a directory named "repo", so `git worktree
// add` had to allocate repo, repo1, repo2, … by scanning .git/worktrees/ and
// then creating the entry — not atomically. Two concurrent adds could pick the
// same id and one of them read a half-written commondir.
func TestConcurrentWorkspacesDoNotRace(t *testing.T) {
	ctx := context.Background()
	_, clone := testutil.SetupRepos(t)
	m := workspace.NewManager(clone, filepath.Join(t.TempDir(), "ws"))
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}

	const n = 8
	var wg sync.WaitGroup
	spaces := make([]*workspace.Workspace, n)
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			spaces[i], errs[i] = m.Detached(ctx, fmt.Sprintf("role-%d", i), "main")
		}()
	}
	wg.Wait()

	seen := map[string]int{}
	ids := map[string]int{}
	for i := range n {
		if errs[i] != nil {
			t.Errorf("workspace %d: %v", i, errs[i])
			continue
		}
		if prev, dup := seen[spaces[i].RepoDir]; dup {
			t.Errorf("workspace %d reuses the directory of workspace %d: %s", i, prev, spaces[i].RepoDir)
		}
		seen[spaces[i].RepoDir] = i

		// The id git allocated under .git/worktrees/ must be the workspace's
		// own leaf name. When it is not, git had to pick a numbered suffix,
		// which is the allocation this test guards against.
		id := worktreeID(t, spaces[i].RepoDir)
		if want := filepath.Base(spaces[i].RepoDir); id != want {
			t.Errorf("workspace %d got worktree id %q, want %q", i, id, want)
		}
		if prev, dup := ids[id]; dup {
			t.Errorf("workspace %d shares worktree id %q with workspace %d", i, id, prev)
		}
		ids[id] = i
	}
	if t.Failed() {
		return
	}

	if got := worktreeCount(ctx, t, clone); got != n+1 {
		t.Fatalf("worktree list: got %d worktrees, want %d (the clone plus %d workspaces)", got, n+1, n)
	}
	for i := range n {
		if err := m.Remove(ctx, spaces[i]); err != nil {
			t.Errorf("remove %d: %v", i, err)
		}
	}
	if got := worktreeCount(ctx, t, clone); got != 1 {
		t.Fatalf("after removal: got %d worktrees, want 1 (the clone)", got)
	}
}

// TestRepoDirIsNamedAfterTheWorkspace pins the property the concurrency guard
// relies on: the leaf name is neither the constant "repo" nor derived from the
// caller's name, both of which two live workspaces can share.
func TestRepoDirIsNamedAfterTheWorkspace(t *testing.T) {
	ctx := context.Background()
	_, clone := testutil.SetupRepos(t)
	m := workspace.NewManager(clone, filepath.Join(t.TempDir(), "ws"))
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}

	a, err := m.Detached(ctx, "developer", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Remove(ctx, a) }()
	b, err := m.Detached(ctx, "developer", "main")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = m.Remove(ctx, b) }()

	for _, ws := range []*workspace.Workspace{a, b} {
		if leaf := filepath.Base(ws.RepoDir); leaf == "repo" {
			t.Errorf("RepoDir leaf is the constant %q: %s", leaf, ws.RepoDir)
		}
		if filepath.Dir(ws.RepoDir) != ws.Root {
			t.Errorf("RepoDir %s is not inside Root %s", ws.RepoDir, ws.Root)
		}
	}
	if filepath.Base(a.RepoDir) == filepath.Base(b.RepoDir) {
		t.Fatalf("two workspaces of the same name share a leaf name: %s", a.RepoDir)
	}
}

// worktreeID reads the id git gave the worktree in repoDir from its .git
// file ("gitdir: <main>/.git/worktrees/<id>").
func worktreeID(t *testing.T, repoDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(repoDir, ".git"))
	if err != nil {
		t.Fatal(err)
	}
	gitdir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok {
		t.Fatalf("unexpected .git file in %s: %q", repoDir, b)
	}
	return filepath.Base(gitdir)
}

func worktreeCount(ctx context.Context, t *testing.T, clone string) int {
	t.Helper()
	out, err := workspace.Git(ctx, clone, "worktree", "list", "--porcelain")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(line, "worktree ") {
			n++
		}
	}
	return n
}

// TestFetchDoesNotRaceWorktreeOperations guards #230: `git fetch` in the main
// clone enumerates .git/worktrees/<id>/HEAD to build its "have" set, so it can
// read an entry a concurrent `git worktree add` is still writing and fail the
// whole fetch with:
//
//	fatal: bad object worktrees/<id>/HEAD
//	error: <origin> did not send all necessary objects
//
// This is not the id-allocation race of #133/#142 (unique leaf names fixed
// that): it is a read of one clone's .git against a write of it. The scheduler
// overlaps exactly these two — runSingleton calls Fetch while developer
// workers call Branch — so Manager serialises both behind one mutex, and every
// call below must return without error.
//
// The width (24 workers) matters more than the depth: without the mutex this
// fails on the first few rounds, 15 runs out of 15. The failure seen on macOS
// is a sibling of the reported one rather than the fetch message above — `git
// worktree add` racing the implicit prune inside another `worktree
// remove`/`prune` ("failed to read .git/worktrees/<id>/commondir") — but it is
// the same defect: concurrent git commands on one .git directory.
func TestFetchDoesNotRaceWorktreeOperations(t *testing.T) {
	ctx := context.Background()
	_, clone := testutil.SetupRepos(t)
	m := workspace.NewManager(clone, filepath.Join(t.TempDir(), "ws"))
	if err := m.Fetch(ctx); err != nil {
		t.Fatal(err)
	}

	const (
		workers = 24
		rounds  = 8
	)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var failures []string
	fail := func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		if len(failures) < 20 {
			failures = append(failures, fmt.Sprintf(format, args...))
		}
	}

	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for r := range rounds {
				// A third of the workers fetch and the rest churn worktrees,
				// so a fetch always has adds, removes and prunes running
				// against the same .git.
				if i%3 == 0 {
					if err := m.Fetch(ctx); err != nil {
						fail("worker %d round %d: fetch: %v", i, r, err)
						return
					}
					continue
				}
				name := fmt.Sprintf("w%d-r%d", i, r)
				var (
					ws  *workspace.Workspace
					err error
				)
				if r%2 == 0 {
					ws, err = m.Detached(ctx, name, "main")
				} else {
					ws, err = m.Branch(ctx, name, "bees/"+name, "main")
				}
				if err != nil {
					fail("worker %d round %d: create: %v", i, r, err)
					return
				}
				if err := m.Remove(ctx, ws); err != nil {
					fail("worker %d round %d: remove: %v", i, r, err)
					return
				}
				if err := m.Prune(ctx); err != nil {
					fail("worker %d round %d: prune: %v", i, r, err)
					return
				}
			}
		}()
	}
	wg.Wait()

	for _, f := range failures {
		t.Error(f)
	}
	if got := worktreeCount(ctx, t, clone); got != 1 {
		t.Fatalf("after the run: got %d worktrees, want 1 (the clone)", got)
	}
}
