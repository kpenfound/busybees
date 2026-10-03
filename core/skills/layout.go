package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// pluginDirFor is the layout step Prepare hands the clone root and its
// resolved sub-directory to, once the reference is cloned and refreshed. It
// returns a plugin directory exposing the skill(s) found at target: target
// itself when it is already a Claude Code plugin and the manager is not
// SkillsOnly, or a generated wrapper under the manager's cache directory
// otherwise.
//
// By default (SkillsOnly false) it checks, in order:
//
//  1. a Claude Code plugin (.claude-plugin/plugin.json): target is returned
//     as-is;
//  2. a single skill (SKILL.md): wrapped in a generated plugin holding that
//     one skill under skills/;
//  3. a skills collection (a skills/ directory): wrapped in a generated
//     plugin whose skills/ is that directory.
//
// Anything else is an error naming all three shapes.
//
// With SkillsOnly set, case 1 is skipped: a target is accepted only under
// case 2 or 3, and Prepare always returns a generated wrapper holding only
// skills/ and the manifest the wrapper needs, so no hooks, MCP servers,
// agents or commands come through even when target carries them. A target
// with neither SKILL.md nor skills/, including a full plugin that has no
// skills/, is refused with an error naming the two accepted shapes.
func (m *Manager) pluginDirFor(ref Ref, raw, dir, target string) (string, error) {
	within, err := withinClone(dir, target)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %w", target, err)
	}
	if !within {
		return "", fmt.Errorf("sub-directory %q resolves outside the clone", ref.Subdir)
	}

	if !m.SkillsOnly && exists(filepath.Join(target, ".claude-plugin", "plugin.json")) {
		return target, nil
	}

	name := skillName(ref)
	pluginDir := m.wrapperDir(ref)
	skillsDir := filepath.Join(pluginDir, "skills")

	// Where the wrapper's symlink lives and what it points at, per layout.
	var link, dest string
	switch {
	case exists(filepath.Join(target, "SKILL.md")):
		link, dest = filepath.Join(skillsDir, name), target
	case isDir(filepath.Join(target, "skills")):
		link, dest = skillsDir, filepath.Join(target, "skills")
	case m.SkillsOnly:
		return "", fmt.Errorf("%s is not a skill (SKILL.md) or a skills collection (skills/)", target)
	default:
		return "", fmt.Errorf("%s is not a plugin (.claude-plugin/plugin.json), a skill (SKILL.md) or a skills collection (skills/)", target)
	}

	return m.wrapPlugin(pluginDir, link, dest, name, raw)
}

// withinClone reports whether target lies inside the clone rooted at dir
// once every symlink on both sides is followed. Parse can reject a
// sub-directory that is absolute or climbs above its root syntactically,
// but it cannot see a symlink inside the repository itself that walks
// outside the clone once the repository is actually on disk; this is the
// check that catches that case.
func withinClone(dir, target string) (bool, error) {
	root, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false, err
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false, err
	}
	if resolved == root {
		return true, nil
	}
	return strings.HasPrefix(resolved, root+string(filepath.Separator)), nil
}

// wrapPlugin returns pluginDir holding a generated plugin manifest and a
// skills/ symlink at link pointing at dest, creating or replacing the
// wrapper unless it already exists and link already points at dest.
//
// A wrapper that already points at dest is reused as it is: sessions run
// for a long time with --plugin-dir pointing here, and removing the
// directory would pull it out from under them, including when the clone it
// was built from has since been refreshed to a new commit. Only a missing
// manifest or a link pointing elsewhere (the reference's sub-directory or
// ref changed) forces a rebuild.
func (m *Manager) wrapPlugin(pluginDir, link, dest, name, raw string) (string, error) {
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

// wrapperDir is the cache directory a reference's generated wrapper plugin
// lives in, keyed the same way cloneDir keys clone directories: a
// readable name, followed by a short hash of the reference's full string
// (Ref.String()), so two references that derive the same skill name from
// different repositories (for example acme/skills and other/skills) never
// share a wrapper directory, and preparing them alternately never rebuilds
// either one's wrapper.
func (m *Manager) wrapperDir(ref Ref) string {
	return filepath.Join(m.cacheDir, "plugins", keyedName(skillName(ref), ref))
}

// keyedName returns name (or "ref" when it is empty) followed by a short
// hash of ref's full reference string. cloneDirName and wrapperDir both use
// this scheme, so a directory is never keyed by a derived name alone.
func keyedName(name string, ref Ref) string {
	if name == "" {
		name = "ref"
	}
	sum := sha256.Sum256([]byte(ref.String()))
	return name + "-" + hex.EncodeToString(sum[:4])
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
