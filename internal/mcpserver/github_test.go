package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// fakeGitHub is a GitHub backend that answers from memory and records every
// write, so a test can assert both what a tool did and — for a refusal —
// that it did nothing.
type fakeGitHub struct {
	q      github.Query
	labels config.Labels
	issues map[int]github.Issue
	prs    map[int]github.PR
	parent *github.Parent
	checks []github.Check
	acts   []github.Activity

	comments []numberedBody
	bodies   []numberedBody
	edits    []labelEdit
}

type numberedBody struct {
	number int
	body   string
}

type labelEdit struct {
	number      int
	add, remove []string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{
		q:      github.Query{Label: "bees", Assignee: "kyle"},
		labels: config.LabelsFor("bees"),
		issues: map[int]github.Issue{},
		prs:    map[int]github.PR{},
	}
}

// issue seeds a visible issue: it carries the filter's label and assignee.
func (f *fakeGitHub) issue(number int, labels ...string) *fakeGitHub {
	f.issues[number] = github.Issue{
		Number:    number,
		Title:     fmt.Sprintf("Issue %d", number),
		Body:      "the body",
		Labels:    labelsOf(append([]string{"bees"}, labels...)...),
		Assignees: []github.Author{{Login: "kyle"}},
	}
	return f
}

func labelsOf(names ...string) []github.Label {
	out := make([]github.Label, 0, len(names))
	for _, n := range names {
		out = append(out, github.Label{Name: n})
	}
	return out
}

func (f *fakeGitHub) Rules(context.Context) (github.Query, config.Labels, error) {
	return f.q, f.labels, nil
}

func (f *fakeGitHub) Issue(_ context.Context, number int) (github.Issue, error) {
	i, ok := f.issues[number]
	if !ok {
		return github.Issue{}, fmt.Errorf("no issue #%d", number)
	}
	return i, nil
}

func (f *fakeGitHub) Parent(context.Context, int) (*github.Parent, error) { return f.parent, nil }

func (f *fakeGitHub) PR(_ context.Context, number int) (github.PR, error) {
	p, ok := f.prs[number]
	if !ok {
		return github.PR{}, fmt.Errorf("no pull request #%d", number)
	}
	return p, nil
}

func (f *fakeGitHub) PRActivity(_ context.Context, _ int, _ time.Time) ([]github.Activity, error) {
	return f.acts, nil
}

func (f *fakeGitHub) Checks(context.Context, int) ([]github.Check, error) { return f.checks, nil }

func (f *fakeGitHub) Comment(_ context.Context, number int, body string) error {
	f.comments = append(f.comments, numberedBody{number, body})
	return nil
}

func (f *fakeGitHub) EditBody(_ context.Context, number int, body string) error {
	f.bodies = append(f.bodies, numberedBody{number, body})
	return nil
}

func (f *fakeGitHub) EditLabels(_ context.Context, number int, add, remove []string) error {
	f.edits = append(f.edits, labelEdit{number: number, add: add, remove: remove})
	return nil
}

// wrote reports whether the backend was asked to change anything.
func (f *fakeGitHub) wrote() bool {
	return len(f.comments)+len(f.bodies)+len(f.edits) > 0
}

// ---- tool lists ------------------------------------------------------------

// TestGitHubToolsPerRole is the enforcement a role cannot argue with: a
// developer is never offered the project manager's state moves.
func TestGitHubToolsPerRole(t *testing.T) {
	base := "comment, done, issue_create, issue_link, issue_view, mail_list, mail_send, pr_view"
	for role, want := range map[string]string{
		config.RoleDeveloper: base,
		config.RoleReviewer:  base,
		config.RoleQA:        base,
		config.RoleProjectManager: "comment, done, issue_create, issue_edit_body, issue_link, " +
			"issue_set_state, issue_view, mail_list, mail_send, pr_view",
		config.RoleProductManager: "comment, done, issue_create, issue_edit_body, issue_link, " +
			"issue_question, issue_view, mail_list, mail_send, pr_view",
		// Hand use through `bees mcp serve`: everything.
		"": "comment, done, issue_create, issue_edit_body, issue_link, issue_question, " +
			"issue_set_state, issue_view, mail_list, mail_send, pr_view",
	} {
		list, err := Tools(context.Background(), Env{Role: role})
		if err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, tool := range list {
			names = append(names, tool.Name)
		}
		if got := strings.Join(names, ", "); got != want {
			t.Errorf("role %q:\n got %s\nwant %s", role, got, want)
		}
	}
}

