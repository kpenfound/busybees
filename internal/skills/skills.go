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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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
	// Refresh pulls already-cloned repositories before use.
	Refresh bool
	// Git overrides git execution (tests).
	Git func(ctx context.Context, dir string, args ...string) error
}

// NewManager returns a manager caching under dir.
func NewManager(dir string) *Manager {
	m := &Manager{CacheDir: dir}
	m.Git = func(ctx context.Context, dir string, args ...string) error {
		cmd := exec.CommandContext(ctx, "git", args...)
		if dir != "" {
			cmd.Dir = dir
		}
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	return m
}

// DefaultCacheDir returns ~/.cache/bees.
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

func (m *Manager) clone(ctx context.Context, spec Spec) (string, error) {
	sum := sha256.Sum256([]byte(spec.URL + "@" + spec.Ref))
	dir := filepath.Join(m.CacheDir, "repos", spec.Name+"-"+hex.EncodeToString(sum[:4]))
	if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
		if m.Refresh {
			// Best effort: a failed refresh should not block the factory.
			_ = m.Git(ctx, dir, "pull", "--ff-only", "--quiet")
		}
		return dir, nil
	}
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", err
	}
	args := []string{"clone", "--depth", "1", "--quiet"}
	if spec.Ref != "" {
		args = append(args, "--branch", spec.Ref)
	}
	args = append(args, spec.URL, dir)
	if err := m.Git(ctx, "", args...); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	return dir, nil
}

// pluginDirFor returns a plugin directory exposing the skill(s) in target.
func (m *Manager) pluginDirFor(spec Spec, target string) (string, error) {
	if exists(filepath.Join(target, ".claude-plugin", "plugin.json")) {
		return target, nil
	}
	pluginDir := filepath.Join(m.CacheDir, "plugins", spec.Name)
	skillsDir := filepath.Join(pluginDir, "skills")
	// Rebuild the wrapper every time; it is cheap and avoids stale links.
	if err := os.RemoveAll(pluginDir); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Join(pluginDir, ".claude-plugin"), 0o755); err != nil {
		return "", err
	}
	switch {
	case exists(filepath.Join(target, "SKILL.md")):
		if err := os.MkdirAll(skillsDir, 0o755); err != nil {
			return "", err
		}
		if err := os.Symlink(target, filepath.Join(skillsDir, spec.Name)); err != nil {
			return "", err
		}
	case isDir(filepath.Join(target, "skills")):
		if err := os.Symlink(filepath.Join(target, "skills"), skillsDir); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("%s is not a plugin (.claude-plugin/plugin.json), a skill (SKILL.md) or a skills collection (skills/)", target)
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
