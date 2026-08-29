package github

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestQueryArgs(t *testing.T) {
	q := Query{Label: "bees", Assignee: "kyle", Milestone: "v1"}
	got := strings.Join(q.args(), " ")
	if got != "--label bees --assignee kyle --milestone v1" {
		t.Fatalf("args: %s", got)
	}
	if len((Query{}).args()) != 0 {
		t.Fatal("empty query should produce no args")
	}
	labels := []Label{{Name: "bees"}}
	if !q.Matches(labels, []Author{{Login: "Kyle"}}, "v1") {
		t.Fatal("should match")
	}
	if q.Matches(labels, nil, "v1") {
		t.Fatal("should not match without assignee")
	}
	if (Query{Label: "bees"}).Matches(nil, nil, "") {
		t.Fatal("should not match without label")
	}
}

func TestSetState(t *testing.T) {
	var calls [][]string
	c := New("a/b")
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	}
	states := []string{"bees:triage", "bees:ready", "bees:in-progress"}
	current := []Label{{Name: "bees"}, {Name: "bees:triage"}, {Name: "bees:ready"}}
	if err := c.SetState(context.Background(), 7, current, "bees:in-progress", states); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls: %v", calls)
	}
	got := strings.Join(calls[0], " ")
	want := "issue edit 7 -R a/b --add-label bees:in-progress --remove-label bees:triage --remove-label bees:ready"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// Already in the right state: no call.
	calls = nil
	if err := c.SetState(context.Background(), 7, []Label{{Name: "bees:in-progress"}}, "bees:in-progress", states); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("expected no call, got %v", calls)
	}
}

func TestListParsing(t *testing.T) {
	c := New("a/b")
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "issue" {
			return json.Marshal([]Issue{{Number: 1, Title: "one", Labels: []Label{{Name: "bees"}}}})
		}
		return json.Marshal([]PR{{Number: 2, HeadRefName: "bees/issue-1", Body: "Closes #1"}})
	}
	issues, err := c.ListOpenIssues(context.Background(), Query{Label: "bees"})
	if err != nil || len(issues) != 1 || !HasLabel(issues[0].Labels, "bees") {
		t.Fatalf("issues: %v %v", issues, err)
	}
	pr, err := c.FindPRForBranch(context.Background(), "bees/issue-1")
	if err != nil || pr == nil || pr.ClosingIssues()[0] != 1 {
		t.Fatalf("pr: %v %v", pr, err)
	}
}

func TestPRActivity(t *testing.T) {
	c := New("a/b")
	base := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ts := func(m int) string { return base.Add(time.Duration(m) * time.Minute).Format(time.RFC3339) }
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		path := args[len(args)-1]
		switch {
		case strings.HasSuffix(path, "/reviews"):
			return []byte(`[[{"id":1,"user":{"login":"h"},"body":"","state":"APPROVED","submitted_at":"` + ts(5) + `"},
				{"id":2,"user":{"login":"h"},"body":"needs work","state":"CHANGES_REQUESTED","submitted_at":"` + ts(3) + `"}]]`), nil
		case strings.Contains(path, "/pulls/"):
			return []byte(`[[{"id":3,"user":{"login":"h"},"body":"old","path":"a.go","line":2,"created_at":"` + ts(-10) + `"},
				{"id":4,"user":{"login":"h"},"body":"rename","path":"a.go","line":9,"created_at":"` + ts(4) + `"}],
				[{"id":5,"user":{"login":"h"},"body":"ack <!-- bees:developer -->","path":"a.go","line":9,"created_at":"` + ts(6) + `"}]]`), nil
		default:
			return []byte(`[[{"id":6,"user":{"login":"h"},"body":"thanks","created_at":"` + ts(1) + `"}]]`), nil
		}
	}
	got, err := c.PRActivity(context.Background(), 9, base)
	if err != nil {
		t.Fatal(err)
	}
	var kinds []string
	for _, a := range got {
		kinds = append(kinds, a.Kind+":"+strconv.FormatInt(a.ID, 10))
	}
	if want := "comment:6,review:2,review-comment:4"; strings.Join(kinds, ",") != want {
		t.Fatalf("got %v want %s", kinds, want)
	}
	if got[2].Path != "a.go" || got[2].Line != 9 {
		t.Fatalf("inline comment: %+v", got[2])
	}
}

func TestRequiredChecks(t *testing.T) {
	c := New("a/b")
	var resp []byte
	var rerr error
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] != "pr" || args[1] != "checks" || !strings.Contains(strings.Join(args, " "), "--required") {
			t.Fatalf("unexpected args %v", args)
		}
		return resp, rerr
	}
	ctx := context.Background()
	// pending: gh exits 8 but prints JSON
	resp, rerr = []byte(`[{"name":"a","bucket":"pending"},{"name":"b","bucket":"pass"}]`), fmt.Errorf("gh: exit status 8")
	checks, err := c.RequiredChecks(ctx, 1)
	if err != nil || Summarize(checks) != ChecksPending {
		t.Fatalf("pending: %v %v", checks, err)
	}
	resp, rerr = []byte(`[{"name":"a","bucket":"fail"},{"name":"b","bucket":"pass"}]`), fmt.Errorf("gh: exit status 1")
	checks, err = c.RequiredChecks(ctx, 1)
	if err != nil || Summarize(checks) != ChecksFailed || len(Failed(checks)) != 1 || Failed(checks)[0].Name != "a" {
		t.Fatalf("failed: %v %v", checks, err)
	}
	resp, rerr = []byte(`[{"name":"a","bucket":"skipping"},{"name":"b","bucket":"pass"}]`), nil
	checks, err = c.RequiredChecks(ctx, 1)
	if err != nil || Summarize(checks) != ChecksPassed {
		t.Fatalf("passed: %v %v", checks, err)
	}
	resp, rerr = nil, fmt.Errorf("gh pr checks: exit status 1: no required checks reported on the 'x' branch")
	checks, err = c.RequiredChecks(ctx, 1)
	if err != nil || len(checks) != 0 || Summarize(checks) != ChecksPassed {
		t.Fatalf("none: %v %v", checks, err)
	}
	resp, rerr = nil, fmt.Errorf("gh pr checks: exit status 4: not logged in")
	if _, err = c.RequiredChecks(ctx, 1); err == nil {
		t.Fatal("real errors must propagate")
	}
}

func TestClosingIssues(t *testing.T) {
	cases := []struct {
		body string
		want []int
	}{
		{"Closes #1", []int{1}},
		{"closes #1\n\nfixes #2, Resolved: #3, fix #2 again", []int{1, 2, 3}},
		{"Fixed https://github.com/a/b/issues/4 and closes https://github.com/x/y/issues/5", []int{4}},
		{"See #6 and closes#7 and closest #8", []int{7}},
		{"", nil},
	}
	for _, tc := range cases {
		got := PR{URL: "https://github.com/a/b/pull/9", Body: tc.body}.ClosingIssues()
		if fmt.Sprint(got) != fmt.Sprint(tc.want) {
			t.Errorf("%q: got %v want %v", tc.body, got, tc.want)
		}
	}
}