func TestGitHubToolsWithoutABackend(t *testing.T) {
	h := newHarness(t, "", Deps{})
	for name, args := range map[string]map[string]any{
		"issue_view":      {"number": 12},
		"pr_view":         {"number": 12},
		"comment":         {"number": 12, "body": "hello"},
		"issue_edit_body": {"number": 12, "body": "hello"},
		"issue_set_state": {"number": 12, "state": "blocked"},
		"issue_question":  {"number": 12, "waiting": true},
	} {
		res := h.callRaw(name, args)
		if !res.IsError || !strings.Contains(resultText(res), "bees.toml") {
			t.Errorf("%s: %v %q", name, res.IsError, resultText(res))
		}
	}
}

// ---- reads -----------------------------------------------------------------

func TestIssueViewRendersTheWholeIssue(t *testing.T) {
	f := newFakeGitHub().issue(36, "bees:in-progress", "bees:size/s", "bees:bug")
	i := f.issues[36]
	i.Title = "Crash on empty input"
	i.Body = "steps to reproduce"
	i.Milestone = &struct {
		Title string `json:"title"`
	}{Title: "v0.1.0"}
	i.Comments = []github.Comment{
		{Author: github.Author{Login: "kpenfound"}, Body: "Which format?", CreatedAt: time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)},
		{Author: github.Author{Login: "kpenfound"}, Body: "JSON.\n\n<!-- bees:project_manager -->", CreatedAt: time.Date(2026, 8, 30, 10, 30, 0, 0, time.UTC)},
	}
	f.issues[36] = i
	f.parent = &github.Parent{Number: 72, Title: "Roles act through built-in tools"}

	h := newHarness(t, config.RoleDeveloper, Deps{GitHub: f})
	// No number: the issue this session is working on.
	got := h.call("issue_view", nil)
	want := `#36 Crash on empty input
state: in-progress · size: s · kind: bug
milestone: v0.1.0
parent: #72 Roles act through built-in tools

steps to reproduce

comments (2):

kpenfound (human) · 2026-08-30 09:00
Which format?

kpenfound (bee: project_manager) · 2026-08-30 10:30
JSON.

<!-- bees:project_manager -->
`
	if got != want {
		t.Fatalf("issue_view =\n%s\nwant\n%s", got, want)
	}
}

func TestIssueViewRefusesAnIssueOutsideTheFilter(t *testing.T) {
	f := newFakeGitHub()
	// No `bees` label: a person's own issue, none of the factory's business.
	f.issues[12] = github.Issue{Number: 12, Title: "Private", Assignees: []github.Author{{Login: "kyle"}}}
	h := newHarness(t, config.RoleDeveloper, Deps{GitHub: f})
	res := h.callRaw("issue_view", map[string]any{"number": 12})
	if !res.IsError || !strings.Contains(resultText(res), "does not match the factory's filter") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
	if !strings.Contains(resultText(res), "label=bees and assignee=kyle") {
		t.Fatalf("the filter is not named: %q", resultText(res))
	}
}

