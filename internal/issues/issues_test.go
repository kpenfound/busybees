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
		case strings.HasPrefix(call, "api repos/acme/widgets/issues/77"):
			return []byte(`{"id": 9077, "milestone": null}`), nil
		case strings.HasPrefix(call, "issue create"):
			return []byte("https://github.com/acme/widgets/issues/77\n"), nil
		case strings.HasPrefix(call, "api --method POST repos/acme/widgets/issues/12/sub_issues"):
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
		{Options{Title: "t", Kind: KindFeature}, "--label bees --label bees:feature --milestone pinned"},
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
