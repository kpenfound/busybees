package issues

import (
	"context"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

func fake(t *testing.T, parentMilestone string) (*github.Client, *[]string) {
	t.Helper()
	return fakeWith(t, parentMilestone, "")
}

// fakeWith is fake with a milestone on the child (#77) as well as the parent.
func fakeWith(t *testing.T, parentMilestone, childMilestone string) (*github.Client, *[]string) {
	t.Helper()
	var calls []string
	c := github.New("acme/widgets")
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		calls = append(calls, call)
		switch {
		case strings.HasPrefix(call, "api repos/acme/widgets/issues/12"):
			ms := "null"
			if parentMilestone != "" {
				ms = `{"title":"` + parentMilestone + `"}`
			}
			return []byte(`{"id": 9001, "milestone": ` + ms + `, "sub_issues_summary": {"total": 2, "completed": 1}}`), nil
		case strings.HasPrefix(call, "api repos/acme/widgets/issues/13"):
			// A proposal: a feature issue a bee wrote, not approved yet.
			return []byte(`{"id": 9013, "milestone": {"title":"v1"}, "labels": [{"name":"bees"},{"name":"bees:feature"},{"name":"bees:proposal"}]}`), nil
		case strings.HasPrefix(call, "api repos/acme/widgets/issues/14"):
			// In planning: a person is still agreeing it with the product
			// manager, so nothing is broken down from it yet.
			return []byte(`{"id": 9014, "milestone": {"title":"v1"}, "labels": [{"name":"bees"},{"name":"bees:feature"},{"name":"bees:planning"}]}`), nil
		case strings.HasPrefix(call, "api repos/acme/widgets/issues/77"):
			ms := "null"
			if childMilestone != "" {
				ms = `{"title":"` + childMilestone + `"}`
			}
			return []byte(`{"id": 9077, "milestone": ` + ms + `}`), nil
		case strings.HasPrefix(call, "api repos/acme/widgets/milestones"):
			return []byte(`[{"number": 3, "title": "v1"}, {"number": 4, "title": "v2"}]`), nil
		case strings.HasPrefix(call, "api --method PATCH repos/acme/widgets/issues/77"):
			return []byte(`{}`), nil
		case strings.HasPrefix(call, "issue create"):
			return []byte("https://github.com/acme/widgets/issues/77\n"), nil
		case strings.HasPrefix(call, "api --method POST repos/acme/widgets/issues/12/sub_issues"),
			strings.HasPrefix(call, "api --method POST repos/acme/widgets/issues/14/sub_issues"):
			return []byte(`{}`), nil
		}
		t.Fatalf("unexpected call: %s", call)
		return nil, nil
	}
	return c, &calls
}

func TestCreateChild(t *testing.T) {
	gh, calls := fake(t, "v1")
	filter := config.Filter{Label: "bees", Assignee: "kyle"}
	labels := config.LabelsFor("bees")
	res, err := Create(context.Background(), gh, filter, labels, Options{Title: "Add export", Body: "b", Kind: KindTask, Parent: 12})
	if err != nil {
		t.Fatal(err)
	}
	if res.Number != 77 || res.Milestone != "v1" || res.Parent != 12 {
		t.Fatalf("result: %+v", res)
	}
	joined := strings.Join(*calls, "\n")
	for _, want := range []string{
		"issue create -R acme/widgets --title Add export --body b --label bees --label bees:triage --assignee kyle --milestone v1",
		"api --method POST repos/acme/widgets/issues/12/sub_issues -F sub_issue_id=9077",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing call %q in:\n%s", want, joined)
		}
	}
}

func TestCreateKinds(t *testing.T) {
	labels := config.LabelsFor("bees")
	for _, c := range []struct {
		opts Options
		want string
	}{
		{Options{Title: "t", Kind: KindBug, Related: 12}, "--label bees --label bees:bug --label bees:triage --milestone v1"},
		{Options{Title: "t", Kind: KindFeature}, "--label bees --label bees:feature --label bees:proposal --milestone pinned"},
		{Options{Title: "t", Kind: KindTask, Ready: true, Milestone: "v2", Related: 12}, "--label bees --label bees:ready --milestone v2"},
		{Options{Title: "t", Kind: KindTask, ExtraLabels: []string{"docs"}}, "--label bees --label bees:triage --label docs --milestone pinned"},
	} {
		gh, calls := fake(t, "v1")
		_, err := Create(context.Background(), gh, config.Filter{Label: "bees", Milestone: "pinned"}, labels, c.opts)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(*calls, "\n") + "\n"
		if !strings.Contains(joined, c.want) {
			t.Errorf("%+v: missing %q in:\n%s", c.opts, c.want, joined)
		}
		if strings.Contains(joined, "sub_issues") {
			t.Errorf("%+v: must not link without --parent", c.opts)
		}
	}
	gh, _ := fake(t, "")
	if _, err := Create(context.Background(), gh, config.Filter{Label: "bees"}, labels, Options{Title: "t", Parent: 1, Related: 2}); err == nil {
		t.Fatal("parent and related together must fail")
	}
	if _, err := Create(context.Background(), gh, config.Filter{Label: "bees"}, labels, Options{Kind: KindTask}); err == nil {
		t.Fatal("title required")
	}
}