func TestPRViewRendersChecksAndHumanActivity(t *testing.T) {
	f := newFakeGitHub()
	f.prs[72] = github.PR{
		Number: 72, Title: "Add the GitHub tools", Body: "Closes #59",
		HeadRefName: "bees/issue-59", BaseRefName: "main", IsDraft: true,
		Labels: labelsOf("bees"), Assignees: []github.Author{{Login: "kyle"}},
	}
	f.checks = []github.Check{{Name: "go", Bucket: "pass"}, {Name: "dagger", Bucket: "fail"}}
	f.acts = []github.Activity{
		{Kind: "review-comment", Author: "kpenfound", Body: "rename this", Path: "internal/mcpserver/github.go", Line: 12},
		{Kind: "review", Author: "kpenfound", Body: "one more round", State: "CHANGES_REQUESTED"},
	}
	h := newHarness(t, config.RoleDeveloper, Deps{GitHub: f})
	// No number: the pull request this session is working on.
	got := h.call("pr_view", nil)
	want := `#72 Add the GitHub tools
bees/issue-59 → main (draft)
checks: failed (dagger)

Closes #59

from people (2):

review-comment · kpenfound · internal/mcpserver/github.go:12
rename this

review · kpenfound · changes_requested
one more round
`
	if got != want {
		t.Fatalf("pr_view =\n%s\nwant\n%s", got, want)
	}
}

func TestPRViewRefusesAPROutsideTheFilter(t *testing.T) {
	f := newFakeGitHub()
	f.prs[72] = github.PR{Number: 72, Assignees: []github.Author{{Login: "kyle"}}}
	h := newHarness(t, config.RoleDeveloper, Deps{GitHub: f})
	res := h.callRaw("pr_view", map[string]any{"number": 72})
	if !res.IsError || !strings.Contains(resultText(res), "does not match the factory's filter") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
}

// ---- comment ---------------------------------------------------------------

func TestCommentAlwaysEndsWithTheRolesMarker(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"plain", "Filed #90.", "Filed #90.\n\n<!-- bees:product_manager -->"},
		{"trailing newlines", "Filed #90.\n\n", "Filed #90.\n\n<!-- bees:product_manager -->"},
		{"already marked", "Filed #90.\n\n<!-- bees:product_manager -->", "Filed #90.\n\n<!-- bees:product_manager -->"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitHub().issue(36, "bees:feedback")
			h := newHarness(t, config.RoleProductManager, Deps{GitHub: f})
			if got := h.call("comment", map[string]any{"number": 36, "body": tc.body}); got != "commented on #36" {
				t.Fatalf("result: %q", got)
			}
			if len(f.comments) != 1 || f.comments[0].body != tc.want {
				t.Fatalf("comment = %q, want %q", f.comments, tc.want)
			}
			if n := strings.Count(f.comments[0].body, github.BeesMarker); n != 1 {
				t.Fatalf("marker appears %d times: %q", n, f.comments[0].body)
			}
		})
	}
}

func TestCommentOnAPullRequest(t *testing.T) {
	f := newFakeGitHub()
	f.prs[72] = github.PR{Number: 72, Labels: labelsOf("bees"), Assignees: []github.Author{{Login: "kyle"}}}
	h := newHarness(t, config.RoleDeveloper, Deps{GitHub: f})
	h.call("comment", map[string]any{"number": 72, "body": "Addressed in the last push."})
	if len(f.comments) != 1 || !strings.HasSuffix(f.comments[0].body, "<!-- bees:developer -->") {
		t.Fatalf("comments: %v", f.comments)
	}
}

func TestCommentRefusesOutsideTheFilterAndWithoutABody(t *testing.T) {
	f := newFakeGitHub()
	f.issues[12] = github.Issue{Number: 12, Labels: labelsOf("bees")} // not assigned to kyle
	f.issues[36] = github.Issue{Number: 36, Labels: labelsOf("bees"), Assignees: []github.Author{{Login: "kyle"}}}
	h := newHarness(t, config.RoleDeveloper, Deps{GitHub: f})

	res := h.callRaw("comment", map[string]any{"number": 12, "body": "hello"})
	if !res.IsError || !strings.Contains(resultText(res), "does not match the factory's filter") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
	res = h.callRaw("comment", map[string]any{"number": 36, "body": "   \n"})
	if !res.IsError || !strings.Contains(resultText(res), "needs a body") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
	if f.wrote() {
		t.Fatalf("a refused comment was posted: %v", f.comments)
	}
}

