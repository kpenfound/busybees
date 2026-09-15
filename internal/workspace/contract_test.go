package workspace_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

func TestProviderContract(t *testing.T) {
	ctx := context.Background()
	_, clone := testutil.SetupRepos(t)
	m := workspace.NewManager(clone, t.TempDir())
	var provider vcs.Provider = m
	for _, branch := range []string{"", "bees/contract"} {
		ws, err := provider.Acquire(ctx, vcs.Request{Name: "contract", Ref: "main", Branch: branch})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(ws.Directory()); err != nil {
			t.Fatal(err)
		}
		want, err := workspace.Git(ctx, ws.Directory(), "rev-parse", "--path-format=absolute", "--git-common-dir")
		if err != nil {
			t.Fatal(err)
		}
		if access := ws.VCS(); access == nil || len(access.Mounts) != 1 || access.Mounts[0] != filepath.Clean(want) {
			t.Fatalf("metadata: %+v, want %q", access, want)
		}
		// Keep and later forced release use exactly the acquired object.
		m.Keep = true
		if err := provider.Release(ctx, ws); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(ws.Directory()); err != nil {
			t.Fatalf("keep: %v", err)
		}
		m.Keep = false
		if err := os.WriteFile(filepath.Join(ws.Directory(), "untracked"), []byte("dirty"), 0600); err != nil {
			t.Fatal(err)
		}
		canceled, cancel := context.WithCancel(ctx)
		cancel()
		if err := provider.Release(context.WithoutCancel(canceled), ws); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(ws.Directory()); !os.IsNotExist(err) {
			t.Fatalf("release left directory: %v", err)
		}
		if n := worktreeCount(ctx, t, clone); n != 1 {
			t.Fatalf("release left metadata: %d", n)
		}
	}
	if err := provider.Release(ctx, vcs.Directory(t.TempDir())); err == nil {
		t.Fatal("accepted a workspace from another provider")
	}
	if err := provider.Prune(ctx); err != nil {
		t.Fatal(err)
	}
}
