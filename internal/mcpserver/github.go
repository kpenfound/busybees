package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// GitHub is the backend behind the GitHub tools: the `gh` calls a role used
// to build itself, plus the factory's own rules (the visibility filter and
// the label set) so the tools can enforce them.
//
// The rules are enforced here rather than in the implementation, so that
// what "matches the filter" or "belongs to the product manager" means is one
// piece of code covered by one set of tests.
type GitHub interface {
	// Rules returns the factory's visibility filter and label set.
	Rules(ctx context.Context) (github.Query, config.Labels, error)
	// ActsAs returns the GitHub login the factory acts as ([github].login),
	// or "" when the factory shares an account with the people it works for.
	ActsAs(ctx context.Context) (string, error)
	Issue(ctx context.Context, number int) (github.Issue, error)
	Parent(ctx context.Context, number int) (*github.Parent, error)
	PR(ctx context.Context, number int) (github.PR, error)
	PRActivity(ctx context.Context, number int, since time.Time) ([]github.Activity, error)
	Checks(ctx context.Context, number int) ([]github.Check, error)
	Comment(ctx context.Context, number int, body string) error
	EditBody(ctx context.Context, number int, body string) error
	EditLabels(ctx context.Context, number int, add, remove []string) error
	// SubmitReview submits one review on a pull request: event is one of
	// github.ReviewEvents.
	SubmitReview(ctx context.Context, number int, event, body string) error
}

// errNoGitHub is what every GitHub tool reports when there is no backend,
// the same shape the issue tools use: the server still starts, and the
// failure surfaces from the tool that needs it.
var errNoGitHub = errors.New("GitHub is unavailable: bees.toml could not be loaded")

// states are the moves issue_set_state offers, as short names. They are the
// only two transitions a role makes itself; the orchestrator owns the rest.
var states = []string{"ready", "blocked"}

func (s *server) addGitHubTools(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "issue_view",
		Title: "Read an issue",
		Description: "Read one issue in full: its labels, milestone, parent feature, body and " +
			"every comment, oldest first, marked as written by a bee or by a person. Defaults " +
			"to the issue this session is working on. Issues outside the factory's filter are refused.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schemaFor[issueViewInput](nil),
	}, s.issueView)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "pr_view",
		Title: "Read a pull request",
		Description: "Read one pull request: title, branches, body, the state of its required " +
			"checks and every review and comment a person left on it. Defaults to the pull " +
			"request this session is working on. Pull requests outside the factory's filter are refused.",
		Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true},
		InputSchema: schemaFor[prViewInput](nil),
	}, s.prView)

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "comment",
		Title: "Comment on an issue or pull request",
		Description: "Post a comment on an issue or pull request. Comments on GitHub are for " +
			"people: use `mail_send` to talk to another role. The marker that tells your " +
			"comments apart from a person's is appended for you.",
		InputSchema: schemaFor[commentInput](nil),
	}, s.comment)

	if s.roleIs(config.RoleProductManager, config.RoleProjectManager) {
		mcp.AddTool(srv, &mcp.Tool{
			Name:  "issue_edit_body",
			Title: "Rewrite an issue body",
			Description: "Replace the body of an issue with a new one. Feature and feedback " +
				"issues belong to the product manager; nobody else may rewrite them.",
			InputSchema: schemaFor[issueEditBodyInput](nil),
		}, s.issueEditBody)
	}

	if s.roleIs(config.RoleProjectManager) {
		mcp.AddTool(srv, &mcp.Tool{
			Name:  "issue_set_state",
			Title: "Move a work item out of triage",
			Description: "Move a refined work item from triage to ready (with its size, which " +
				"is required) or to blocked, in one edit. Only an issue in triage can be moved; " +
				"the orchestrator owns every other transition.",
			InputSchema: schemaFor[issueSetStateInput](map[string][]string{
				"state": states,
				"size":  config.Sizes,
			}),
		}, s.issueSetState)
	}

	if s.roleIs(config.RoleReviewer) {
		mcp.AddTool(srv, &mcp.Tool{
			Name:  "submit_review",
			Title: "Submit a review on a pull request",
			Description: "Submit one GitHub review on a pull request a person asked the factory to " +
				"review: approve, request-changes or comment, with the whole verdict in the body. " +
				"Only for a requested review; a developer's pull request gets its feedback by " +
				"mail. Defaults to the pull request this session is working on. The marker that " +
				"tells your reviews apart from a person's is appended for you.",
			InputSchema: schemaFor[submitReviewInput](map[string][]string{
				"event": github.ReviewEvents,
			}),
		}, s.submitReview)
	}

	if s.roleIs(config.RoleProductManager) {
		mcp.AddTool(srv, &mcp.Tool{
			Name:  "issue_question",
			Title: "Wait for a person to answer",
			Description: "Mark a feature or feedback issue as waiting for a person to answer, " +
				"or clear that once they have. Post the question itself as a comment first.",
			InputSchema: schemaFor[issueQuestionInput](nil),
		}, s.issueQuestion)
	}
}