// ---- issue_edit_body -------------------------------------------------------

func TestIssueEditBodyOnAFeatureIssue(t *testing.T) {
	// The project manager refines work items; feature and feedback issues
	// are the product manager's.
	f := newFakeGitHub().issue(72, "bees:feature")
	h := newHarness(t, config.RoleProjectManager, Deps{GitHub: f})
	res := h.callRaw("issue_edit_body", map[string]any{"number": 72, "body": "rewritten"})
	if !res.IsError || !strings.Contains(resultText(res), "belong to the product manager") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
	if f.wrote() {
		t.Fatalf("the body was rewritten anyway: %v", f.bodies)
	}

	// The product manager may.
	pm := newHarness(t, config.RoleProductManager, Deps{GitHub: f})
	if got := pm.call("issue_edit_body", map[string]any{"number": 72, "body": "rewritten"}); got != "rewrote the body of #72" {
		t.Fatalf("result: %q", got)
	}
	if len(f.bodies) != 1 || f.bodies[0] != (numberedBody{72, "rewritten"}) {
		t.Fatalf("bodies: %v", f.bodies)
	}
}

func TestIssueEditBodyOnAWorkItem(t *testing.T) {
	f := newFakeGitHub().issue(59, "bees:triage")
	h := newHarness(t, config.RoleProjectManager, Deps{GitHub: f})
	h.call("issue_edit_body", map[string]any{"number": 59, "body": "## Scope\n\nrewritten"})
	if len(f.bodies) != 1 || f.bodies[0].body != "## Scope\n\nrewritten" {
		t.Fatalf("bodies: %v", f.bodies)
	}
	// An empty body would silently delete the issue's contents.
	res := h.callRaw("issue_edit_body", map[string]any{"number": 59, "body": ""})
	if !res.IsError || len(f.bodies) != 1 {
		t.Fatalf("result: %v %q, bodies %v", res.IsError, resultText(res), f.bodies)
	}
}

// ---- issue_set_state -------------------------------------------------------

func TestIssueSetStateReadyWithASize(t *testing.T) {
	// The issue was pre-sized m by the product manager; the project manager
	// has read the code and makes it s.
	f := newFakeGitHub().issue(59, "bees:triage", "bees:size/m")
	h := newHarness(t, config.RoleProjectManager, Deps{GitHub: f})
	got := h.call("issue_set_state", map[string]any{"number": 59, "state": "ready", "size": "s"})
	if got != "#59 is now bees:ready + bees:size/s" {
		t.Fatalf("result: %q", got)
	}
	// One edit, or the issue is briefly in a state no rule describes.
	if len(f.edits) != 1 {
		t.Fatalf("edits: %v", f.edits)
	}
	e := f.edits[0]
	if strings.Join(e.add, ",") != "bees:ready,bees:size/s" {
		t.Errorf("add = %v", e.add)
	}
	if strings.Join(e.remove, ",") != "bees:triage,bees:size/m" {
		t.Errorf("remove = %v", e.remove)
	}
}

func TestIssueSetStateBlockedLeavesTheSizeAlone(t *testing.T) {
	f := newFakeGitHub().issue(59, "bees:triage", "bees:size/m")
	h := newHarness(t, config.RoleProjectManager, Deps{GitHub: f})
	h.call("issue_set_state", map[string]any{"number": 59, "state": "blocked", "size": "l"})
	if len(f.edits) != 1 {
		t.Fatalf("edits: %v", f.edits)
	}
	if strings.Join(f.edits[0].add, ",") != "bees:blocked" || strings.Join(f.edits[0].remove, ",") != "bees:triage" {
		t.Fatalf("edit: %+v", f.edits[0])
	}
}

