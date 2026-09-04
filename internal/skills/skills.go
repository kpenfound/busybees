// Package skills installs skills referenced by git URL in bees.toml.
//
// Each URL is cloned into a cache directory and exposed to claude as a
// plugin directory (claude --plugin-dir), which keeps the project worktree
// untouched. Three repository layouts are supported:
//
//   - a Claude Code plugin (has .claude-plugin/plugin.json): used as-is
//   - a single skill (has SKILL.md at the root or selected sub-directory):
//     wrapped in a generated plugin exposing that one skill
//   - a skills collection (has a skills/ directory): wrapped in a generated
//     plugin exposing every skill in it
//
// URL syntax: <git-url>[@<ref>][#<sub/dir>], for example
//
//	https://github.com/acme/skills#skills/tdd
//	https://github.com/acme/my-plugin@v1.2.0
package skills

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Spec is a parsed skill reference.
type Spec struct {
	Raw    string
	URL    string
	Ref    string
	Subdir string
	Name   string
}

// Parse parses a skill reference.
func Parse(raw string) (Spec, error) {
	s := Spec{Raw: strings.TrimSpace(raw)}
	if s.Raw == "" {
		return s, fmt.Errorf("skills: empty reference")
	}
	rest := s.Raw
	if i := strings.Index(rest, "#"); i >= 0 {
		s.Subdir = strings.Trim(rest[i+1:], "/")
		rest = rest[:i]
	}
	// A trailing "@ref" is only a ref when it comes after the last path
	// separator, so scp-style git@host:org/repo URLs still work.
	if i := strings.LastIndex(rest, "@"); i > strings.LastIndexAny(rest, "/:") {
		s.Ref = rest[i+1:]
		rest = rest[:i]
	}
	s.URL = rest
	if s.URL == "" {
		return s, fmt.Errorf("skills: %q has no git url", raw)
	}
	base := s.URL
	base = strings.TrimSuffix(base, "/")
	base = strings.TrimSuffix(base, ".git")
	if i := strings.LastIndexAny(base, "/:"); i >= 0 {
		base = base[i+1:]
	}
	name := base
	if s.Subdir != "" {
		name += "-" + filepath.Base(s.Subdir)
	}
	s.Name = sanitizeName(name)
	if s.Name == "" {
		return s, fmt.Errorf("skills: cannot derive a name from %q", raw)
	}
	return s, nil
}

// Manager clones skills and produces plugin directories.
type Manager struct {
	// CacheDir holds clones (CacheDir/repos) and generated plugins (CacheDir/plugins).
	CacheDir string
	// RefreshAfter pulls an existing clone that was last fetched at least
	// this long ago. Zero never refreshes by age.
	RefreshAfter time.Duration
	// RefreshAlways pulls every existing clone before use.
	RefreshAlways bool
	// Git overrides git execution (tests). It returns the command's output.
	Git func(ctx context.Context, dir string, args ...string) (string, error)
	// Now overrides the clock (tests).
	Now func() time.Time
	// Logger receives refresh warnings. nil uses slog.Default().
	Logger *slog.Logger

	// mu serialises prepareOne. Sessions start concurrently and share one
	// manager, so without it two sessions clone into and build the same
	// wrapper directory at the same time.
	mu sync.Mutex
}

// Info describes one skill reference in the cache.
type Info struct {
	Spec      Spec
	Dir       string
	Cached    bool
	Commit    string
	FetchedAt time.Time
}

// NewManager returns a manager caching under dir.
func NewManager(dir string) *Manager {
	m := &Manager{CacheDir: dir}
	m.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		cmd := exec.CommandContext(ctx, "git", args...)
		if dir != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return string(out), nil
	}
	return m
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

func (m *Manager) logger() *slog.Logger {
	if m.Logger != nil {
		return m.Logger
	}
	return slog.Default()
}

// CacheDir is where clones and generated plugins live: $BEES_CACHE_DIR when
// it is set, DefaultCacheDir otherwise. It is the one place the environment
// variable is read, so a session, `bees skills` and `bees doctor` all warm the
// same cache.
func CacheDir() string {
	if d := os.Getenv("BEES_CACHE_DIR"); d != "" {
		return d
	}
	return DefaultCacheDir()
}

// DefaultCacheDir returns the user cache directory: ~/.cache/bees on Linux,
// ~/Library/Caches/bees on macOS.
func DefaultCacheDir() string {
	if dir, err := os.UserCacheDir(); err == nil {
		return filepath.Join(dir, "bees")
	}
	return filepath.Join(os.TempDir(), "bees-cache")
}