// roleIs reports whether this session's role is one of want. An empty or
// unknown role — someone running `bees mcp serve` by hand — gets every tool,
// exactly as it gets every outcome in done.
func (s *server) roleIs(want ...string) bool {
	if !slices.Contains(config.Roles, s.env.Role) {
		return true
	}
	return slices.Contains(want, s.env.Role)
}

// ---- issue_view ------------------------------------------------------------

type issueViewInput struct {
	Number int `json:"number,omitempty" jsonschema:"issue to read (defaults to the issue this session is working on)"`
}

func (s *server) issueView(ctx context.Context, _ *mcp.CallToolRequest, in issueViewInput) (*mcp.CallToolResult, any, error) {
	n, err := s.issueNumber(in.Number)
	if err != nil {
		return nil, nil, err
	}
	issue, labels, err := s.visibleIssue(ctx, n)
	if err != nil {
		return nil, nil, err
	}
	// The parent is context, not the answer: a repository or a token without
	// sub-issues must still be able to read the issue.
	parent, _ := s.github.Parent(ctx, n)
	// So is the login the factory acts as: it only widens what counts as a
	// bee's comment, and "" is the marker-only rule every configuration
	// without [github] uses anyway.
	actsAs, _ := s.github.ActsAs(ctx)
	return text("%s", issueText(issue, parent, labels, actsAs)), nil, nil
}

// ---- pr_view ---------------------------------------------------------------

type prViewInput struct {
	Number int `json:"number,omitempty" jsonschema:"pull request to read (defaults to the pull request this session is working on)"`
}

func (s *server) prView(ctx context.Context, _ *mcp.CallToolRequest, in prViewInput) (*mcp.CallToolResult, any, error) {
	if s.github == nil {
		return nil, nil, errNoGitHub
	}
	n, pr, err := s.visiblePR(ctx, in.Number)
	if err != nil {
		return nil, nil, err
	}
	checks, err := s.github.Checks(ctx, n)
	if err != nil {
		return nil, nil, err
	}
	activity, err := s.github.PRActivity(ctx, n, time.Time{})
	if err != nil {
		return nil, nil, err
	}
	return text("%s", prText(pr, checks, activity)), nil, nil
}

// visiblePR resolves a pull request number (0 means the one this session
// is working on) to the pull request, refusing one outside the filter.
func (s *server) visiblePR(ctx context.Context, number int) (int, github.PR, error) {
	n := number
	if n == 0 {
		n = s.env.PR
	}
	if n == 0 {
		return 0, github.PR{}, errors.New("no pull request number: this session is not working on a pull request")
	}
	q, _, err := s.github.Rules(ctx)
	if err != nil {
		return 0, github.PR{}, err
	}
	pr, err := s.github.PR(ctx, n)
	if err != nil {
		return 0, github.PR{}, err
	}
	if !q.Matches(pr.Labels, pr.Assignees, prMilestone(pr)) {
		return 0, github.PR{}, outsideFilter(n, q)
	}
	return n, pr, nil
}

