// Package testutil holds helpers shared by tests.
package testutil

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/busybees/internal/workspace"
)

// SetupRepos creates a bare origin with one commit on main and a clone of it.
func SetupRepos(t *testing.T) (origin, clone string) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	origin = filepath.Join(root, "origin.git")
	clone = filepath.Join(root, "main")
	must := func(dir string, args ...string) {
		t.Helper()
		if _, err := workspace.Git(ctx, dir, args...); err != nil {
			t.Fatal(err)
		}
	}
	must(root, "init", "--bare", "--initial-branch=main", origin)
	must(root, "clone", "--quiet", origin, clone)
	must(clone, "config", "user.email", "test@example.com")
	must(clone, "config", "user.name", "test")
	must(clone, "checkout", "-q", "-b", "main")
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("# test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	must(clone, "add", ".")
	must(clone, "commit", "-q", "-m", "init")
	must(clone, "push", "-q", "-u", "origin", "main")
	return origin, clone
}
