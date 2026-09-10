package review

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/workspace"
)

// Remote is the git remote `bees review` reads a repository name from when
// the pull request reference does not carry one.
const Remote = "origin"

// Ref is the pull request a review was asked for.
type Ref struct {
	// Repo is the repository the pull request belongs to, "owner/name".
	Repo string `json:"repo"`
	// Number is the pull request number.
	Number int `json:"number"`
}

// String is the reference in the form a person writes it, "owner/name#123".
func (r Ref) String() string { return fmt.Sprintf("%s#%d", r.Repo, r.Number) }

// URL is the pull request's page on github.com.
func (r Ref) URL() string {
	return fmt.Sprintf("https://github.com/%s/pull/%d", r.Repo, r.Number)
}

// The three forms a pull request reference takes. A URL may carry the tab
// GitHub was on ("/files") and an anchor; a bare number may carry the "#" a
// person writes out of habit.
var (
	refURL    = regexp.MustCompile(`^(?:https?://)?(?:www\.)?github\.com/([^/\s]+)/([^/\s]+)/pull/(\d+)(?:[/?#].*)?$`)
	refRepo   = regexp.MustCompile(`^([^/\s#]+)/([^/\s#]+)#(\d+)$`)
	refNumber = regexp.MustCompile(`^#?(\d+)$`)
)

// ParseRef reads a pull request reference in any of the three forms
// `bees review` takes: a github.com URL, "owner/name#123", or a bare number.
// A bare number leaves Repo empty, for ResolveRef to fill in from the
// checkout the command was run in.
func ParseRef(s string) (Ref, error) {
	s = strings.TrimSpace(s)
	switch {
	case s == "":
		return Ref{}, fmt.Errorf("no pull request given: a github.com URL, owner/name#123, or the number of a pull request in this checkout")
	case refURL.MatchString(s):
		m := refURL.FindStringSubmatch(s)
		return newRef(m[1]+"/"+m[2], m[3], s)
	case refRepo.MatchString(s):
		m := refRepo.FindStringSubmatch(s)
		return newRef(m[1]+"/"+m[2], m[3], s)
	case refNumber.MatchString(s):
		return newRef("", refNumber.FindStringSubmatch(s)[1], s)
	}
	return Ref{}, fmt.Errorf("%q is not a pull request: give a github.com URL, owner/name#123, or the number of a pull request in this checkout", s)
}

func newRef(repo, number, s string) (Ref, error) {
	n, err := strconv.Atoi(number)
	if err != nil || n <= 0 {
		return Ref{}, fmt.Errorf("%q is not a pull request: %s is not a pull request number", s, number)
	}
	return Ref{Repo: repo, Number: n}, nil
}

// ResolveRef parses a pull request reference and, when it names no
// repository, takes the one dir's git remote points at.
func ResolveRef(ctx context.Context, s, dir string) (Ref, error) {
	ref, err := ParseRef(s)
	if err != nil || ref.Repo != "" {
		return ref, err
	}
	repo, ok := RepoOf(ctx, dir)
	if !ok {
		return Ref{}, fmt.Errorf("%q is a pull request number and %s has no GitHub %s remote to take the repository from: give a URL, or owner/name#%d", s, dirName(dir), Remote, ref.Number)
	}
	ref.Repo = repo
	return ref, nil
}

// RepoOf is the GitHub repository dir's git remote points at, and false when
// dir is not a checkout of one.
func RepoOf(ctx context.Context, dir string) (string, bool) {
	url, err := workspace.Git(ctx, dir, "remote", "get-url", Remote)
	if err != nil {
		return "", false
	}
	return config.ParseGitHubRepo(url)
}

// CheckoutOf is dir when it is a checkout of repo, and "" when it is not:
// the sources that read files then gather nothing rather than reading an
// unrelated repository's style rules into the review.
func CheckoutOf(ctx context.Context, repo, dir string) string {
	if got, ok := RepoOf(ctx, dir); ok && strings.EqualFold(got, repo) {
		return dir
	}
	return ""
}

// dirName names a directory in an error message, and says "this directory"
// for the one the command was run in.
func dirName(dir string) string {
	if dir == "" || dir == "." {
		return "this directory"
	}
	return dir
}
