package skills

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pluginDirFor returns a plugin directory exposing the skill(s) found at
// target: target itself when it is already a Claude Code plugin, or a
// generated wrapper under the manager's cache directory otherwise.
//
// This is the layout step Prepare hands the resolved clone sub-directory
// to, kept behind this one function so richer layout detection (a
// skills-only mode, a symlink-containment check, wrappers keyed by the
// full reference rather than by name alone) can replace its body without
// touching Prepare's clone, refresh or concurrency plumbing.
func (m *Manager) pluginDirFor(name, raw, target string) (string, error) {
	if exists(filepath.Join(target, ".claude-plugin", "plugin.json")) {
		return target, nil
	}
	pluginDir := filepath.Join(m.cacheDir, "plugins", name)
	skillsDir := filepath.Join(pluginDir, "skills")

	// Where the wrapper's symlink lives and what it points at, per layout.
	var link, dest string
	switch {
	case exists(filepath.Join(target, "SKILL.md")):
		link, dest = filepath.Join(skillsDir, name), target
	case isDir(filepath.Join(target, "skills")):
		link, dest = skillsDir, filepath.Join(target, "skills")
	default:
		return "", fmt.Errorf("%s is not a plugin (.claude-plugin/plugin.json), a skill (SKILL.md) or a skills collection (skills/)", target)
	}

	// A wrapper that already points at this target is used as it is:
	// sessions run for a long time with --plugin-dir pointing here, and
	// removing the directory would pull it out from under them. Only a
	// missing manifest or a link pointing elsewhere (the reference's
	// sub-directory or ref changed) forces a rebuild.
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
		"name":        name,
		"description": "busybees skill from " + raw,
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

// baseName is the last path segment of a git URL, with a trailing slash or
// .git suffix removed.
func baseName(url string) string {
	base := strings.TrimSuffix(strings.TrimSuffix(url, "/"), ".git")
	if i := strings.LastIndexAny(base, "/:"); i >= 0 {
		base = base[i+1:]
	}
	return base
}

// skillName derives a filesystem-safe name for a reference's wrapper
// plugin: its URL's base name, plus the last segment of its sub-directory
// when it has one.
func skillName(ref Ref) string {
	name := baseName(ref.URL)
	if ref.Subdir != "" {
		name += "-" + filepath.Base(ref.Subdir)
	}
	return sanitizeName(name)
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
