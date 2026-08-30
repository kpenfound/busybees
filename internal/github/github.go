// Package github is a thin wrapper around the `gh` CLI. It uses the user's
// existing gh authentication, so the factory needs no extra tokens.
package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Client runs gh commands against one repository.
type Client struct {
	Repo string // owner/name
	// Exec overrides command execution (tests).
	Exec func(ctx context.Context, args ...string) ([]byte, error)
}

// New returns a client for repo.
func New(repo string) *Client {
	c := &Client{Repo: repo}
	c.Exec = c.run
	return c
}

func (c *Client) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.Bytes(), nil
}

// Label is a GitHub label.
type Label struct {
	Name string `json:"name"`
}

// MilestoneRef is the milestone an issue or pull request is in.
type MilestoneRef struct {
	Title string `json:"title"`
}

// Milestone is a GitHub milestone.
type Milestone struct {
	Number       int    `json:"number"`
	Title        string `json:"title"`
	Description  string `json:"description"`
	State        string `json:"state"`
	DueOn        string `json:"due_on,omitempty"`
	OpenIssues   int    `json:"open_issues"`
	ClosedIssues int    `json:"closed_issues"`
}

// Author is the actor that created an issue or PR.
type Author struct {
	Login string `json:"login"`
}

// Issue is a GitHub issue.
type Issue struct {
	Number    int           `json:"number"`
	Title     string        `json:"title"`
	Body      string        `json:"body"`
	State     string        `json:"state"`
	URL       string        `json:"url"`
	Labels    []Label       `json:"labels"`
	Milestone *MilestoneRef `json:"milestone"`
	Author    Author        `json:"author"`
	Assignees []Author      `json:"assignees"`
	CreatedAt time.Time     `json:"createdAt"`
	UpdatedAt time.Time     `json:"updatedAt"`
	Comments  []Comment     `json:"comments,omitempty"`
}

// MilestoneTitle returns the milestone title or "".
func (i Issue) MilestoneTitle() string {
	if i.Milestone == nil {
		return ""
	}
	return i.Milestone.Title
}

// Comment is an issue or PR comment.
type Comment struct {
	Author    Author    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
}

// IsBee reports whether the comment was written by a bees role.
func (c Comment) IsBee() bool { return strings.Contains(c.Body, BeesMarker) }

// AwaitingBee reports whether the latest human activity on an issue (its
// creation or a human comment) is more recent than the latest bee comment,
// i.e. a bee still owes a reply.
func (i Issue) AwaitingBee() bool {
	lastHuman := i.CreatedAt
	var lastBee time.Time
	for _, c := range i.Comments {
		if c.IsBee() {
			if c.CreatedAt.After(lastBee) {
				lastBee = c.CreatedAt
			}
		} else if c.CreatedAt.After(lastHuman) {
			lastHuman = c.CreatedAt
		}
	}
	return lastHuman.After(lastBee)
}

// PR is a GitHub pull request.
type PR struct {
	Number      int        `json:"number"`
	Title       string     `json:"title"`
	Body        string     `json:"body"`
	State       string     `json:"state"`
	URL         string     `json:"url"`
	Labels      []Label    `json:"labels"`
	HeadRefName string     `json:"headRefName"`
	BaseRefName string     `json:"baseRefName"`
	IsDraft     bool       `json:"isDraft"`
	MergedAt    *time.Time `json:"mergedAt"`
	MergeCommit *struct {
		OID string `json:"oid"`
	} `json:"mergeCommit"`
	Author    Author        `json:"author"`
	Assignees []Author      `json:"assignees"`
	Milestone *MilestoneRef `json:"milestone"`
	CreatedAt time.Time     `json:"createdAt"`
	UpdatedAt time.Time     `json:"updatedAt"`
	// HeadSHA is the commit the PR's head branch points at (headRefOid).
	HeadSHA string `json:"headRefOid"`
	// Mergeable is GitHub's verdict on merging the PR into its base:
	// MERGEABLE, CONFLICTING or UNKNOWN. GitHub computes it asynchronously,
	// so UNKNOWN (or empty) means "not computed yet", not "fine".
	Mergeable string `json:"mergeable"`
	// MergeStateStatus refines Mergeable: BEHIND (no conflict but the base
	// moved on), DIRTY (conflicts), CLEAN, BLOCKED, UNSTABLE, UNKNOWN, ...
	MergeStateStatus string `json:"mergeStateStatus"`
}

