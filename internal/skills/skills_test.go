package skills

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in                     string
		url, ref, subdir, name string
	}{
		{"https://github.com/acme/skills", "https://github.com/acme/skills", "", "", "skills"},
		{"https://github.com/acme/skills.git@v1.2", "https://github.com/acme/skills.git", "v1.2", "", "skills"},
		{"https://github.com/acme/skills#skills/tdd", "https://github.com/acme/skills", "", "skills/tdd", "skills-tdd"},
		{"git@github.com:acme/My_Plugin.git@main#a/b", "git@github.com:acme/My_Plugin.git", "main", "a/b", "my-plugin-b"},
	}
	for _, c := range cases {
		s, err := Parse(c.in)
		if err != nil {
			t.Fatalf("%s: %v", c.in, err)
		}
		if s.URL != c.url || s.Ref != c.ref || s.Subdir != c.subdir || s.Name != c.name {
			t.Errorf("%s: got %+v", c.in, s)
		}
	}
	if _, err := Parse(""); err == nil {
		t.Fatal("expected error")
	}
}

// fakeGit "clones" by copying a local fixture directory.
func fakeGit(fixture string) func(ctx context.Context, dir string, args ...string) (string, error) {
	return func(ctx context.Context, dir string, args ...string) (string, error) {
		if args[0] != "clone" {
			return "", nil
		}
		dest := args[len(args)-1]
		return "", os.CopyFS(dest, os.DirFS(fixture))
	}
}

func TestPrepareLayouts(t *testing.T) {
	fixtures := t.TempDir()
	// single skill at root
	single := filepath.Join(fixtures, "single")
	mk(t, filepath.Join(single, "SKILL.md"), "---\nname: single\n---\n")
	mk(t, filepath.Join(single, ".git", "HEAD"), "")
	// collection
	coll := filepath.Join(fixtures, "coll")
	mk(t, filepath.Join(coll, "skills", "a", "SKILL.md"), "")
	mk(t, filepath.Join(coll, "skills", "b", "SKILL.md"), "")
	mk(t, filepath.Join(coll, ".git", "HEAD"), "")
	// plugin
	plug := filepath.Join(fixtures, "plug")
	mk(t, filepath.Join(plug, ".claude-plugin", "plugin.json"), `{"name":"plug"}`)
	mk(t, filepath.Join(plug, ".git", "HEAD"), "")

	for _, c := range []struct {
		fixture, ref string
		check        func(t *testing.T, dir string)
	}{
		{single, "https://github.com/x/single", func(t *testing.T, dir string) {
			mustExist(t, filepath.Join(dir, ".claude-plugin", "plugin.json"))
			mustExist(t, filepath.Join(dir, "skills", "single", "SKILL.md"))
		}},
		{coll, "https://github.com/x/coll", func(t *testing.T, dir string) {
			mustExist(t, filepath.Join(dir, "skills", "a", "SKILL.md"))
			mustExist(t, filepath.Join(dir, "skills", "b", "SKILL.md"))
		}},
		{coll, "https://github.com/x/coll#skills/b", func(t *testing.T, dir string) {
			mustExist(t, filepath.Join(dir, "skills", "coll-b", "SKILL.md"))
		}},
		{plug, "https://github.com/x/plug", func(t *testing.T, dir string) {
			if filepath.Base(filepath.Dir(dir)) != "repos" {
				t.Fatalf("plugin should be used in place, got %s", dir)
			}
		}},
	} {
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
	}

	// not a skill at all
	bad := filepath.Join(fixtures, "bad")
	mk(t, filepath.Join(bad, "README.md"), "")
	mk(t, filepath.Join(bad, ".git", "HEAD"), "")
	m := NewManager(t.TempDir())
	m.Git = fakeGit(bad)
	if _, err := m.Prepare(context.Background(), []string{"https://github.com/x/bad"}); err == nil {
		t.Fatal("expected error for non-skill repo")
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

// recorder is a fake git that copies a fixture on clone, records every
// invocation and can be told to fail pulls.
type recorder struct {
	fixture  string
	calls    [][]string
	pullErr  error
	commit   string
	commits  []string // successive rev-parse answers; the last one sticks
	revIndex int
}

func (r *recorder) git(ctx context.Context, dir string, args ...string) (string, error) {
	r.calls = append(r.calls, args)
	switch args[0] {
	case "clone":
		return "", os.CopyFS(args[len(args)-1], os.DirFS(r.fixture))
	case "pull":
		return "", r.pullErr
	case "rev-parse":
		switch {
		case r.revIndex < len(r.commits):
			c := r.commits[r.revIndex]
			r.revIndex++
			return c + "\n", nil
		case len(r.commits) > 0:
			return r.commits[len(r.commits)-1] + "\n", nil
		}
		return r.commit + "\n", nil
	}
	return "", nil
}

func (r *recorder) count(name string) int {
	n := 0
	for _, c := range r.calls {
		if c[0] == name {
			n++
		}
	}
	return n
}

// skillFixture is a minimal single-skill repository.
func skillFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "fixture")
	mk(t, filepath.Join(dir, "SKILL.md"), "---\nname: fix\n---\n")
	mk(t, filepath.Join(dir, ".git", "HEAD"), "")
	return dir
}