func TestIssueSetStateRefusals(t *testing.T) {
	f := newFakeGitHub().issue(59, "bees:ready", "bees:size/s").issue(60, "bees:triage")
	h := newHarness(t, config.RoleProjectManager, Deps{GitHub: f})

	// Not in triage: everything else on the state machine is the
	// orchestrator's, and the error says where the issue actually is.
	res := h.callRaw("issue_set_state", map[string]any{"number": 59, "state": "ready", "size": "s"})
	if !res.IsError || !strings.Contains(resultText(res), "#59 is bees:ready, not bees:triage") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
	// Ready without a size: the orchestrator would guess m.
	res = h.callRaw("issue_set_state", map[string]any{"number": 60, "state": "ready"})
	if !res.IsError || !strings.Contains(resultText(res), "needs a size") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
	if f.wrote() {
		t.Fatalf("a refused move edited labels: %v", f.edits)
	}
}

// ---- issue_question --------------------------------------------------------

func TestIssueQuestion(t *testing.T) {
	f := newFakeGitHub().issue(72, "bees:feature").issue(80, "bees:feedback", "bees:question").issue(59, "bees:triage")
	h := newHarness(t, config.RoleProductManager, Deps{GitHub: f})

	h.call("issue_question", map[string]any{"number": 72, "waiting": true})
	h.call("issue_question", map[string]any{"number": 80, "waiting": false})
	if len(f.edits) != 2 {
		t.Fatalf("edits: %v", f.edits)
	}
	if strings.Join(f.edits[0].add, ",") != "bees:question" || f.edits[0].remove != nil {
		t.Errorf("adding: %+v", f.edits[0])
	}
	if f.edits[1].add != nil || strings.Join(f.edits[1].remove, ",") != "bees:question" {
		t.Errorf("removing: %+v", f.edits[1])
	}
	// Already in the wanted shape: nothing to do, and no gh call. (The
	// fake still reports #72 without the label it was just given.)
	h.call("issue_question", map[string]any{"number": 72, "waiting": false})
	if len(f.edits) != 2 {
		t.Fatalf("removing a label the issue does not carry should be an edit-free no-op: %v", f.edits)
	}

	// A work item never waits for a person: that is what bees:blocked and
	// the mailbox are for.
	res := h.callRaw("issue_question", map[string]any{"number": 59, "waiting": true})
	if !res.IsError || !strings.Contains(resultText(res), "work item") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
}

// ---- helpers ---------------------------------------------------------------

func TestDescribeQuery(t *testing.T) {
	for _, tc := range []struct {
		q    github.Query
		want string
	}{
		{github.Query{Label: "bees", Assignee: "kyle"}, "label=bees and assignee=kyle"},
		{github.Query{Assignee: "kyle", Milestone: "v1"}, "assignee=kyle and milestone=v1"},
		{github.Query{}, "no criteria"},
	} {
		if got := describeQuery(tc.q); got != tc.want {
			t.Errorf("describeQuery(%+v) = %q, want %q", tc.q, got, tc.want)
		}
	}
}

func TestAuthorReadsTheMarker(t *testing.T) {
	for body, want := range map[string]string{
		"plain text":                        "human",
		"hi\n\n<!-- bees:developer -->":     "bee: developer",
		"hi\n\n<!-- bees:product_manager >": "human", // unterminated: not a marker
	} {
		if got := author(body); got != want {
			t.Errorf("author(%q) = %q, want %q", body, got, want)
		}
	}
}

// errRules is a backend whose configuration cannot be read.
type errRules struct{ *fakeGitHub }

func (errRules) Rules(context.Context) (github.Query, config.Labels, error) {
	return github.Query{}, config.Labels{}, errors.New("bees.toml: no such file")
}

func TestToolsReportAFailingConfiguration(t *testing.T) {
	h := newHarness(t, config.RoleDeveloper, Deps{GitHub: errRules{newFakeGitHub()}})
	res := h.callRaw("issue_view", map[string]any{"number": 36})
	if !res.IsError || !strings.Contains(resultText(res), "no such file") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
}
