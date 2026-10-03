package skills

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeGit "clones" by copying a local fixture directory and otherwise
// answers every command with empty, successful output, the same fake
// core/skills' own tests use.
func fakeGit(fixture string) func(ctx context.Context, dir string, args ...string) (string, error) {
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		if args[0] != "clone" {
			return "", nil
		}
		dest := args[len(args)-1]
		return "", os.CopyFS(dest, os.DirFS(fixture))
	}
}

func mk(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustExist(t *testing.T, p string) {
	t.Helper()
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("missing %s: %v", p, err)
	}
}

// TestCacheDirEnv checks CacheDir is the one place $BEES_CACHE_DIR is read:
// set, it wins; unset, it falls back to DefaultCacheDir.
func TestCacheDirEnv(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BEES_CACHE_DIR", dir)
	if got := CacheDir(); got != dir {
		t.Errorf("CacheDir() = %q, want %q", got, dir)
	}
	t.Setenv("BEES_CACHE_DIR", "")
	if got := CacheDir(); got != DefaultCacheDir() {
		t.Errorf("CacheDir() = %q, want the default %q", got, DefaultCacheDir())
	}
}

// TestPrepareLayouts checks the three layouts busybees accepts today are
// still accepted through this adapter, in the manager's default mode: a
// full plugin used as-is, a single skill and a skills collection, each
// wrapped. The detailed layout behaviour (symlink targets, rejection
// messages, SkillsOnly mode, ...) is core/skills' own coverage.
func TestPrepareLayouts(t *testing.T) {
	fixtures := t.TempDir()
	single := filepath.Join(fixtures, "single")
	mk(t, filepath.Join(single, "SKILL.md"), "---\nname: single\n---\n")
	mk(t, filepath.Join(single, ".git", "HEAD"), "")

	coll := filepath.Join(fixtures, "coll")
	mk(t, filepath.Join(coll, "skills", "a", "SKILL.md"), "")
	mk(t, filepath.Join(coll, ".git", "HEAD"), "")

	plug := filepath.Join(fixtures, "plug")
	mk(t, filepath.Join(plug, ".claude-plugin", "plugin.json"), `{"name":"plug"}`)
	mk(t, filepath.Join(plug, ".git", "HEAD"), "")

	for _, c := range []struct {
		name, fixture, ref string
		check              func(t *testing.T, dir string)
	}{
		{"single skill wrapped", single, "https://github.com/x/single", func(t *testing.T, dir string) {
			mustExist(t, filepath.Join(dir, ".claude-plugin", "plugin.json"))
			mustExist(t, filepath.Join(dir, "skills", "single", "SKILL.md"))
		}},
		{"skills collection wrapped", coll, "https://github.com/x/coll", func(t *testing.T, dir string) {
			mustExist(t, filepath.Join(dir, "skills", "a", "SKILL.md"))
		}},
		{"full plugin used as-is", plug, "https://github.com/x/plug", func(t *testing.T, dir string) {
			mustExist(t, filepath.Join(dir, ".claude-plugin", "plugin.json"))
			if _, err := os.Lstat(filepath.Join(dir, "skills")); err == nil {
				t.Fatalf("a full plugin must not gain a generated skills/: %s", dir)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := NewManager(t.TempDir())
			m.Git = fakeGit(c.fixture)
			dirs, err := m.Prepare(context.Background(), []string{c.ref})
			if err != nil {
				t.Fatalf("%s: %v", c.ref, err)
			}
			if len(dirs) != 1 {
				t.Fatalf("%s: dirs %v", c.ref, dirs)
			}
			c.check(t, dirs[0])
		})
	}
}

// TestConfigurationMapsOntoRefreshPolicy checks RefreshAfter and
// RefreshAlways, the fields cmd/bees and internal/doctor set directly from
// bees.toml, reach core/skills' refresh policy: never refreshes below the
// age, always refreshes every time. The exact staleness arithmetic is
// core/skills' own coverage (ParseRefresh, RefreshEvery).
func TestConfigurationMapsOntoRefreshPolicy(t *testing.T) {
	fixture := t.TempDir()
	mk(t, filepath.Join(fixture, "SKILL.md"), "---\nname: fix\n---\n")
	mk(t, filepath.Join(fixture, ".git", "HEAD"), "")
	const ref = "https://github.com/x/fix"

	countingGit := func(n *int) func(ctx context.Context, dir string, args ...string) (string, error) {
		real := fakeGit(fixture)
		return func(ctx context.Context, dir string, args ...string) (string, error) {
			if len(args) > 0 && args[0] == "pull" {
				*n++
			}
			return real(ctx, dir, args...)
		}
	}

	t.Run("RefreshAfter zero never pulls", func(t *testing.T) {
		var pulls int
		m := NewManager(t.TempDir())
		m.Git = countingGit(&pulls)
		now := time.Now()
		m.Now = func() time.Time { return now }
		prepare(t, m, ref)
		now = now.Add(30 * 24 * time.Hour)
		prepare(t, m, ref)
		if pulls != 0 {
			t.Fatalf("pulls = %d, want 0", pulls)
		}
	})

	t.Run("RefreshAlways pulls every time", func(t *testing.T) {
		var pulls int
		m := NewManager(t.TempDir())
		m.Git = countingGit(&pulls)
		m.RefreshAlways = true
		prepare(t, m, ref)
		prepare(t, m, ref)
		if pulls != 1 {
			t.Fatalf("pulls = %d, want 1 (the first Prepare clones, the second pulls)", pulls)
		}
	})

	t.Run("RefreshAfter pulls only once the age is reached", func(t *testing.T) {
		var pulls int
		m := NewManager(t.TempDir())
		m.Git = countingGit(&pulls)
		m.RefreshAfter = time.Hour
		now := time.Now()
		m.Now = func() time.Time { return now }
		prepare(t, m, ref)
		now = now.Add(30 * time.Minute)
		prepare(t, m, ref)
		if pulls != 0 {
			t.Fatalf("pulls = %d, want 0 before RefreshAfter elapses", pulls)
		}
		now = now.Add(time.Hour)
		prepare(t, m, ref)
		if pulls != 1 {
			t.Fatalf("pulls = %d, want 1 once RefreshAfter elapses", pulls)
		}
	})
}

func prepare(t *testing.T, m *Manager, ref string) string {
	t.Helper()
	dirs, err := m.Prepare(context.Background(), []string{ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 {
		t.Fatalf("dirs %v", dirs)
	}
	return dirs[0]
}