// testManager returns a manager with a fake git and a clock the test drives.
func testManager(t *testing.T, now *time.Time) (*Manager, *recorder) {
	t.Helper()
	rec := &recorder{fixture: skillFixture(t), commit: "abc1234"}
	m := NewManager(t.TempDir())
	m.Git = rec.git
	m.Now = func() time.Time { return *now }
	return m, rec
}

const testRef = "https://github.com/x/fix"

func TestRefreshPolicy(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("fresh clone does not pull and is stamped", func(t *testing.T) {
		now := start
		m, rec := testManager(t, &now)
		m.RefreshAfter = time.Hour
		if _, err := m.Prepare(context.Background(), []string{testRef}); err != nil {
			t.Fatal(err)
		}
		if rec.count("clone") != 1 || rec.count("pull") != 0 {
			t.Fatalf("calls %v", rec.calls)
		}
		spec, _ := Parse(testRef)
		if got := fetchedAt(m.repoDir(spec)); !got.Equal(start) {
			t.Fatalf("fetched at %v, want %v", got, start)
		}
	})

	t.Run("below RefreshAfter does not pull", func(t *testing.T) {
		now := start
		m, rec := testManager(t, &now)
		m.RefreshAfter = time.Hour
		prepare(t, m)
		now = start.Add(59 * time.Minute)
		prepare(t, m)
		if rec.count("pull") != 0 {
			t.Fatalf("calls %v", rec.calls)
		}
	})

	t.Run("at RefreshAfter pulls and moves the stamp", func(t *testing.T) {
		now := start
		m, rec := testManager(t, &now)
		m.RefreshAfter = time.Hour
		prepare(t, m)
		now = start.Add(time.Hour)
		prepare(t, m)
		if rec.count("pull") != 1 {
			t.Fatalf("calls %v", rec.calls)
		}
		spec, _ := Parse(testRef)
		if got := fetchedAt(m.repoDir(spec)); !got.Equal(now) {
			t.Fatalf("fetched at %v, want %v", got, now)
		}
		// The stamp moved, so the next session is inside the window again.
		prepare(t, m)
		if rec.count("pull") != 1 {
			t.Fatalf("pulled again: %v", rec.calls)
		}
	})

	t.Run("RefreshAfter zero never pulls", func(t *testing.T) {
		now := start
		m, rec := testManager(t, &now)
		prepare(t, m)
		now = start.Add(30 * 24 * time.Hour)
		prepare(t, m)
		if rec.count("pull") != 0 {
			t.Fatalf("calls %v", rec.calls)
		}
	})

	t.Run("RefreshAlways pulls every time", func(t *testing.T) {
		now := start
		m, rec := testManager(t, &now)
		m.RefreshAlways = true
		prepare(t, m)
		prepare(t, m)
		prepare(t, m)
		if rec.count("pull") != 2 {
			t.Fatalf("calls %v", rec.calls)
		}
	})

	t.Run("a missing stamp counts as stale", func(t *testing.T) {
		now := start
		m, rec := testManager(t, &now)
		m.RefreshAfter = time.Hour
		prepare(t, m)
		spec, _ := Parse(testRef)
		if err := os.Remove(stamp(m.repoDir(spec))); err != nil {
			t.Fatal(err)
		}
		prepare(t, m)
		if rec.count("pull") != 1 {
			t.Fatalf("calls %v", rec.calls)
		}
	})

	t.Run("a failed pull does not fail Prepare", func(t *testing.T) {
		now := start
		m, rec := testManager(t, &now)
		m.RefreshAlways = true
		m.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
		prepare(t, m)
		rec.pullErr = errors.New("You are not currently on a branch")
		dirs, err := m.Prepare(context.Background(), []string{testRef})
		if err != nil {
			t.Fatalf("Prepare: %v", err)
		}
		if len(dirs) != 1 {
			t.Fatalf("dirs %v", dirs)
		}
		spec, _ := Parse(testRef)
		if got := fetchedAt(m.repoDir(spec)); !got.Equal(start) {
			t.Fatalf("a failed pull moved the stamp to %v", got)
		}
	})
}