// ---- comment ---------------------------------------------------------------

type commentInput struct {
	Number int    `json:"number" jsonschema:"issue or pull request to comment on"`
	Body   string `json:"body" jsonschema:"the comment, markdown; written for a person to read"`
}

func (s *server) comment(ctx context.Context, _ *mcp.CallToolRequest, in commentInput) (*mcp.CallToolResult, any, error) {
	if s.github == nil {
		return nil, nil, errNoGitHub
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, nil, errors.New("a comment needs a body")
	}
	if s.env.Role == "" {
		return nil, nil, errors.New("comments can only be posted from a session ($BEES_ROLE is not set)")
	}
	if err := s.visibleItem(ctx, in.Number); err != nil {
		return nil, nil, err
	}
	if err := s.github.Comment(ctx, in.Number, withMarker(in.Body, s.env.Role)); err != nil {
		return nil, nil, err
	}
	return text("commented on #%d", in.Number), nil, nil
}

// withMarker appends the role's comment marker, so a bee comment is always
// distinguishable from a person's — humans and bees share one GitHub
// account. Only a body github.BeeRole already reads as *this role's* is left
// alone, which is the same rule the readers apply: a body quoting another
// role's comment carries that role's marker, and a body whose last line is a
// quoted marker (`> <!-- bees:... -->`) carries nobody's, so suppressing the
// append in either case would hand the comment to the wrong author.
func withMarker(body, role string) string {
	trimmed := strings.TrimRight(body, "\n")
	if got, ok := github.BeeRole(trimmed); ok && got == role {
		return trimmed
	}
	return trimmed + "\n\n" + roleMarker(role)
}

// roleMarker is the marker a role's comments carry.
func roleMarker(role string) string {
	return fmt.Sprintf("%s%s -->", github.BeesMarker, role)
}

// ---- issue_edit_body -------------------------------------------------------

type issueEditBodyInput struct {
	Number int    `json:"number" jsonschema:"issue to rewrite"`
	Body   string `json:"body" jsonschema:"the new body, markdown; it replaces the old one entirely, so include everything worth keeping"`
}

func (s *server) issueEditBody(ctx context.Context, _ *mcp.CallToolRequest, in issueEditBodyInput) (*mcp.CallToolResult, any, error) {
	if s.github == nil {
		return nil, nil, errNoGitHub
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, nil, errors.New("an issue needs a body")
	}
	issue, labels, err := s.visibleIssue(ctx, in.Number)
	if err != nil {
		return nil, nil, err
	}
	if kind := ownedByProductManager(issue, labels); kind != "" && !s.roleIs(config.RoleProductManager) {
		return nil, nil, fmt.Errorf("#%d is %s: feature and feedback issues belong to the product manager", in.Number, kind)
	}
	if err := s.github.EditBody(ctx, in.Number, in.Body); err != nil {
		return nil, nil, err
	}
	return text("rewrote the body of #%d", in.Number), nil, nil
}

// ownedByProductManager returns the label that makes an issue the product
// manager's, or "" for an ordinary work item.
func ownedByProductManager(i github.Issue, l config.Labels) string {
	for _, name := range []string{l.Feature, l.Feedback} {
		if github.HasLabel(i.Labels, name) {
			return name
		}
	}
	return ""
}

// ---- issue_set_state -------------------------------------------------------

type issueSetStateInput struct {
	Number int    `json:"number" jsonschema:"work item to move"`
	State  string `json:"state" jsonschema:"where the work item goes: ready once it is detailed enough to build, blocked while you wait for a product decision"`
	Size   string `json:"size,omitempty" jsonschema:"the work item's size, required for ready and ignored otherwise"`
}

