package config

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

func TestResolveFromGit(t *testing.T) {
	ctx := context.Background()
	origin, clone := testutil.SetupRepos(t)
	// Give the remote a GitHub-looking URL for repo detection; DefaultBranch
	// detection uses the real local remote through a second remote name.
	if _, err := workspace.Git(ctx, clone, "remote", "add", "gh", "git@github.com:acme/widgets.git"); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Git(ctx, clone, "remote", "set-head", "origin", "main"); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(clone, "bees.toml")
	if err := os.WriteFile(p, []byte("version = 1\n[project]\nrepo = \"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	// origin is a local path: repo cannot be derived, branch can.
	if err := cfg.Resolve(ctx); err == nil {
		t.Fatal("expected an error deriving repo from a local remote")
	}
	cfg.Project.Repo = ""
	cfg.Project.Remote = "gh"
	cfg.Project.DefaultBranch = "main"
	if err := cfg.Resolve(ctx); err != nil || cfg.Project.Repo != "acme/widgets" {
		t.Fatalf("repo from remote url: %q %v", cfg.Project.Repo, err)
	}
	cfg.Project.Remote = "origin"
	cfg.Project.DefaultBranch = ""
	if err := cfg.Resolve(ctx); err != nil || cfg.Project.DefaultBranch != "main" {
		t.Fatalf("default branch from remote HEAD: %q %v", cfg.Project.DefaultBranch, err)
	}
	// Without the symref, ls-remote answers.
	if _, err := workspace.Git(ctx, clone, "remote", "set-head", "origin", "--delete"); err != nil {
		t.Fatal(err)
	}
	branch, err := workspace.DefaultBranch(ctx, clone, "origin")
	if err != nil || branch != "main" {
		t.Fatalf("ls-remote fallback: %q %v (origin %s)", branch, err, origin)
	}
}
