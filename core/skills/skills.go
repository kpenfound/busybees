// Package skills parses and prepares skill references for agent.SkillPreparer.
//
// A reference has the form <git-url>[@<ref>][#<sub/dir>], for example
//
//	https://github.com/acme/skills#skills/tdd
//	https://github.com/acme/my-plugin@v1.2.0
//	git@github.com:acme/my-plugin@v1.2.0
package skills

import (
	"fmt"
	"path"
	"strings"
)

// Ref is a parsed skill reference: a git URL, an optional ref (branch, tag
// or commit) and an optional sub-directory within the repository.
type Ref struct {
	URL    string
	Ref    string
	Subdir string
}

// String is a stable string form of the full reference, built from URL, Ref
// and Subdir. Two Refs differing in any field have distinct strings; this is
// what the manager keys cache directories on.
func (r Ref) String() string {
	s := r.URL
	if r.Ref != "" {
		s += "@" + r.Ref
	}
	if r.Subdir != "" {
		s += "#" + r.Subdir
	}
	return s
}

// Parse splits a skill reference of the form <git-url>[@<ref>][#<sub/dir>]
// into its URL, ref and sub-directory.
//
// A trailing "@ref" is only taken as a ref when it comes after the last "/"
// or ":" of the remaining string, so scp-style URLs such as
// git@host:org/repo and git@host:org/repo@v1 still parse correctly.
//
// Parse rejects an empty URL and a sub-directory that is absolute or that,
// once cleaned, climbs above its root (for example #../x or #a/../../x).
func Parse(raw string) (Ref, error) {
	rest := strings.TrimSpace(raw)
	if rest == "" {
		return Ref{}, fmt.Errorf("skills: empty reference")
	}

	var subdir string
	if i := strings.Index(rest, "#"); i >= 0 {
		subdir = rest[i+1:]
		rest = rest[:i]
	}

	var ref string
	if i := strings.LastIndex(rest, "@"); i > strings.LastIndexAny(rest, "/:") {
		ref = rest[i+1:]
		rest = rest[:i]
	}

	if rest == "" {
		return Ref{}, fmt.Errorf("skills: %q has no git url", raw)
	}

	cleanSubdir, err := cleanSubdir(subdir)
	if err != nil {
		return Ref{}, fmt.Errorf("skills: %q: %w", raw, err)
	}

	return Ref{URL: rest, Ref: ref, Subdir: cleanSubdir}, nil
}

// cleanSubdir validates and normalizes a sub-directory taken from a skill
// reference. It rejects an absolute path and one that, after path.Clean,
// climbs above its root.
func cleanSubdir(subdir string) (string, error) {
	if subdir == "" {
		return "", nil
	}
	if path.IsAbs(subdir) {
		return "", fmt.Errorf("sub-directory %q must not be absolute", subdir)
	}
	cleaned := path.Clean(subdir)
	if cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("sub-directory %q climbs above its root", subdir)
	}
	if cleaned == "." {
		return "", nil
	}
	return cleaned, nil
}