func (s *server) issueSetState(ctx context.Context, _ *mcp.CallToolRequest, in issueSetStateInput) (*mcp.CallToolResult, any, error) {
	if s.github == nil {
		return nil, nil, errNoGitHub
	}
	issue, labels, err := s.visibleIssue(ctx, in.Number)
	if err != nil {
		return nil, nil, err
	}
	to := stateLabel(labels, in.State)
	if to == "" {
		return nil, nil, fmt.Errorf("unknown state %q (want %s)", in.State, strings.Join(states, " or "))
	}
	// Everything else on the state machine is the orchestrator's, so an
	// issue that is not in triage is a mistake worth naming.
	if now := stateOf(issue, labels); now != labels.Triage {
		return nil, nil, fmt.Errorf("#%d is %s, not %s: only an issue in triage is yours to move",
			in.Number, describeState(now), labels.Triage)
	}
	add := []string{to}
	remove := []string{labels.Triage}
	if in.State == "ready" {
		size := labels.SizeLabel(in.Size)
		if size == "" {
			return nil, nil, fmt.Errorf("moving #%d to %s needs a size (%s)", in.Number, labels.Ready, strings.Join(config.Sizes, ", "))
		}
		add = append(add, size)
		// Sizes are exclusive: whatever the issue was sized at goes.
		for _, l := range labels.SizeLabels() {
			if l != size && github.HasLabel(issue.Labels, l) {
				remove = append(remove, l)
			}
		}
	}
	if err := s.github.EditLabels(ctx, in.Number, add, remove); err != nil {
		return nil, nil, err
	}
	return text("#%d is now %s", in.Number, strings.Join(add, " + ")), nil, nil
}

// ---- submit_review ---------------------------------------------------------

type submitReviewInput struct {
	Number int    `json:"number,omitempty" jsonschema:"pull request to review (defaults to the pull request this session is working on)"`
	Event  string `json:"event" jsonschema:"the verdict: approve when every stage passed, request-changes when any failed, comment when you may not approve (the pull request's author is the login the factory acts as)"`
	Body   string `json:"body" jsonschema:"the review: each stage's verdict line and the points under it, in the stages' order"`
}

func (s *server) submitReview(ctx context.Context, _ *mcp.CallToolRequest, in submitReviewInput) (*mcp.CallToolResult, any, error) {
	if s.github == nil {
		return nil, nil, errNoGitHub
	}
	if strings.TrimSpace(in.Body) == "" {
		return nil, nil, errors.New("a review needs a body")
	}
	if !slices.Contains(github.ReviewEvents, in.Event) {
		return nil, nil, fmt.Errorf("unknown review event %q (want %s)", in.Event, strings.Join(github.ReviewEvents, ", "))
	}
	if s.env.Role == "" {
		return nil, nil, errors.New("reviews can only be submitted from a session ($BEES_ROLE is not set)")
	}
	number, _, err := s.visiblePR(ctx, in.Number)
	if err != nil {
		return nil, nil, err
	}
	if err := s.github.SubmitReview(ctx, number, in.Event, withMarker(in.Body, s.env.Role)); err != nil {
		return nil, nil, err
	}
	return text("submitted a %s review on #%d", in.Event, number), nil, nil
}

// stateLabel maps a short state name to its label, or "" when the name is
// not one a role may move an issue to.
func stateLabel(l config.Labels, state string) string {
	switch state {
	case "ready":
		return l.Ready
	case "blocked":
		return l.Blocked
	}
	return ""
}

// describeState names an issue's current state label for an error message.
func describeState(label string) string {
	if label == "" {
		return "on no state label"
	}
	return label
}

// ---- issue_question --------------------------------------------------------

type issueQuestionInput struct {
	Number  int  `json:"number" jsonschema:"feature or feedback issue"`
	Waiting bool `json:"waiting" jsonschema:"true once you have asked a person a question on the issue, false when they have answered"`
}

