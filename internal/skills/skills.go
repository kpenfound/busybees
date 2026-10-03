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
//
// Manager is a thin adapter over core/skills.Manager, which does the actual
// parsing, cloning, refreshing and wrapper building: this package's own job
// is to read $BEES_CACHE_DIR (CacheDir) and map bees' configuration (the
// RefreshAfter/RefreshAlways fields cmd/bees and internal/doctor set
// directly) onto core/skills' refresh policy, plus the small bookkeeping
// `bees skills list` and `bees skills update` need (Info, Update) that
// core/skills, built only for agent.SkillPreparer, has no use for itself.
package skills

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	coreskills "github.com/kpenfound/busybees/core/skills"
)

// Spec is a parsed skill reference: core/skills' Ref, under the name bees
// callers already use.
type Spec = coreskills.Ref

// Parse parses a skill reference. See core/skills.Parse.
func Parse(raw string) (Spec, error) { return coreskills.Parse(raw) }

// Manager clones skills and produces plugin directories, by delegating to a
// core/skills.Manager built lazily from the fields below. cmd/bees and
// internal/doctor assign CacheDir, RefreshAfter, RefreshAlways, Git and
// Logger directly (there is no constructor call after NewManager that would
// give them another way to), so Manager keeps reading them itself rather
// than taking a one-shot configuration.
type Manager struct {
	// CacheDir holds clones and generated plugins, under core/skills'
	// layout (repos/ and plugins/, keyed by the full reference).
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
	// Logger receives refresh warnings. nil uses slog.Default(). Read once,
	// when the underlying core/skills.Manager is built.
	Logger *slog.Logger

	// mu serialises every call: the lazy build of core below, the fields
	// copied onto it (Git, Now, the refresh policy) and the index file
	// Prepare and Update maintain for Info all have to agree with one
	// another, the same guarantee the original Manager gave prepareOne.
	mu   sync.Mutex
	core *coreskills.Manager
}

// Info describes one skill reference in the cache, as Prepare or Update last
// found it.
type Info struct {
	Spec      Spec
	Dir       string
	Cached    bool
	Commit    string
	FetchedAt time.Time
}

// NewManager returns a manager caching under dir. Git starts out as
// core/skills' own default (a throwaway core/skills.Manager is built just to
// read it off), so a caller that captures Git right away to wrap it (as
// internal/doctor's tests do) finds a real implementation there, not nil,
// without this package holding its own copy of it.
func NewManager(dir string) *Manager {
	return &Manager{CacheDir: dir, Git: coreskills.NewManager(dir, nil).Git}
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

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// ensureCore builds the core/skills.Manager this adapter delegates to, the
// first time it is needed, and keeps the fields a caller can assign at any
// time (Git, Now, the refresh policy) in sync with it on every call. Logger
// and CacheDir are only read at the first call, because core/skills.Manager
// takes them once, in NewManager, and has no setter for either.
func (m *Manager) ensureCore() *coreskills.Manager {
	if m.core == nil {
		m.core = coreskills.NewManager(m.CacheDir, m.Logger)
	}
	if m.Git != nil {
		m.core.Git = m.Git
	}
	if m.Now != nil {
		m.core.Now = m.Now
	}
	m.core.SetRefresh(m.refreshPolicy())
	return m.core
}

// refreshPolicy maps RefreshAfter/RefreshAlways, the fields bees' config
// sets directly, onto a core/skills.RefreshPolicy through ParseRefresh, the
// same way a "never"/"always"/duration config string would.
func (m *Manager) refreshPolicy() coreskills.RefreshPolicy {
	s := "never"
	switch {
	case m.RefreshAlways:
		s = "always"
	case m.RefreshAfter > 0:
		s = m.RefreshAfter.String()
	}
	policy, err := coreskills.ParseRefresh(s)
	if err != nil {
		// Unreachable: s is always "never", "always" or a duration string
		// produced by time.Duration.String(), which ParseRefresh accepts.
		return coreskills.RefreshNever
	}
	return policy
}

// Prepare ensures every reference is cloned and returns plugin directories
// to pass to claude, in the same order.
func (m *Manager) Prepare(ctx context.Context, refs []string) ([]string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	core := m.ensureCore()
	dirs, err := core.Prepare(ctx, refs)
	if err != nil {
		return nil, err
	}
	for i, raw := range refs {
		if spec, err := Parse(raw); err == nil {
			m.recordFetch(ctx, core, spec, dirs[i])
		}
	}
	return dirs, nil
}

// Info reports what the cache holds for a reference, without touching it:
// it reads the index Prepare and Update maintain rather than the clone
// itself, which core/skills keeps no public way to inspect from outside a
// Prepare call.
func (m *Manager) Info(ctx context.Context, spec Spec) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	entry, ok := m.readIndex()[spec.String()]
	if !ok {
		return Info{Spec: spec}, nil
	}
	return Info{Spec: spec, Dir: entry.Dir, Cached: true, Commit: entry.Commit, FetchedAt: entry.FetchedAt}, nil
}

