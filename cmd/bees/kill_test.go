package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

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