func (s *server) issueQuestion(ctx context.Context, _ *mcp.CallToolRequest, in issueQuestionInput) (*mcp.CallToolResult, any, error) {
	if s.github == nil {
		return nil, nil, errNoGitHub
	}
	issue, labels, err := s.visibleIssue(ctx, in.Number)
	if err != nil {
		return nil, nil, err
	}
	if ownedByProductManager(issue, labels) == "" {
		return nil, nil, fmt.Errorf("#%d is a work item, not a %s or %s issue: only those wait for a person",
			in.Number, labels.Feature, labels.Feedback)
	}
	has := github.HasLabel(issue.Labels, labels.Question)
	switch {
	case in.Waiting == has:
		return text("#%d already %s %s", in.Number, map[bool]string{true: "carries", false: "does not carry"}[has], labels.Question), nil, nil
	case in.Waiting:
		err = s.github.EditLabels(ctx, in.Number, []string{labels.Question}, nil)
	default:
		err = s.github.EditLabels(ctx, in.Number, nil, []string{labels.Question})
	}
	if err != nil {
		return nil, nil, err
	}
	if in.Waiting {
		return text("#%d now carries %s", in.Number, labels.Question), nil, nil
	}
	return text("removed %s from #%d", labels.Question, in.Number), nil, nil
}

// ---- shared rules ----------------------------------------------------------

func (s *server) issueNumber(n int) (int, error) {
	if n == 0 {
		n = s.env.Issue
	}
	if n == 0 {
		return 0, errors.New("no issue number: this session is not working on an issue")
	}
	return n, nil
}

// visibleIssue reads an issue and refuses it when it does not match the
// factory's filter: the factory never touches what a person did not hand it.
func (s *server) visibleIssue(ctx context.Context, number int) (github.Issue, config.Labels, error) {
	if s.github == nil {
		return github.Issue{}, config.Labels{}, errNoGitHub
	}
	q, labels, err := s.github.Rules(ctx)
	if err != nil {
		return github.Issue{}, config.Labels{}, err
	}
	i, err := s.github.Issue(ctx, number)
	if err != nil {
		return github.Issue{}, config.Labels{}, err
	}
	if !q.Matches(i.Labels, i.Assignees, i.MilestoneTitle()) {
		return github.Issue{}, config.Labels{}, outsideFilter(number, q)
	}
	return i, labels, nil
}

// visibleItem checks the filter for a number that may be an issue or a pull
// request: `gh issue comment` accepts both, and a role does not always know
// which it is holding.
func (s *server) visibleItem(ctx context.Context, number int) error {
	q, _, err := s.github.Rules(ctx)
	if err != nil {
		return err
	}
	if i, err := s.github.Issue(ctx, number); err == nil {
		if !q.Matches(i.Labels, i.Assignees, i.MilestoneTitle()) {
			return outsideFilter(number, q)
		}
		return nil
	}
	p, err := s.github.PR(ctx, number)
	if err != nil {
		return err
	}
	if !q.Matches(p.Labels, p.Assignees, prMilestone(p)) {
		return outsideFilter(number, q)
	}
	return nil
}

func outsideFilter(number int, q github.Query) error {
	return fmt.Errorf("#%d does not match the factory's filter (%s): the factory never touches it", number, describeQuery(q))
}

// describeQuery renders the visibility filter the way bees.toml states it.
func describeQuery(q github.Query) string {
	var parts []string
	for _, p := range [][2]string{{"label", q.Label}, {"assignee", q.Assignee}, {"milestone", q.Milestone}} {
		if p[1] != "" {
			parts = append(parts, p[0]+"="+p[1])
		}
	}
	if len(parts) == 0 {
		return "no criteria"
	}
	return strings.Join(parts, " and ")
}

func prMilestone(p github.PR) string {
	if p.Milestone == nil {
		return ""
	}
	return p.Milestone.Title
}

// stateOf returns the issue's workflow state label, or "".
func stateOf(i github.Issue, l config.Labels) string {
	for _, name := range l.StateLabels() {
		if github.HasLabel(i.Labels, name) {
			return name
		}
	}
	return ""
}
