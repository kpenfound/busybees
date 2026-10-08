package skills

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGit "clones" a reference by copying a local fixture directory and
// records every invocation it sees. It never touches a real git remote.
type fakeGit struct {
	fixture string

	mu      sync.Mutex
	calls   [][]string
	pullErr error
}

func (f *fakeGit) run(_ context.Context, _ string, args ...string) (string, error) {
	f.mu.Lock()
	f.calls = append(f.calls, append([]string{}, args...))
	f.mu.Unlock()
	switch args[0] {
	case "clone":
		return "", os.CopyFS(args[len(args)-1], os.DirFS(f.fixture))
	case "pull":
		return "", f.pullErr
	case "rev-parse":
		return "abc1234\n", nil
	}
	return "", nil
}

func (f *fakeGit) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c[0] == name {
			n++
		}
	}
	return n
}

// skillFixture is a minimal single-skill repository.
func skillFixture(t *testing.T) string {
	t.Helper()
	return skillFixtureNamed(t, "fix")
}

func skillFixtureNamed(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	mk(t, filepath.Join(dir, "SKILL.md"), "---\nname: "+name+"\n---\n")
	mk(t, filepath.Join(dir, ".git", "HEAD"), "")
	return dir
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

// testManager returns a manager with a fake git over fixture, discarding
// its logger output by default.
func testManager(t *testing.T, fixture string) (*Manager, *fakeGit) {
	t.Helper()
	g := &fakeGit{fixture: fixture}
	m := NewManager(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.Git = g.run
	return m, g
}

func prepare(t *testing.T, m *Manager, ref string) string {
	t.Helper()
	dirs, err := m.Prepare(context.Background(), []string{ref})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 {
		t.Fatalf("dirs: %v", dirs)
	}
	return dirs[0]
}

const testRef = "https://github.com/x/fix"

func TestClonesOnlyUnderCacheRoot(t *testing.T) {
	cacheDir := t.TempDir()
	g := &fakeGit{fixture: skillFixture(t)}
	m := NewManager(cacheDir, nil)
	m.Git = g.run

	dir := prepare(t, m, testRef)
	if !strings.HasPrefix(dir, cacheDir) {
		t.Fatalf("plugin dir %s is not under cache root %s", dir, cacheDir)
	}

	found := false
	if err := filepath.WalkDir(cacheDir, func(_ string, de os.DirEntry, err error) error {
		if err == nil && de.Name() == ".git" {
			found = true
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("no clone (.git) found under the cache root")
	}
}

func TestDistinctClonesForSameDerivedName(t *testing.T) {
	m, g := testManager(t, skillFixture(t))

	acme, err := Parse("https://github.com/acme/skills")
	if err != nil {
		t.Fatal(err)
	}
	other, err := Parse("https://github.com/other/skills")
	if err != nil {
		t.Fatal(err)
	}
	// If acme/skills and other/skills shared a clone directory, the second
	// Prepare would find the first one's .git already there and skip its
	// own clone (Manager.clone), so distinct directories show up here as
	// two clones, not one.
	if _, err := m.Prepare(context.Background(), []string{acme.String(), other.String()}); err != nil {
		t.Fatal(err)
	}
	if g.count("clone") != 2 {
		t.Fatalf("expected 2 clones (one per distinct clone directory), got %d: %v", g.count("clone"), g.calls)
	}
}

func TestParseRefresh(t *testing.T) {
	if p, err := ParseRefresh("never"); err != nil || p != RefreshNever {
		t.Fatalf("never: got %+v, %v", p, err)
	}
	if p, err := ParseRefresh("always"); err != nil || p != RefreshAlways {
		t.Fatalf("always: got %+v, %v", p, err)
	}
	p, err := ParseRefresh("1h")
	if err != nil {
		t.Fatal(err)
	}
	if p != RefreshEvery(time.Hour) {
		t.Fatalf("1h: got %+v", p)
	}
}

func TestParseRefreshRejects(t *testing.T) {
	for _, s := range []string{"", "sometimes", "-x"} {
		if _, err := ParseRefresh(s); err == nil {
			t.Fatalf("ParseRefresh(%q): expected an error, got none", s)
		}
	}
}

func TestRefreshPolicyDecisions(t *testing.T) {
	start := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

	t.Run("never does not refresh an existing clone", func(t *testing.T) {
		now := start
		m, g := testManager(t, skillFixture(t))
		m.Now = func() time.Time { return now }
		m.SetRefresh(RefreshNever)
		prepare(t, m, testRef)
		now = start.Add(30 * 24 * time.Hour)
		prepare(t, m, testRef)
		if g.count("pull") != 0 {
			t.Fatalf("calls: %v", g.calls)
		}
	})

	t.Run("always refreshes every time", func(t *testing.T) {
		now := start
		m, g := testManager(t, skillFixture(t))
		m.Now = func() time.Time { return now }
		m.SetRefresh(RefreshAlways)
		prepare(t, m, testRef)
		prepare(t, m, testRef)
		prepare(t, m, testRef)
		if g.count("pull") != 2 {
			t.Fatalf("calls: %v", g.calls)
		}
	})

	t.Run("a duration refreshes only once stale", func(t *testing.T) {
		now := start
		m, g := testManager(t, skillFixture(t))
		m.Now = func() time.Time { return now }
		m.SetRefresh(RefreshEvery(time.Hour))
		prepare(t, m, testRef)
		now = start.Add(30 * time.Minute)
		prepare(t, m, testRef)
		if g.count("pull") != 0 {
			t.Fatalf("pulled before the duration elapsed: %v", g.calls)
		}
		now = start.Add(time.Hour)
		prepare(t, m, testRef)
		if g.count("pull") != 1 {
			t.Fatalf("calls once the duration elapsed: %v", g.calls)
		}
	})

	for _, name := range []string{"never", "always", "1h"} {
		t.Run("a missing clone is cloned under "+name, func(t *testing.T) {
			m, g := testManager(t, skillFixture(t))
			policy, err := ParseRefresh(name)
			if err != nil {
				t.Fatal(err)
			}
			m.SetRefresh(policy)
			prepare(t, m, testRef)
			if g.count("clone") != 1 {
				t.Fatalf("calls: %v", g.calls)
			}
		})
	}
}

func TestSetRefreshChangesPolicyBetweenPrepares(t *testing.T) {
	m, g := testManager(t, skillFixture(t))
	m.SetRefresh(RefreshNever)
	prepare(t, m, testRef)
	prepare(t, m, testRef)
	if g.count("pull") != 0 {
		t.Fatalf("never: calls %v", g.calls)
	}

	m.SetRefresh(RefreshAlways)
	prepare(t, m, testRef)
	if g.count("pull") != 1 {
		t.Fatalf("always: calls %v", g.calls)
	}
}

// TestSetRefreshConcurrentWithPrepare guards the config-reload case: a
// caller may flip the policy while sessions are starting. It must pass
// under the race detector.
func TestSetRefreshConcurrentWithPrepare(t *testing.T) {
	m, _ := testManager(t, skillFixture(t))
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			m.SetRefresh(RefreshAlways)
		}()
		go func() {
			defer wg.Done()
			if _, err := m.Prepare(context.Background(), []string{testRef}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

func TestRefreshFailureIsLoggedAndPrepareSucceeds(t *testing.T) {
	var buf bytes.Buffer
	g := &fakeGit{fixture: skillFixture(t)}
	m := NewManager(t.TempDir(), slog.New(slog.NewTextHandler(&buf, nil)))
	m.Git = g.run
	m.SetRefresh(RefreshAlways)

	prepare(t, m, testRef)
	g.pullErr = errors.New("cannot fast-forward")
	dir := prepare(t, m, testRef)
	mustExist(t, filepath.Join(dir, "skills", "fix", "SKILL.md"))
	if !strings.Contains(buf.String(), "skill refresh failed") {
		t.Fatalf("expected a log record about the failed refresh, got %q", buf.String())
	}
}

// TestParallelPrepareDistinctReferences prepares several distinct
// references concurrently on one manager; each must return its own
// correct plugin directory. It must pass under the race detector.
func TestParallelPrepareDistinctReferences(t *testing.T) {
	names := []string{"one", "two", "three", "four"}
	fixtures := make(map[string]string, len(names))
	refs := make([]string, len(names))
	for i, name := range names {
		fixtures[name] = skillFixtureNamed(t, name)
		refs[i] = "https://github.com/x/" + name
	}

	m := NewManager(t.TempDir(), nil)
	m.Git = func(_ context.Context, _ string, args ...string) (string, error) {
		if args[0] != "clone" {
			return "", nil
		}
		url := args[len(args)-2]
		name := url[strings.LastIndex(url, "/")+1:]
		return "", os.CopyFS(args[len(args)-1], os.DirFS(fixtures[name]))
	}

	var wg sync.WaitGroup
	results := make([]string, len(refs))
	errs := make([]error, len(refs))
	for i, ref := range refs {
		wg.Add(1)
		go func(i int, ref string) {
			defer wg.Done()
			dirs, err := m.Prepare(context.Background(), []string{ref})
			if err != nil {
				errs[i] = err
				return
			}
			results[i] = dirs[0]
		}(i, ref)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("%s: %v", refs[i], err)
		}
	}
	for i, name := range names {
		if results[i] == "" {
			continue
		}
		mustExist(t, filepath.Join(results[i], "skills", name, "SKILL.md"))
	}
}
