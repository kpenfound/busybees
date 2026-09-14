package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/versions"
)

// withDir inserts project.dir = dir into botTOML's [project] table (not
// appended at the end, which would land inside [github] instead).
func withDir(dir string) string {
	return strings.Replace(botTOML, "state_dir = \".bees\"\n", "state_dir = \".bees\"\ndir = \""+filepath.ToSlash(dir)+"\"\n", 1)
}

// TestNewAppProjectDir covers #661: bees.toml living outside the git clone it
// manages, project.dir naming where the clone actually is. newApp's git-clone
// check, and the workspace manager it builds, both have to follow project.dir
// rather than assume bees.toml's own directory is the clone.
func TestNewAppProjectDir(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")

	t.Run("project.dir unset behaves as today", func(t *testing.T) {
		path := setupBotFactory(t, botTOML)
		a, err := newApp(context.Background(), &globalFlags{config: path})
		if err != nil {
			t.Fatal(err)
		}
		if a.cfg.CloneDir() != a.cfg.Dir() {
			t.Fatalf("clone dir: got %s, want %s", a.cfg.CloneDir(), a.cfg.Dir())
		}
	})

	t.Run("project.dir set to a clone elsewhere passes and runs normally", func(t *testing.T) {
		_, clone := testutil.SetupRepos(t)
		confDir := t.TempDir()
		path := filepath.Join(confDir, "bees.toml")
		if err := os.WriteFile(path, []byte(withDir(clone)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BEES_TEST_TOKEN", "ghp_bot")
		fakeMe(t, "kyle")

		a, err := newApp(context.Background(), &globalFlags{config: path})
		if err != nil {
			t.Fatal(err)
		}
		if a.cfg.CloneDir() != clone {
			t.Fatalf("clone dir: got %s, want %s", a.cfg.CloneDir(), clone)
		}
		if a.ws.MainRepo != clone {
			t.Errorf("workspace manager main repo: got %s, want %s", a.ws.MainRepo, clone)
		}
	})

	t.Run("project.dir pointing at a non-clone fails with an actionable error", func(t *testing.T) {
		confDir := t.TempDir()
		notAClone := t.TempDir()
		path := filepath.Join(confDir, "bees.toml")
		if err := os.WriteFile(path, []byte(withDir(notAClone)), 0o644); err != nil {
			t.Fatal(err)
		}
		t.Setenv("BEES_TEST_TOKEN", "ghp_bot")
		fakeMe(t, "kyle")

		_, err := newApp(context.Background(), &globalFlags{config: path})
		if err == nil {
			t.Fatal("expected an error: project.dir does not name a git clone")
		}
		if !strings.Contains(err.Error(), notAClone) || !strings.Contains(err.Error(), "project.dir") {
			t.Errorf("error does not name the directory or project.dir: %v", err)
		}
	})
}