func TestInfoAndUpdate(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	now := start
	m, rec := testManager(t, &now)
	spec, err := Parse(testRef)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	info, err := m.Info(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if info.Cached {
		t.Fatalf("nothing is cached yet: %+v", info)
	}

	rec.commits = []string{"aaaaaaa"}
	before, after, err := m.Update(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if before != "" || after != "aaaaaaa" {
		t.Fatalf("clone reported %q → %q", before, after)
	}
	if rec.count("clone") != 1 || rec.count("pull") != 0 {
		t.Fatalf("calls %v", rec.calls)
	}

	now = start.Add(time.Hour)
	rec.commits = []string{"aaaaaaa", "bbbbbbb"}
	rec.revIndex = 0
	before, after, err = m.Update(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if before != "aaaaaaa" || after != "bbbbbbb" {
		t.Fatalf("update reported %q → %q", before, after)
	}
	if rec.count("pull") != 1 {
		t.Fatalf("calls %v", rec.calls)
	}

	info, err = m.Info(ctx, spec)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Cached || info.Commit != "bbbbbbb" || !info.FetchedAt.Equal(now) {
		t.Fatalf("info %+v", info)
	}

	rec.pullErr = errors.New("no")
	if _, _, err := m.Update(ctx, spec); err == nil {
		t.Fatal("expected a failed update to report the error")
	}
}

func prepare(t *testing.T, m *Manager) {
	t.Helper()
	if _, err := m.Prepare(context.Background(), []string{testRef}); err != nil {
		t.Fatal(err)
	}
}

// TestConcurrentPrepare guards the case the scheduler creates: several
// sessions starting at once share one manager and one skill reference.
func TestConcurrentPrepare(t *testing.T) {
	up := filepath.Join(t.TempDir(), "up")
	if err := os.MkdirAll(up, 0o755); err != nil {
		t.Fatal(err)
	}
	sh := func(args ...string) {
		t.Helper()
		c := exec.Command(args[0], args[1:]...)
		c.Dir = up
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %v: %s", args, err, out)
		}
	}
	sh("git", "init", "-q", "-b", "main", ".")
	mk(t, filepath.Join(up, "SKILL.md"), "---\nname: tdd\ndescription: d\n---\none\n")
	sh("git", "add", "-A")
	sh("git", "-c", "user.email=a@b", "-c", "user.name=a", "commit", "-qm", "one")

	// race runs n concurrent Prepare calls and returns the directory they
	// must all agree on.
	race := func(t *testing.T, m *Manager) string {
		t.Helper()
		const n = 8
		var wg sync.WaitGroup
		dirs := make([][]string, n)
		errs := make([]error, n)
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				dirs[i], errs[i] = m.Prepare(context.Background(), []string{up})
			}(i)
		}
		wg.Wait()
		want := ""
		for i, err := range errs {
			if err != nil {
				t.Errorf("goroutine %d: %v", i, err)
				continue
			}
			if len(dirs[i]) != 1 {
				t.Errorf("goroutine %d returned %v", i, dirs[i])
				continue
			}
			if want == "" {
				want = dirs[i][0]
			} else if dirs[i][0] != want {
				t.Errorf("goroutine %d returned %s, want %s", i, dirs[i][0], want)
			}
		}
		return want
	}

	// Cold cache: the clone and the wrapper are built while the others wait.
	t.Run("cold cache", func(t *testing.T) {
		dir := race(t, NewManager(t.TempDir()))
		if dir == "" {
			t.Fatal("no goroutine succeeded")
		}
		mustExist(t, filepath.Join(dir, "skills", filepath.Base(dir), "SKILL.md"))
	})

	// Warm cache: the reproduction from the report — every session finds the
	// wrapper already built and must leave it alone.
	t.Run("warm cache", func(t *testing.T) {
		m := NewManager(t.TempDir())
		first, err := m.Prepare(context.Background(), []string{up})
		if err != nil {
			t.Fatal(err)
		}
		if dir := race(t, m); dir != first[0] {
			t.Fatalf("plugin dir moved from %s to %s", first[0], dir)
		}
		mustExist(t, filepath.Join(first[0], "skills", filepath.Base(first[0]), "SKILL.md"))
	})
}

