package skills

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kpenfound/busybees/core/agent"
)

// Manager implements agent.SkillPreparer: it clones skill references on
// demand, refreshes them under a caller-controlled policy, and hands their
// resolved target directory to the layout step that builds (or reuses) the
// plugin directory claude is pointed at.
//
// Manager caches exclusively under the directory passed to NewManager; it
// reads no environment variable to choose where that is.
type Manager struct {
	cacheDir string
	logger   *slog.Logger

	// Git runs a git subcommand in dir (or outside any repository when dir
	// is empty) and returns its combined output. Tests replace it with a
	// fake that copies a fixture directory instead of cloning over the
	// network.
	Git func(ctx context.Context, dir string, args ...string) (string, error)
	// Now overrides the clock (tests).
	Now func() time.Time

	// SkillsOnly restricts the layout step to the SKILL.md and skills/
	// shapes: Prepare never returns a repository's own directory, even when
	// it is itself a Claude Code plugin, and always returns a generated
	// wrapper holding only skills/ and the manifest the wrapper needs. No
	// hooks, MCP servers, agents or commands from the repository come
	// through, even when it has them. Set it before the manager is used; it
	// is not safe to change concurrently with Prepare the way the refresh
	// policy is.
	SkillsOnly bool

	// mu serialises Prepare: sessions can start concurrently and share one
	// manager, and without it two of them could clone into, or build the
	// wrapper plugin for, the same directory at the same time.
	mu sync.Mutex

	// policy is read and written independently of mu, so SetRefresh never
	// waits on a Prepare call that is busy cloning or pulling.
	policy atomic.Pointer[RefreshPolicy]
}

var _ agent.SkillPreparer = (*Manager)(nil)

// NewManager returns a Manager that caches clones and generated wrapper
// plugins under cacheDir. A nil logger uses slog.Default(). The refresh
// policy starts at RefreshNever; call SetRefresh to change it.
func NewManager(cacheDir string, logger *slog.Logger) *Manager {
	if logger == nil {
		logger = slog.Default()
	}
	m := &Manager{
		cacheDir: cacheDir,
		logger:   logger,
		Git:      defaultGit,
	}
	m.SetRefresh(RefreshNever)
	return m
}

// SetRefresh changes the refresh policy a running Manager uses. It is safe
// to call concurrently with Prepare; a Prepare call that starts after
// SetRefresh returns uses the new policy.
func (m *Manager) SetRefresh(p RefreshPolicy) {
	policy := p
	m.policy.Store(&policy)
}

func (m *Manager) refreshPolicy() RefreshPolicy {
	if p := m.policy.Load(); p != nil {
		return *p
	}
	return RefreshNever
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Prepare implements agent.SkillPreparer: it ensures every reference is
// cloned (and refreshed, under the current policy) and returns their
// plugin directories in the same order, ready to pass to claude via
// --plugin-dir.
func (m *Manager) Prepare(ctx context.Context, refs []string) ([]string, error) {
	var dirs []string
	for _, raw := range refs {
		ref, err := Parse(raw)
		if err != nil {
			return nil, err
		}
		dir, err := m.prepareOne(ctx, raw, ref)
		if err != nil {
			return nil, fmt.Errorf("skill %s: %w", raw, err)
		}
		dirs = append(dirs, dir)
	}
	return dirs, nil
}

func (m *Manager) prepareOne(ctx context.Context, raw string, ref Ref) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	dir := m.cloneDir(ref)
	if err := m.clone(ctx, ref, dir); err != nil {
		return "", err
	}
	target := dir
	if ref.Subdir != "" {
		target = filepath.Join(dir, filepath.FromSlash(ref.Subdir))
		if st, err := os.Stat(target); err != nil || !st.IsDir() {
			return "", fmt.Errorf("sub-directory %q not found in repository", ref.Subdir)
		}
	}
	return m.pluginDirFor(ref, raw, dir, target)
}

// cloneDir is the cache directory a reference is cloned into: a readable
// name derived from its URL, followed by a short hash of its full
// reference string (Ref.String()), so two references that derive the same
// name from different repositories (for example acme/skills and
// other/skills) never share a clone directory.
func (m *Manager) cloneDir(ref Ref) string {
	return filepath.Join(m.cacheDir, "repos", cloneDirName(ref))
}

func cloneDirName(ref Ref) string {
	return keyedName(sanitizeName(baseName(ref.URL)), ref)
}

// stamp is the sibling file whose mtime is when dir was last refreshed.
func stamp(dir string) string { return dir + ".fetched" }

// fetchedAt returns the mtime of dir's stamp file, or the zero time.
func fetchedAt(dir string) time.Time {
	st, err := os.Stat(stamp(dir))
	if err != nil {
		return time.Time{}
	}
	return st.ModTime()
}

// touch records now as the refresh time of dir.
func (m *Manager) touch(dir string) {
	now := m.now()
	p := stamp(dir)
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		return
	}
	_ = os.Chtimes(p, now, now)
}

// stale reports whether an existing clone should be refreshed before use,
// under the policy in force.
func (m *Manager) stale(dir string, policy RefreshPolicy) bool {
	switch policy.kind {
	case refreshAlways:
		return true
	case refreshEveryDuration:
		last := fetchedAt(dir)
		if last.IsZero() { // cloned before stamps existed, or the stamp was removed
			return true
		}
		return m.now().Sub(last) >= policy.every
	default: // refreshNever
		return false
	}
}

// clone ensures dir holds a clone of ref, cloning it fresh when it does not
// exist yet (whatever the refresh policy) and otherwise refreshing it only
// when stale() says so.
func (m *Manager) clone(ctx context.Context, ref Ref, dir string) error {
	if cloned(dir) {
		if m.stale(dir, m.refreshPolicy()) {
			// Best effort: a failed refresh (a pinned tag is detached and
			// cannot pull, a remote is briefly unreachable) must not block
			// Prepare when a usable clone already exists.
			if _, err := m.pull(ctx, dir); err != nil {
				m.logger.Warn("skill refresh failed", "ref", ref.String(), "url", ref.URL, "error", err)
			}
		}
		return nil
	}
	return m.cloneFresh(ctx, ref, dir)
}

func (m *Manager) cloneFresh(ctx context.Context, ref Ref, dir string) error {
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return err
	}
	args := []string{"clone", "--depth", "1", "--quiet"}
	if ref.Ref != "" {
		args = append(args, "--branch", ref.Ref)
	}
	args = append(args, ref.URL, dir)
	if _, err := m.Git(ctx, "", args...); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	m.touch(dir)
	return nil
}

// pull fast-forwards an existing clone and records the refresh time.
func (m *Manager) pull(ctx context.Context, dir string) (string, error) {
	out, err := m.Git(ctx, dir, "pull", "--ff-only", "--quiet")
	if err != nil {
		return out, err
	}
	m.touch(dir)
	return out, nil
}

func cloned(dir string) bool { return exists(filepath.Join(dir, ".git")) }

// defaultGit runs git as a subprocess; it is the Manager.Git every
// NewManager starts with, replaced in tests with a fake that copies a
// fixture directory instead of reaching a real remote.
func defaultGit(ctx context.Context, dir string, args ...string) (string, error) {
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
