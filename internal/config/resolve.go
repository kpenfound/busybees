package config

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	"github.com/kpenfound/busybees/internal/workspace"
)

// Resolve fills in settings that are derived from the git clone containing
// bees.toml: project.repo from the remote's URL and project.default_branch
// from the remote's HEAD. Explicit values in bees.toml win.
func (c *Config) Resolve(ctx context.Context) error {
	dir := c.Dir()
	if c.Project.Repo == "" {
		url, err := workspace.Git(ctx, dir, "remote", "get-url", c.Project.Remote)
		if err != nil {
			return fmt.Errorf("project.repo is not set and remote %q has no URL: %w", c.Project.Remote, err)
		}
		repo, ok := ParseGitHubRepo(url)
		if !ok {
			return fmt.Errorf("project.repo is not set and remote %q URL %q is not a GitHub repository; set project.repo = \"owner/name\"", c.Project.Remote, url)
		}
		c.Project.Repo = repo
	}
	if c.Project.DefaultBranch == "" {
		branch, err := workspace.DefaultBranch(ctx, dir, c.Project.Remote)
		if err != nil {
			return fmt.Errorf("project.default_branch is not set and could not be detected from remote %q: %w", c.Project.Remote, err)
		}
		c.Project.DefaultBranch = branch
	}
	return nil
}

var githubURL = regexp.MustCompile(`^(?:https?://|ssh://)?(?:[^@/]+@)?github\.com[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

// ParseGitHubRepo extracts owner/name from a GitHub remote URL in https,
// ssh:// or scp-like form.
func ParseGitHubRepo(url string) (string, bool) {
	m := githubURL.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return "", false
	}
	return m[1] + "/" + m[2], true
}