// MilestoneTitle returns the milestone title or "".
func (p PR) MilestoneTitle() string {
	if p.Milestone == nil {
		return ""
	}
	return p.Milestone.Title
}

// Merge-state values gh reports for a pull request.
const (
	MergeableConflicting = "CONFLICTING"
	MergeStateBehind     = "BEHIND"
)

// Conflicting reports whether GitHub says the PR cannot be merged into
// its base because of conflicts.
func (p PR) Conflicting() bool { return p.Mergeable == MergeableConflicting }

// Behind reports whether the PR merges cleanly but its base has moved on
// since the branch was last updated.
func (p PR) Behind() bool { return !p.Conflicting() && p.MergeStateStatus == MergeStateBehind }

// HasLabel reports whether the item carries label name.
func HasLabel(labels []Label, name string) bool {
	for _, l := range labels {
		if l.Name == name {
			return true
		}
	}
	return false
}

// HasAssignee reports whether login is one of the assignees. GitHub logins are
// case-insensitive.
func HasAssignee(assignees []Author, login string) bool {
	for _, a := range assignees {
		if strings.EqualFold(a.Login, login) {
			return true
		}
	}
	return false
}

// LabelNames returns the label names.
func LabelNames(labels []Label) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		out = append(out, l.Name)
	}
	return out
}