func TestCreateBlockedBy(t *testing.T) {
	gh, calls := fake(t, "")
	labels := config.LabelsFor("bees")
	_, err := Create(context.Background(), gh, config.Filter{Label: "bees"}, labels,
		Options{Title: "t", Body: "the real body", Kind: KindTask, BlockedBy: []int{12, 15}})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(*calls, "\n")
	if !strings.Contains(joined, "--body Blocked by #12, #15\n\nthe real body") {
		t.Fatalf("body not prefixed:\n%s", joined)
	}
	if got := blockedByBody(nil, "the real body"); got != "the real body" {
		t.Fatalf("no blockers must leave the body alone: %q", got)
	}
	if got := blockedByBody([]int{12, 15}, "b"); !strings.HasPrefix(got, "Blocked by #12, #15\n\n") {
		t.Fatalf("body: %q", got)
	}
}

// A proposal is a bee's own idea: only a person turns it into work, by
// removing the label. Until then it must grow no sub-issues, through either
// door, and the refusal must happen before anything is created.
func TestProposalGrowsNoSubIssues(t *testing.T) {
	labels := config.LabelsFor("bees")
	filter := config.Filter{Label: "bees"}

	gh, calls := fake(t, "v1")
	_, err := Create(context.Background(), gh, filter, labels, Options{Title: "t", Kind: KindTask, Parent: 13})
	if err == nil {
		t.Fatal("--parent on a proposal must fail")
	}
	for _, want := range []string{"#13", "bees:proposal"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if joined := strings.Join(*calls, "\n"); strings.Contains(joined, "issue create") {
		t.Errorf("the issue was created before the refusal:\n%s", joined)
	}

	// Also with an explicit milestone, which used to be the only reason the
	// parent was looked up at all.
	gh, calls = fake(t, "v1")
	if _, err := Create(context.Background(), gh, filter, labels,
		Options{Title: "t", Kind: KindTask, Parent: 13, Milestone: "v2"}); err == nil {
		t.Error("--parent on a proposal must fail whatever the milestone is")
	}
	if joined := strings.Join(*calls, "\n"); strings.Contains(joined, "issue create") {
		t.Errorf("the issue was created before the refusal:\n%s", joined)
	}

	// Same hole, other door.
	gh, calls = fake(t, "v1")
	_, err = Link(context.Background(), gh, labels, 13, 77)
	if err == nil {
		t.Fatal("linking to a proposal must fail")
	}
	if !strings.Contains(err.Error(), "#13") {
		t.Errorf("error %q does not name the proposal", err)
	}
	if joined := strings.Join(*calls, "\n"); strings.Contains(joined, "sub_issues") {
		t.Errorf("the issue was attached before the refusal:\n%s", joined)
	}

	// --related creates no relationship, so it stays allowed: it only
	// inherits the proposal's milestone.
	gh, calls = fake(t, "v1")
	res, err := Create(context.Background(), gh, filter, labels, Options{Title: "t", Kind: KindTask, Related: 13})
	if err != nil {
		t.Fatalf("--related on a proposal must be allowed: %v", err)
	}
	if res.Number != 77 || res.Milestone != "v1" {
		t.Fatalf("result: %+v", res)
	}
	if joined := strings.Join(*calls, "\n"); strings.Contains(joined, "sub_issues") {
		t.Errorf("--related must create no sub-issue:\n%s", joined)
	}

	// An ordinary parent is still linked.
	gh, calls = fake(t, "v1")
	if _, err := Create(context.Background(), gh, filter, labels, Options{Title: "t", Kind: KindTask, Parent: 12}); err != nil {
		t.Fatal(err)
	}
	if joined := strings.Join(*calls, "\n"); !strings.Contains(joined, "sub_issues") {
		t.Errorf("an approved parent must still be linked:\n%s", joined)
	}
	if _, err := Link(context.Background(), gh, labels, 12, 77); err != nil {
		t.Fatalf("linking to an approved feature: %v", err)
	}
}

// Milestone inheritance is a property of becoming a sub-issue, not of the
// create path: a person putting a feature into a milestone means the work
// under it belongs in that release, however it got there. The other half of
// the rule is that a milestone already on the child is a person's decision,
// so a bee never overwrites or clears one.
func TestLinkInheritsTheParentsMilestone(t *testing.T) {
	labels := config.LabelsFor("bees")

	// Parent in a milestone, child in none: the child ends up in it.
	gh, calls := fakeWith(t, "v1", "")
	res, err := Link(context.Background(), gh, labels, 12, 77)
	if err != nil {
		t.Fatal(err)
	}
	if res.Milestone != "v1" {
		t.Errorf("result: %+v", res)
	}
	if got, want := res.String(), `#77 is now a sub-issue of #12 milestone "v1"`; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	joined := strings.Join(*calls, "\n")
	for _, want := range []string{
		"api --method POST repos/acme/widgets/issues/12/sub_issues -F sub_issue_id=9077",
		"api --method PATCH repos/acme/widgets/issues/77 -F milestone=3",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing call %q in:\n%s", want, joined)
		}
	}

	// Child already in a different milestone: untouched, not even looked up.
	gh, calls = fakeWith(t, "v1", "v2")
	res, err = Link(context.Background(), gh, labels, 12, 77)
	if err != nil {
		t.Fatal(err)
	}
	if res.Milestone != "" {
		t.Errorf("a milestone a person set must not be overwritten: %+v", res)
	}
	if got, want := res.String(), "#77 is now a sub-issue of #12"; got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
	joined = strings.Join(*calls, "\n")
	if !strings.Contains(joined, "sub_issues") {
		t.Errorf("the link was not made:\n%s", joined)
	}
	if strings.Contains(joined, "--method PATCH") || strings.Contains(joined, "widgets/milestones") {
		t.Errorf("the child's milestone was touched:\n%s", joined)
	}

	// Parent in no milestone: there is nothing to inherit, and the child
	// keeps whatever it had.
	gh, calls = fakeWith(t, "", "")
	res, err = Link(context.Background(), gh, labels, 12, 77)
	if err != nil {
		t.Fatal(err)
	}
	if res.Milestone != "" {
		t.Errorf("result: %+v", res)
	}
	joined = strings.Join(*calls, "\n")
	if !strings.Contains(joined, "sub_issues") {
		t.Errorf("the link was not made:\n%s", joined)
	}
	if strings.Contains(joined, "--method PATCH") || strings.Contains(joined, "widgets/milestones") {
		t.Errorf("a milestone call was made with nothing to inherit:\n%s", joined)
	}
}