// TestPrepareKeepsUnchangedWrapper: a running session holds --plugin-dir on
// the wrapper, so an unchanged reference must not remove and rebuild it.
func TestPrepareKeepsUnchangedWrapper(t *testing.T) {
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	m, _ := testManager(t, &now)
	dirs, err := m.Prepare(context.Background(), []string{testRef})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(dirs[0], "marker")
	mk(t, marker, "in use")

	again, err := m.Prepare(context.Background(), []string{testRef})
	if err != nil {
		t.Fatal(err)
	}
	if again[0] != dirs[0] {
		t.Fatalf("plugin dir moved from %s to %s", dirs[0], again[0])
	}
	mustExist(t, marker)
}

// TestPrepareRebuildsChangedWrapper: two references share a name but resolve
// to different sub-directories, so the second one must relink.
func TestPrepareRebuildsChangedWrapper(t *testing.T) {
	fixture := filepath.Join(t.TempDir(), "coll")
	mk(t, filepath.Join(fixture, "x", "tdd", "SKILL.md"), "---\nname: tdd\n---\nx\n")
	mk(t, filepath.Join(fixture, "y", "tdd", "SKILL.md"), "---\nname: tdd\n---\ny\n")
	mk(t, filepath.Join(fixture, ".git", "HEAD"), "")

	m := NewManager(t.TempDir())
	m.Git = fakeGit(fixture)
	ctx := context.Background()

	first, err := m.Prepare(ctx, []string{"https://github.com/x/coll#x/tdd"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := m.Prepare(ctx, []string{"https://github.com/x/coll#y/tdd"})
	if err != nil {
		t.Fatal(err)
	}
	if second[0] != first[0] {
		t.Fatalf("expected the same wrapper directory, got %s and %s", first[0], second[0])
	}
	link := filepath.Join(second[0], "skills", "coll-tdd")
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(filepath.Dir(got)) != "y" {
		t.Fatalf("%s points at %s, want the y/tdd sub-directory", link, got)
	}
}