// closingRef matches GitHub's closing keywords followed by an issue
// reference: "Closes #12", "fixes: #12", "Resolved https://github.com/o/r/issues/12".
var closingRef = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?)\s*:?\s*(?:#(\d+)|(https://github\.com/[^/\s]+/[^/\s]+)/issues/(\d+))`)

// ClosingIssues returns the issue numbers a PR closes, in order of first
// mention. They are derived from the closing keywords in the body (the same
// rule GitHub applies when the PR merges) because older gh releases cannot
// return closingIssuesReferences. Cross-repository references are ignored.
func (p PR) ClosingIssues() []int {
	var out []int
	seen := map[int]bool{}
	repoURL := ""
	if i := strings.Index(p.URL, "/pull/"); i > 0 {
		repoURL = p.URL[:i]
	}
	for _, m := range closingRef.FindAllStringSubmatch(p.Body, -1) {
		num := m[1]
		if num == "" {
			if repoURL == "" || !strings.EqualFold(m[2], repoURL) {
				continue
			}
			num = m[3]
		}
		n, err := strconv.Atoi(num)
		if err != nil || n <= 0 || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// Query selects which issues and PRs are visible. Criteria are ANDed.
type Query struct {
	Label     string // when non-empty, items must carry this label
	Assignee  string // when non-empty, items must be assigned to this login
	Milestone string // when non-empty, items must be in this milestone
}

func (q Query) args() []string {
	var a []string
	if q.Label != "" {
		a = append(a, "--label", q.Label)
	}
	if q.Assignee != "" {
		a = append(a, "--assignee", q.Assignee)
	}
	if q.Milestone != "" {
		a = append(a, "--milestone", q.Milestone)
	}
	return a
}

// Matches reports whether an item with the given labels, assignees and
// milestone satisfies the query (used to double-check server results).
func (q Query) Matches(labels []Label, assignees []Author, milestone string) bool {
	if q.Label != "" && !HasLabel(labels, q.Label) {
		return false
	}
	if q.Assignee != "" && q.Assignee != "@me" {
		found := false
		for _, a := range assignees {
			if strings.EqualFold(a.Login, q.Assignee) {
				found = true
			}
		}
		if !found {
			return false
		}
	}
	if q.Milestone != "" && milestone != q.Milestone {
		return false
	}
	return true
}

const issueFields = "number,title,body,state,url,labels,milestone,author,assignees,createdAt,updatedAt"
const prFields = "number,title,body,state,url,labels,headRefName,baseRefName,isDraft,mergedAt,mergeCommit,author,assignees,milestone,createdAt,updatedAt,headRefOid,mergeable,mergeStateStatus"

// ListOpenIssues returns open issues matching q.
func (c *Client) ListOpenIssues(ctx context.Context, q Query) ([]Issue, error) {
	args := append([]string{"issue", "list", "-R", c.Repo, "--state", "open", "--limit", "500", "--json", issueFields}, q.args()...)
	out, err := c.Exec(ctx, args...)
	if err != nil {
		return nil, err
	}
	var issues []Issue
	return issues, json.Unmarshal(out, &issues)
}

// GetIssue returns one issue including comments.
func (c *Client) GetIssue(ctx context.Context, number int) (Issue, error) {
	out, err := c.Exec(ctx, "issue", "view", strconv.Itoa(number), "-R", c.Repo, "--json", issueFields+",comments")
	if err != nil {
		return Issue{}, err
	}
	var issue Issue
	return issue, json.Unmarshal(out, &issue)
}

// ListOpenPRs returns open PRs matching q.
func (c *Client) ListOpenPRs(ctx context.Context, q Query) ([]PR, error) {
	args := append([]string{"pr", "list", "-R", c.Repo, "--state", "open", "--limit", "200", "--json", prFields}, q.args()...)
	out, err := c.Exec(ctx, args...)
	if err != nil {
		return nil, err
	}
	var prs []PR
	return prs, json.Unmarshal(out, &prs)
}

// ListMergedPRsSince returns PRs matching q merged at or after t.
func (c *Client) ListMergedPRsSince(ctx context.Context, q Query, t time.Time) ([]PR, error) {
	search := fmt.Sprintf("merged:>=%s", t.UTC().Format("2006-01-02T15:04:05Z"))
	args := append([]string{"pr", "list", "-R", c.Repo, "--state", "merged", "--search", search, "--limit", "100", "--json", prFields}, q.args()...)
	out, err := c.Exec(ctx, args...)
	if err != nil {
		return nil, err
	}
	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	// The search filter is date-granular server side; enforce precisely.
	var filtered []PR
	for _, p := range prs {
		if p.MergedAt != nil && !p.MergedAt.Before(t) {
			filtered = append(filtered, p)
		}
	}
	return filtered, nil
}

// GetPR returns one PR.
func (c *Client) GetPR(ctx context.Context, number int) (PR, error) {
	out, err := c.Exec(ctx, "pr", "view", strconv.Itoa(number), "-R", c.Repo, "--json", prFields)
	if err != nil {
		return PR{}, err
	}
	var pr PR
	return pr, json.Unmarshal(out, &pr)
}

// FindPRForBranch returns the open PR whose head is branch, if any.
func (c *Client) FindPRForBranch(ctx context.Context, branch string) (*PR, error) {
	out, err := c.Exec(ctx, "pr", "list", "-R", c.Repo, "--head", branch, "--state", "open", "--json", prFields)
	if err != nil {
		return nil, err
	}
	var prs []PR
	if err := json.Unmarshal(out, &prs); err != nil {
		return nil, err
	}
	if len(prs) == 0 {
		return nil, nil
	}
	return &prs[0], nil
}

// Assign adds assignees to an issue or a pull request. Existing assignees are
// kept: the endpoint is additive.
//
// It goes straight to the REST endpoint rather than through
// `gh issue edit --add-assignee`, which fails against GitHub with a "Projects
// (classic) is being deprecated" GraphQL error when the number is a pull
// request. GitHub's issues API addresses pull requests by the same number, so
// one call covers both.
func (c *Client) Assign(ctx context.Context, number int, logins ...string) error {
	if len(logins) == 0 {
		return nil
	}
	args := []string{"api", "--method", "POST", fmt.Sprintf("repos/%s/issues/%d/assignees", c.Repo, number)}
	for _, l := range logins {
		args = append(args, "-f", "assignees[]="+l)
	}
	_, err := c.Exec(ctx, args...)
	return err
}

// SetMilestone puts an issue or a pull request in the open milestone with the
// given title. Like Assign it uses the REST endpoint, which addresses both by
// the same number. An empty title is a no-op; a title that matches no open
// milestone is an error.
func (c *Client) SetMilestone(ctx context.Context, number int, title string) error {
	if title == "" {
		return nil
	}
	milestones, err := c.ListMilestones(ctx)
	if err != nil {
		return err
	}
	for _, m := range milestones {
		if strings.EqualFold(m.Title, title) {
			_, err := c.Exec(ctx, "api", "--method", "PATCH", fmt.Sprintf("repos/%s/issues/%d", c.Repo, number), "-F", fmt.Sprintf("milestone=%d", m.Number))
			return err
		}
	}
	return fmt.Errorf("no open milestone %q in %s", title, c.Repo)
}

// RequestReview asks the given GitHub logins and org/team slugs (as
// "myorg/bees-team") to review a pull request.
//
// It goes straight to the REST endpoint rather than through
// `gh pr edit --add-reviewer`, which fails against GitHub with a "Projects
// (classic) is being deprecated" GraphQL error. GitHub refuses to request a
// review from the pull request's own author, so a login that authored it is
// rejected with a 422; teams are always accepted.
func (c *Client) RequestReview(ctx context.Context, number int, reviewers ...string) error {
	var logins, teams []string
	for _, r := range reviewers {
		r = strings.TrimSpace(r)
		switch {
		case r == "":
		case strings.Contains(r, "/"):
			// The org is implied by the repository; the endpoint takes slugs.
			_, team, _ := strings.Cut(r, "/")
			teams = append(teams, team)
		default:
			logins = append(logins, r)
		}
	}
	if len(logins) == 0 && len(teams) == 0 {
		return nil
	}
	args := []string{"api", "-X", "POST", fmt.Sprintf("repos/%s/pulls/%d/requested_reviewers", c.Repo, number)}
	for _, l := range logins {
		args = append(args, "-f", "reviewers[]="+l)
	}
	for _, t := range teams {
		args = append(args, "-f", "team_reviewers[]="+t)
	}
	_, err := c.Exec(ctx, args...)
	return err
}

// CurrentUser returns the authenticated gh login.
func CurrentUser(ctx context.Context) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "api", "user", "--jq", ".login")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh api user: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// EditLabels adds and removes labels on an issue or PR (gh treats both the
// same for label edits).
func (c *Client) EditLabels(ctx context.Context, number int, add, remove []string) error {
	args := []string{"issue", "edit", strconv.Itoa(number), "-R", c.Repo}
	for _, l := range add {
		args = append(args, "--add-label", l)
	}
	for _, l := range remove {
		args = append(args, "--remove-label", l)
	}
	if len(add) == 0 && len(remove) == 0 {
		return nil
	}
	_, err := c.Exec(ctx, args...)
	return err
}

// SetState moves an issue to exactly one workflow state label, removing the
// others in states. Labels not in states are untouched.
func (c *Client) SetState(ctx context.Context, number int, current []Label, to string, states []string) error {
	var remove []string
	for _, s := range states {
		if s != to && HasLabel(current, s) {
			remove = append(remove, s)
		}
	}
	var add []string
	if to != "" && !HasLabel(current, to) {
		add = append(add, to)
	}
	return c.EditLabels(ctx, number, add, remove)
}

// ListLabels returns the labels that exist in the repository.
func (c *Client) ListLabels(ctx context.Context) ([]Label, error) {
	out, err := c.Exec(ctx, "label", "list", "-R", c.Repo, "--limit", "200", "--json", "name")
	if err != nil {
		return nil, err
	}
	var labels []Label
	return labels, json.Unmarshal(out, &labels)
}

// EnsureLabel creates or updates a label.
func (c *Client) EnsureLabel(ctx context.Context, name, color, description string) error {
	_, err := c.Exec(ctx, "label", "create", name, "-R", c.Repo, "--color", color, "--description", description, "--force")
	return err
}

// ListMilestones returns open milestones.
func (c *Client) ListMilestones(ctx context.Context) ([]Milestone, error) {
	out, err := c.Exec(ctx, "api", fmt.Sprintf("repos/%s/milestones?state=open&per_page=100", c.Repo))
	if err != nil {
		return nil, err
	}
	var ms []Milestone
	return ms, json.Unmarshal(out, &ms)
}

// DefaultBranch returns the repository default branch.
func (c *Client) DefaultBranch(ctx context.Context) (string, error) {
	out, err := c.Exec(ctx, "repo", "view", c.Repo, "--json", "defaultBranchRef", "--jq", ".defaultBranchRef.name")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// CurrentRepo detects the repo for the git checkout in dir.
func CurrentRepo(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "gh", "repo", "view", "--json", "nameWithOwner", "--jq", ".nameWithOwner")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("gh repo view: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// MergePR merges a PR with method squash, merge or rebase (used only when
// the reviewer's auto_merge is on).
func (c *Client) MergePR(ctx context.Context, number int, method string) error {
	if method == "" {
		method = "squash"
	}
	_, err := c.Exec(ctx, "pr", "merge", strconv.Itoa(number), "-R", c.Repo, "--"+method, "--delete-branch")
	return err
}

// Check is one required status check on a pull request.
type Check struct {
	Name        string `json:"name"`
	State       string `json:"state"`  // SUCCESS, FAILURE, PENDING, ...
	Bucket      string `json:"bucket"` // pass, fail, pending, skipping, cancel
	Link        string `json:"link"`
	Description string `json:"description"`
	Workflow    string `json:"workflow"`
}

// ChecksStatus summarises a set of checks.
type ChecksStatus string

// Check outcomes.
const (
	ChecksPassed  ChecksStatus = "passed"
	ChecksFailed  ChecksStatus = "failed"
	ChecksPending ChecksStatus = "pending"
	// ChecksNone means nothing was reported at all. It is deliberately not
	// ChecksPassed: "nothing reported" and "everything green" are different
	// answers, and a caller that merges on them must say which one it got.
	ChecksNone ChecksStatus = "none"
)

// Summarize returns the overall status of the checks. An empty list is
// ChecksNone, not ChecksPassed: there is nothing to wait for, but there is
// also nothing that passed.
func Summarize(checks []Check) ChecksStatus {
	if len(checks) == 0 {
		return ChecksNone
	}
	status := ChecksPassed
	for _, c := range checks {
		switch c.Bucket {
		case "fail", "cancel":
			return ChecksFailed
		case "pending":
			status = ChecksPending
		}
	}
	return status
}

// Failed returns the checks that failed or were cancelled.
func Failed(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if c.Bucket == "fail" || c.Bucket == "cancel" {
			out = append(out, c)
		}
	}
	return out
}

// RequiredChecks returns the required checks of a PR: the checks the branch
// protection rules make mandatory. An empty result means the branch requires
// nothing, not that everything passed.
func (c *Client) RequiredChecks(ctx context.Context, number int) ([]Check, error) {
	return c.checks(ctx, number, true)
}

// Checks returns every check a PR reports, required or not. It is the
// fallback gate for a repository whose default branch has no branch
// protection: gating on the checks that exist beats gating on nothing.
func (c *Client) Checks(ctx context.Context, number int) ([]Check, error) {
	return c.checks(ctx, number, false)
}

// checks runs `gh pr checks`, with --required when required is set. gh exits
// non-zero when checks are pending (8) or failing (1) while still printing
// JSON, and reports "no required checks" / "no checks reported" when there
// are none; both cases are handled.
func (c *Client) checks(ctx context.Context, number int, required bool) ([]Check, error) {
	args := []string{"pr", "checks", strconv.Itoa(number), "-R", c.Repo}
	if required {
		args = append(args, "--required")
	}
	args = append(args, "--json", "name,state,bucket,link,description,workflow")
	out, err := c.Exec(ctx, args...)
	trimmed := strings.TrimSpace(string(out))
	if trimmed == "" || trimmed == "[]" {
		if err != nil && !strings.Contains(err.Error(), "no required checks") && !strings.Contains(err.Error(), "no checks reported") {
			return nil, err
		}
		return nil, nil
	}
	var checks []Check
	if jerr := json.Unmarshal([]byte(trimmed), &checks); jerr != nil {
		if err != nil {
			return nil, err
		}
		return nil, jerr
	}
	return checks, nil
}

// Comment posts a comment on an issue or PR. The factory only uses this for
// escalations to humans (needs-human), never for role-to-role messaging.
func (c *Client) Comment(ctx context.Context, number int, body string) error {
	_, err := c.Exec(ctx, "issue", "comment", strconv.Itoa(number), "-R", c.Repo, "--body", body)
	return err
}

// BeesMarker is the invisible marker every comment written by a bees role
// carries, so the orchestrator can tell bee comments from human comments
// made with the same GitHub account.
const BeesMarker = "<!-- bees:"

// Activity is one human-visible event on a pull request: a review, an
// inline review comment or a conversation comment.
type Activity struct {
	Kind      string    `json:"kind"` // review, review-comment, comment
	ID        int64     `json:"id"`
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	State     string    `json:"state,omitempty"` // reviews: APPROVED, CHANGES_REQUESTED, COMMENTED
	Path      string    `json:"path,omitempty"`
	Line      int       `json:"line,omitempty"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"created_at"`
}

// IsBee reports whether the activity was written by a bees role.
func (a Activity) IsBee() bool { return strings.Contains(a.Body, BeesMarker) }

// PRActivity returns reviews and comments on a PR created after since,
// oldest first, excluding those written by bees roles and empty approvals.
func (c *Client) PRActivity(ctx context.Context, number int, since time.Time) ([]Activity, error) {
	var out []Activity

	var reviews []struct {
		ID          int64     `json:"id"`
		User        Author    `json:"user"`
		Body        string    `json:"body"`
		State       string    `json:"state"`
		HTMLURL     string    `json:"html_url"`
		SubmittedAt time.Time `json:"submitted_at"`
	}
	if err := c.apiList(ctx, fmt.Sprintf("repos/%s/pulls/%d/reviews", c.Repo, number), &reviews); err != nil {
		return nil, err
	}
	for _, r := range reviews {
		if r.State == "PENDING" || (r.State == "APPROVED" && strings.TrimSpace(r.Body) == "") || (r.State == "COMMENTED" && strings.TrimSpace(r.Body) == "") {
			continue
		}
		out = append(out, Activity{Kind: "review", ID: r.ID, Author: r.User.Login, Body: r.Body, State: r.State, URL: r.HTMLURL, CreatedAt: r.SubmittedAt})
	}

	var reviewComments []struct {
		ID        int64     `json:"id"`
		User      Author    `json:"user"`
		Body      string    `json:"body"`
		Path      string    `json:"path"`
		Line      int       `json:"line"`
		HTMLURL   string    `json:"html_url"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := c.apiList(ctx, fmt.Sprintf("repos/%s/pulls/%d/comments", c.Repo, number), &reviewComments); err != nil {
		return nil, err
	}
	for _, r := range reviewComments {
		out = append(out, Activity{Kind: "review-comment", ID: r.ID, Author: r.User.Login, Body: r.Body, Path: r.Path, Line: r.Line, URL: r.HTMLURL, CreatedAt: r.CreatedAt})
	}

	var comments []struct {
		ID        int64     `json:"id"`
		User      Author    `json:"user"`
		Body      string    `json:"body"`
		HTMLURL   string    `json:"html_url"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := c.apiList(ctx, fmt.Sprintf("repos/%s/issues/%d/comments", c.Repo, number), &comments); err != nil {
		return nil, err
	}
	for _, r := range comments {
		out = append(out, Activity{Kind: "comment", ID: r.ID, Author: r.User.Login, Body: r.Body, URL: r.HTMLURL, CreatedAt: r.CreatedAt})
	}

	var filtered []Activity
	for _, a := range out {
		if a.CreatedAt.After(since) && !a.IsBee() {
			filtered = append(filtered, a)
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].CreatedAt.Before(filtered[j].CreatedAt) })
	return filtered, nil
}

func (c *Client) apiList(ctx context.Context, path string, v any) error {
	out, err := c.Exec(ctx, "api", "--paginate", "--slurp", path)
	if err != nil {
		return err
	}
	// --slurp wraps pages in an outer array; flatten them.
	var pages []json.RawMessage
	if err := json.Unmarshal(out, &pages); err != nil {
		return err
	}
	var items []json.RawMessage
	for _, p := range pages {
		var page []json.RawMessage
		if err := json.Unmarshal(p, &page); err != nil {
			// A non-paginated response is already the item list.
			return json.Unmarshal(out, v)
		}
		items = append(items, page...)
	}
	merged, err := json.Marshal(items)
	if err != nil {
		return err
	}
	return json.Unmarshal(merged, v)
}

// Created is an issue or PR created by the current account.
type Created struct {
	Number    int
	IsPR      bool
	Labels    []Label
	Assignees []Author
	Milestone *MilestoneRef
}

// MilestoneTitle returns the milestone title or "".
func (c Created) MilestoneTitle() string {
	if c.Milestone == nil {
		return ""
	}
	return c.Milestone.Title
}

// ListCreatedSince returns issues and PRs authored by the gh user at or
// after t, regardless of labels. Used to make sure everything a session
// created stays visible to the factory.
func (c *Client) ListCreatedSince(ctx context.Context, t time.Time) ([]Created, error) {
	search := fmt.Sprintf("author:@me created:>=%s", t.UTC().Add(-time.Minute).Format("2006-01-02T15:04:05Z"))
	var out []Created
	issuesOut, err := c.Exec(ctx, "issue", "list", "-R", c.Repo, "--state", "all", "--search", search, "--limit", "50", "--json", "number,labels,assignees,milestone,createdAt")
	if err != nil {
		return nil, err
	}
	var issues []Issue
	if err := json.Unmarshal(issuesOut, &issues); err != nil {
		return nil, err
	}
	for _, i := range issues {
		if !i.CreatedAt.Before(t.Add(-time.Minute)) {
			out = append(out, Created{Number: i.Number, Labels: i.Labels, Assignees: i.Assignees, Milestone: i.Milestone})
		}
	}
	prsOut, err := c.Exec(ctx, "pr", "list", "-R", c.Repo, "--state", "all", "--search", search, "--limit", "50", "--json", "number,labels,assignees,milestone,createdAt")
	if err != nil {
		return nil, err
	}
	var prs []PR
	if err := json.Unmarshal(prsOut, &prs); err != nil {
		return nil, err
	}
	for _, p := range prs {
		if !p.CreatedAt.Before(t.Add(-time.Minute)) {
			out = append(out, Created{Number: p.Number, IsPR: true, Labels: p.Labels, Assignees: p.Assignees, Milestone: p.Milestone})
		}
	}
	return out, nil
}

// SubIssueSummary is GitHub's progress summary of an issue's sub-issues.
type SubIssueSummary struct {
	Total            int `json:"total"`
	Completed        int `json:"completed"`
	PercentCompleted int `json:"percent_completed"`
}

// IssueDetails are REST fields not exposed by `gh issue view --json`.
type IssueDetails struct {
	ID        int64           `json:"id"` // database id, needed for sub-issue calls
	SubIssues SubIssueSummary `json:"sub_issues_summary"`
	Milestone *MilestoneRef   `json:"milestone"`
}

// MilestoneTitle returns the milestone title or "".
func (d IssueDetails) MilestoneTitle() string {
	if d.Milestone == nil {
		return ""
	}
	return d.Milestone.Title
}

// GetIssueDetails fetches an issue through the REST API.
func (c *Client) GetIssueDetails(ctx context.Context, number int) (IssueDetails, error) {
	out, err := c.Exec(ctx, "api", fmt.Sprintf("repos/%s/issues/%d", c.Repo, number))
	if err != nil {
		return IssueDetails{}, err
	}
	var d IssueDetails
	return d, json.Unmarshal(out, &d)
}

// Parent is the parent issue of a sub-issue.
type Parent struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

// ParentIssue returns the parent of a sub-issue, or nil when it has none.
func (c *Client) ParentIssue(ctx context.Context, number int) (*Parent, error) {
	owner, name, _ := strings.Cut(c.Repo, "/")
	query := `query($owner:String!,$name:String!,$number:Int!){repository(owner:$owner,name:$name){issue(number:$number){parent{number title}}}}`
	out, err := c.Exec(ctx, "api", "graphql", "-f", "query="+query, "-F", "owner="+owner, "-F", "name="+name, "-F", fmt.Sprintf("number=%d", number))
	if err != nil {
		return nil, err
	}
	var resp struct {
		Data struct {
			Repository struct {
				Issue struct {
					Parent *Parent `json:"parent"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return nil, err
	}
	return resp.Data.Repository.Issue.Parent, nil
}

// AddSubIssue attaches child to parent as a sub-issue. childID is the
// child's database id (IssueDetails.ID).
func (c *Client) AddSubIssue(ctx context.Context, parent int, childID int64) error {
	_, err := c.Exec(ctx, "api", "--method", "POST", fmt.Sprintf("repos/%s/issues/%d/sub_issues", c.Repo, parent), "-F", fmt.Sprintf("sub_issue_id=%d", childID))
	return err
}

// NewIssue describes an issue to create.
type NewIssue struct {
	Title     string
	Body      string
	Labels    []string
	Assignees []string
	Milestone string
}

// CreateIssue creates an issue and returns its number.
func (c *Client) CreateIssue(ctx context.Context, n NewIssue) (int, error) {
	args := []string{"issue", "create", "-R", c.Repo, "--title", n.Title, "--body", n.Body}
	for _, l := range n.Labels {
		args = append(args, "--label", l)
	}
	for _, a := range n.Assignees {
		args = append(args, "--assignee", a)
	}
	if n.Milestone != "" {
		args = append(args, "--milestone", n.Milestone)
	}
	out, err := c.Exec(ctx, args...)
	if err != nil {
		return 0, err
	}
	// gh prints the new issue's URL.
	url := strings.TrimSpace(string(out))
	if i := strings.LastIndex(url, "/"); i >= 0 {
		if num, err := strconv.Atoi(url[i+1:]); err == nil {
			return num, nil
		}
	}
	return 0, fmt.Errorf("could not parse issue number from %q", url)
}
