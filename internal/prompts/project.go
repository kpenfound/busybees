package prompts

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
)

// ProjectDir is the directory a project repository keeps its own role
// instructions in: bees/prompts/common.md is appended to every role's system
// prompt and bees/prompts/<role>.md to one role's, after the text bees.toml
// configures. The files live in the repository, so they are versioned,
// reviewed in a pull request like code, and a branch can carry its own.
//
// Dotless on purpose. `bees init` gitignores the state directory (/.bees/),
// so files under .bees/prompts/ would be untracked by default - the opposite
// of instructions reviewed like code - whatever state_dir points at.
const ProjectDir = "bees/prompts"

// CommonPromptFile is the project prompt file every role reads.
const CommonPromptFile = "common.md"

// MaxProjectPromptBytes is the largest project prompt file bees reads. It is
// a guard against a session's context being spent on a file nobody meant as a
// prompt, not a budget: real instructions are far smaller.
const MaxProjectPromptBytes = 64 << 10

// ProjectPrompt is the text of one project prompt file, and the path the
// rendered system prompt names it by.
type ProjectPrompt struct {
	// Path is the file's repository-relative path, always slash-separated
	// ("bees/prompts/developer.md").
	Path string
	// Text is the file's contents, trimmed. Never empty: a file with nothing
	// in it is dropped rather than rendered as an empty section.
	Text string
}

// projectFiles are the names a role reads, in the order their text is
// appended to its prompt.
func projectFiles(role string) []string {
	return []string{CommonPromptFile, role + ".md"}
}

// LoadProject reads a role's project prompt files from the repository checked
// out at repoDir - a session's worktree, so a branch's own instructions apply
// to the session working on that branch.
//
// A missing bees/prompts/ directory, and a missing file inside it, are the
// normal case: they read as no prompts, with no error and nothing logged.
// Anything else - a file that cannot be read, or one past
// MaxProjectPromptBytes - comes back as an error alongside the files that did
// read, so a caller can use what it has and report the rest.
func LoadProject(repoDir, role string) ([]ProjectPrompt, error) {
	var out []ProjectPrompt
	var errs []error
	for _, name := range projectFiles(role) {
		rel := path.Join(ProjectDir, name)
		full := filepath.Join(repoDir, filepath.FromSlash(rel))
		info, err := os.Stat(full)
		switch {
		case errors.Is(err, os.ErrNotExist):
			continue
		case err != nil:
			errs = append(errs, fmt.Errorf("%s: %w", rel, err))
			continue
		case info.IsDir():
			errs = append(errs, fmt.Errorf("%s is a directory, not a prompt file", rel))
			continue
		case info.Size() > MaxProjectPromptBytes:
			errs = append(errs, fmt.Errorf("%s is %d bytes, over the %d byte limit", rel, info.Size(), MaxProjectPromptBytes))
			continue
		}
		b, err := os.ReadFile(full)
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rel, err))
			continue
		}
		if s := strings.TrimSpace(string(b)); s != "" {
			out = append(out, ProjectPrompt{Path: rel, Text: s})
		}
	}
	return out, errors.Join(errs...)
}

// ProjectPromptFiles lists the markdown files in repoDir's bees/prompts
// directory, split into the ones a role reads (common.md, and <role>.md for a
// role in config.Roles) and the ones bees ignores, which are almost always a
// misspelled role name. Files that are not markdown are neither: a repository
// may keep whatever else it likes there.
//
// A missing directory yields nothing and no error - every repository that has
// never heard of this feature is in that state.
func ProjectPromptFiles(repoDir string) (known, unknown []string, err error) {
	entries, err := os.ReadDir(filepath.Join(repoDir, filepath.FromSlash(ProjectDir)))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	valid := map[string]bool{CommonPromptFile: true}
	for _, role := range config.Roles {
		valid[role+".md"] = true
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		if valid[name] {
			known = append(known, path.Join(ProjectDir, name))
			continue
		}
		unknown = append(unknown, path.Join(ProjectDir, name))
	}
	sort.Strings(known)
	sort.Strings(unknown)
	return known, unknown, nil
}