// The link is made first, so a failure to set the milestone leaves a real
// sub-issue behind: the error has to name both issues, or the caller cannot
// tell what did happen.
func TestLinkReportsAMilestoneFailure(t *testing.T) {
	// "v9" matches no open milestone in the fake.
	gh, calls := fakeWith(t, "v9", "")
	_, err := Link(context.Background(), gh, config.LabelsFor("bees"), 12, 77)
	if err == nil {
		t.Fatal("a failed milestone call must not be swallowed")
	}
	for _, want := range []string{"#77", "#12", "v9"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if joined := strings.Join(*calls, "\n"); !strings.Contains(joined, "sub_issues") {
		t.Errorf("the link itself should have been made:\n%s", joined)
	}
}

// An issue a person put in planning mode is a conversation, not a spec: it
// must grow no sub-issues through either door until they end planning by
// swapping the label for bees:planned, and the refusal must happen before
// anything is created.
func TestAPlanningIssueGrowsNoSubIssues(t *testing.T) {
	labels := config.LabelsFor("bees")
	filter := config.Filter{Label: "bees"}

	gh, calls := fake(t, "v1")
	_, err := Create(context.Background(), gh, filter, labels, Options{Title: "t", Kind: KindTask, Parent: 14})
	if err == nil {
		t.Fatal("--parent on a planning issue must fail")
	}
	for _, want := range []string{"#14", "bees:planning", "bees:planned"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if joined := strings.Join(*calls, "\n"); strings.Contains(joined, "issue create") {
		t.Errorf("the issue was created before the refusal:\n%s", joined)
	}

	// Same hole, other door.
	gh, calls = fake(t, "v1")
	if _, err := Link(context.Background(), gh, labels, 14, 77); err == nil {
		t.Fatal("linking to a planning issue must fail")
	}
	if joined := strings.Join(*calls, "\n"); strings.Contains(joined, "sub_issues") {
		t.Errorf("the issue was attached before the refusal:\n%s", joined)
	}

	// --related only inherits a milestone: it creates no sub-issue, so it is
	// not this hole and stays allowed.
	gh, _ = fake(t, "v1")
	if _, err := Create(context.Background(), gh, filter, labels, Options{Title: "t", Kind: KindTask, Related: 14}); err != nil {
		t.Errorf("--related on a planning issue: %v", err)
	}
}