// Update clones a reference when it is missing and pulls it otherwise,
// ignoring the refresh policy. It returns the short commits before and
// after; before is empty for a fresh clone.
func (m *Manager) Update(ctx context.Context, spec Spec) (before, after string, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	core := m.ensureCore()
	restore := m.refreshPolicy()
	core.SetRefresh(coreskills.RefreshAlways)
	defer core.SetRefresh(restore)

	if entry, ok := m.readIndex()[spec.String()]; ok {
		before = entry.Commit
	}
	dirs, err := core.Prepare(ctx, []string{spec.String()})
	if err != nil {
		return before, "", err
	}
	after = commitAt(ctx, core.Git, dirs[0])
	m.writeIndexEntry(spec.String(), indexEntry{Dir: dirs[0], Commit: after, FetchedAt: m.now()})
	return before, after, nil
}

// recordFetch updates the index entry for spec after core has prepared it,
// so a later Info call can answer without reaching for the clone.
func (m *Manager) recordFetch(ctx context.Context, core *coreskills.Manager, spec Spec, dir string) {
	commit := commitAt(ctx, core.Git, dir)
	m.writeIndexEntry(spec.String(), indexEntry{Dir: dir, Commit: commit, FetchedAt: m.now()})
}

// commitAt returns the short commit of the clone backing a prepared plugin
// directory dir: dir itself when it is a full plugin used as-is (already
// inside the clone), or, for a generated wrapper, a path reached through its
// skills/ directory, which core/skills documents as holding only the
// skill(s) found in the clone (a real directory for a single skill's
// symlink, or itself a symlink to the clone's skills/ for a collection). A
// path reached either way resolves, through the OS, to somewhere inside the
// clone, which is all a git command run there needs to find its root.
func commitAt(ctx context.Context, git func(context.Context, string, ...string) (string, error), dir string) string {
	at := dir
	if entries, err := os.ReadDir(filepath.Join(dir, "skills")); err == nil {
		for _, e := range entries {
			at = filepath.Join(dir, "skills", e.Name())
			break
		}
	}
	out, err := git(ctx, at, "rev-parse", "--short", "HEAD")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// indexEntry is one reference's record in the index file.
type indexEntry struct {
	Dir       string    `json:"dir"`
	Commit    string    `json:"commit"`
	FetchedAt time.Time `json:"fetched_at"`
}

// indexPath is the file Prepare and Update record what they found for each
// reference in, so Info can answer `bees skills list` by reading it instead
// of touching the clones.
func (m *Manager) indexPath() string {
	return filepath.Join(m.CacheDir, "index.json")
}

func (m *Manager) readIndex() map[string]indexEntry {
	data, err := os.ReadFile(m.indexPath())
	if err != nil {
		return nil
	}
	var idx map[string]indexEntry
	if err := json.Unmarshal(data, &idx); err != nil {
		return nil
	}
	return idx
}

func (m *Manager) writeIndexEntry(key string, entry indexEntry) {
	idx := m.readIndex()
	if idx == nil {
		idx = map[string]indexEntry{}
	}
	idx[key] = entry
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return
	}
	if err := os.MkdirAll(m.CacheDir, 0o755); err != nil {
		return
	}
	_ = os.WriteFile(m.indexPath(), data, 0o644)
}
