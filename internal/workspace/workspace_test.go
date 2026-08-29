package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