// Prepare ensures every reference is cloned and returns plugin directories
// to pass to claude, in the same order.
func (m *Manager) Prepare(ctx context.Context, refs []string) ([]string, error) {
	var dirs []string
	for _, raw := range refs {
		spec, err := Parse(raw)
		if err != nil {
			return nil, err
		}
		dir, err := m.prepareOne(ctx, spec)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", raw, err)
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func (m *Manager) prepareOne(ctx context.Context, spec Spec) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	repoDir, err := m.clone(ctx, spec)
	if err != nil {
		return "", err
	}
	target := repoDir
	if spec.Subdir != "" {
		target = filepath.Join(repoDir, filepath.FromSlash(spec.Subdir))
		if st, err := os.Stat(target); err != nil || !st.IsDir() {
			return "", fmt.Errorf("sub-directory %q not found in repository", spec.Subdir)
		}
	}
	return m.pluginDirFor(spec, target)
}

// repoDir is the cache directory a reference is cloned into.
func (m *Manager) repoDir(spec Spec) string {
	sum := sha256.Sum256([]byte(spec.URL + "@" + spec.Ref))
	return filepath.Join(m.CacheDir, "repos", spec.Name+"-"+hex.EncodeToString(sum[:4]))
}

// stamp is the sibling file whose mtime is when the clone was last fetched.
func stamp(dir string) string { return dir + ".fetched" }

// fetchedAt returns the mtime of the stamp file, or the zero time.
func fetchedAt(dir string) time.Time {
	st, err := os.Stat(stamp(dir))
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// touch records now as the fetch time of dir.
func (m *Manager) touch(dir string) {
	now := m.now()
	p := stamp(dir)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		return
	}
	_ = os.Chtimes(p, now, now)
}

// stale reports whether an existing clone should be pulled before use.
func (m *Manager) stale(dir string) bool {
	if m.RefreshAlways {
		return true
	}
	if m.RefreshAfter <= 0 {
		return false
	}
	last := fetchedAt(dir)
	if last.IsZero() { // cloned before stamps existed, or the file was removed
		return true
	}
	return m.now().Sub(last) >= m.RefreshAfter
}

func (m *Manager) clone(ctx context.Context, spec Spec) (string, error) {
	dir := m.repoDir(spec)
	if cloned(dir) {
		if m.stale(dir) {
			// Best effort: a failed refresh (a pinned tag is detached and
			// cannot pull) must not block the factory.
			if _, err := m.pull(ctx, dir); err != nil {
				m.logger().Warn("skill refresh failed", "skill", spec.Raw, "url", spec.URL, "error", err)
			}
		}
		return dir, nil
	}
	if err := m.cloneFresh(ctx, spec, dir); err != nil {
		return "", err
	}
	return dir, nil
}

func (m *Manager) cloneFresh(ctx context.Context, spec Spec, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	args := []string{"clone", "--depth", "1", "--quiet"}
	if spec.Ref != "" {
		args = append(args, "--branch", spec.Ref)
	}
	args = append(args, spec.URL, dir)
	if _, err := m.Git(ctx, "", args...); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	m.touch(dir)
	return nil
}

// pull fast-forwards an existing clone and records the fetch time.
func (m *Manager) pull(ctx context.Context, dir string) (string, error) {
	out, err := m.Git(ctx, dir, "pull", "--ff-only", "--quiet")
	if err != nil {
		return out, err
	}
	m.touch(dir)
	return out, nil
}

// head returns the short commit of a clone ("" when it cannot be read).
func (m *Manager) head(ctx context.Context, dir string) string {
	out, err := m.Git(ctx, dir, "rev-parse", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// Info reports what the cache holds for a reference, without touching it.
func (m *Manager) Info(ctx context.Context, spec Spec) (Info, error) {
	info := Info{Spec: spec, Dir: m.repoDir(spec)}
	if !cloned(info.Dir) {
		return info, nil
	}
	info.Cached = true
	info.Commit = m.head(ctx, info.Dir)
	info.FetchedAt = fetchedAt(info.Dir)
	return info, nil
}

// Update clones a reference when it is missing and pulls it otherwise,
// ignoring the refresh policy. It returns the short commits before and
// after; before is empty for a fresh clone.
func (m *Manager) Update(ctx context.Context, spec Spec) (before, after string, err error) {
	dir := m.repoDir(spec)
	if !cloned(dir) {
		if err := m.cloneFresh(ctx, spec, dir); err != nil {
			return "", "", err
		}
		return "", m.head(ctx, dir), nil
	}
	before = m.head(ctx, dir)
	if _, err := m.pull(ctx, dir); err != nil {
		return before, "", err
	}
	return before, m.head(ctx, dir), nil
}

func cloned(dir string) bool { return exists(filepath.Join(dir, ".git")) }

// pluginDirFor returns a plugin directory exposing the skill(s) in target.
func (m *Manager) pluginDirFor(spec Spec, target string) (string, error) {
	if exists(filepath.Join(target, ".claude-plugin", "plugin.json")) {
		return target, nil
	}
	pluginDir := filepath.Join(m.CacheDir, "plugins", spec.Name)
	skillsDir := filepath.Join(pluginDir, "skills")

	// Where the wrapper's symlink lives and what it points at, per layout.
	var link, dest string
	switch {
	case exists(filepath.Join(target, "SKILL.md")):
		link, dest = filepath.Join(skillsDir, spec.Name), target
	case isDir(filepath.Join(target, "skills")):
		link, dest = skillsDir, filepath.Join(target, "skills")
	default:
		return "", fmt.Errorf("%s is not a plugin (.claude-plugin/plugin.json), a skill (SKILL.md) or a skills collection (skills/)", target)
	}

	// A wrapper that already points at this target is used as it is: sessions
	// run for a long time with --plugin-dir pointing here, and removing the
	// directory would pull it out from under them. Only a missing manifest or
	// a link pointing elsewhere (the reference's sub-directory or ref changed)
	// forces a rebuild.
	if exists(filepath.Join(pluginDir, ".claude-plugin", "plugin.json")) {
		if got, err := os.Readlink(link); err == nil && got == dest {
			return pluginDir, nil
		}
	}

	if err := os.RemoveAll(pluginDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(pluginDir, ".claude-plugin"), 0o755); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(link), 0o755); err != nil {
		return "", err
	}
	if err := os.Symlink(dest, link); err != nil {
		return "", err
	}
	manifest := map[string]string{
		"name":        spec.Name,
		"description": "busybees skill from " + spec.Raw,
		"version":     "0.0.0",
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(pluginDir, ".claude-plugin", "plugin.json"), data, 0o644); err != nil {
		return "", err
	}
	return pluginDir, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func sanitizeName(s string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteRune('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
