package github

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
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
	// The size label is not in states, so it must survive the move.
	current := []Label{{Name: "bees"}, {Name: "bees:triage"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}
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
				[{"id":5,"user":{"login":"h"},"body":"ack\n\n<!-- bees:developer -->","path":"a.go","line":9,"created_at":"` + ts(6) + `"}]]`), nil
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
	if err != nil || len(checks) != 0 || Summarize(checks) != ChecksNone {
		t.Fatalf("none: %v %v", checks, err)
	}
	resp, rerr = nil, fmt.Errorf("gh pr checks: exit status 4: not logged in")
	if _, err = c.RequiredChecks(ctx, 1); err == nil {
		t.Fatal("real errors must propagate")
	}
}

// TestChecks covers the unrequired call: same gh command without --required,
// and the same tolerance for gh's non-zero exits and empty output.
func TestChecks(t *testing.T) {
	c := New("a/b")
	var resp []byte
	var rerr error
	var got []string
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		got = args
		return resp, rerr
	}
	ctx := context.Background()
	resp, rerr = []byte(`[{"name":"a","bucket":"pending"},{"name":"b","bucket":"pass"}]`), fmt.Errorf("gh: exit status 8")
	checks, err := c.Checks(ctx, 7)
	if err != nil || Summarize(checks) != ChecksPending {
		t.Fatalf("pending: %v %v", checks, err)
	}
	if want := "pr checks 7 -R a/b --json name,state,bucket,link,description,workflow"; strings.Join(got, " ") != want {
		t.Fatalf("args: %v", got)
	}
	if slices.Contains(got, "--required") {
		t.Fatal("Checks must not ask for the required checks only")
	}
	resp, rerr = []byte(`[{"name":"a","bucket":"fail"}]`), fmt.Errorf("gh: exit status 1")
	checks, err = c.Checks(ctx, 7)
	if err != nil || Summarize(checks) != ChecksFailed || len(Failed(checks)) != 1 {
		t.Fatalf("failed: %v %v", checks, err)
	}
	resp, rerr = nil, fmt.Errorf("gh pr checks: exit status 1: no checks reported on the 'x' branch")
	checks, err = c.Checks(ctx, 7)
	if err != nil || len(checks) != 0 || Summarize(checks) != ChecksNone {
		t.Fatalf("none: %v %v", checks, err)
	}
	resp, rerr = []byte("[]"), nil
	checks, err = c.Checks(ctx, 7)
	if err != nil || len(checks) != 0 || Summarize(checks) != ChecksNone {
		t.Fatalf("empty list: %v %v", checks, err)
	}
	resp, rerr = nil, fmt.Errorf("gh pr checks: exit status 4: not logged in")
	if _, err = c.Checks(ctx, 7); err == nil {
		t.Fatal("real errors must propagate")
	}
}

// TestSummarizeNoChecks pins the distinction #117 turns on: nothing reported
// is not everything green.
func TestSummarizeNoChecks(t *testing.T) {
	if got := Summarize(nil); got != ChecksNone {
		t.Fatalf("Summarize(nil) = %q, want %q", got, ChecksNone)
	}
	if got := Summarize([]Check{}); got != ChecksNone {
		t.Fatalf("Summarize([]) = %q, want %q", got, ChecksNone)
	}
	if got := Summarize([]Check{{Bucket: "pass"}}); got != ChecksPassed {
		t.Fatalf("Summarize(one pass) = %q", got)
	}
	if got := Summarize([]Check{{Bucket: "skipping"}}); got != ChecksPassed {
		t.Fatalf("Summarize(skipping) = %q", got)
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

// TestEditBody pins where the body goes: on gh's standard input, never on
// the command line, where an arbitrarily long markdown document with any
// quoting in it does not belong.
func TestEditBody(t *testing.T) {
	var args []string
	var stdin string
	calls := 0
	c := New("a/b")
	c.Exec = func(ctx context.Context, a ...string) ([]byte, error) {
		t.Fatalf("EditBody used Exec, which cannot carry the body: %v", a)
		return nil, nil
	}
	c.ExecStdin = func(ctx context.Context, in string, a ...string) ([]byte, error) {
		calls++
		args, stdin = a, in
		return nil, nil
	}
	body := "## Scope\n\nA body with \"quotes\", $shell and a trailing newline.\n"
	if err := c.EditBody(context.Background(), 7, body); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("calls: %d", calls)
	}
	if got, want := strings.Join(args, " "), "issue edit 7 -R a/b --body-file -"; got != want {
		t.Fatalf("args = %q, want %q", got, want)
	}
	if stdin != body {
		t.Fatalf("stdin = %q, want %q", stdin, body)
	}
	for _, a := range args {
		if strings.Contains(a, "Scope") {
			t.Fatalf("the body reached the command line: %v", args)
		}
	}
}

func TestAssignUsesTheRESTEndpoint(t *testing.T) {
	var calls [][]string
	c := New("a/b")
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		return nil, nil
	}
	if err := c.Assign(context.Background(), 7, "kyle", "ada"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 {
		t.Fatalf("calls: %v", calls)
	}
	got := strings.Join(calls[0], " ")
	// `gh issue edit --add-assignee` fails against GitHub for a PR number
	// with a Projects (classic) GraphQL error, so it must not be used.
	want := "api --method POST repos/a/b/issues/7/assignees -f assignees[]=kyle -f assignees[]=ada"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// No logins: no call.
	calls = nil
	if err := c.Assign(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("empty assign called gh: %v", calls)
	}
}

func TestSetMilestone(t *testing.T) {
	var calls [][]string
	c := New("a/b")
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		calls = append(calls, args)
		if strings.Contains(args[len(args)-1], "/milestones?") {
			return []byte(`[{"number":3,"title":"v0.1.0"},{"number":4,"title":"v0.2.0"}]`), nil
		}
		return nil, nil
	}
	if err := c.SetMilestone(context.Background(), 7, "v0.2.0"); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 2 {
		t.Fatalf("calls: %v", calls)
	}
	got := strings.Join(calls[1], " ")
	want := "api --method PATCH repos/a/b/issues/7 -F milestone=4"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	// An unknown title is an error naming it, and edits nothing.
	calls = nil
	err := c.SetMilestone(context.Background(), 7, "v9")
	if err == nil || !strings.Contains(err.Error(), `"v9"`) {
		t.Fatalf("err: %v", err)
	}
	if len(calls) != 1 {
		t.Fatalf("unknown milestone edited the PR: %v", calls)
	}
	// An empty title is a no-op.
	calls = nil
	if err := c.SetMilestone(context.Background(), 7, ""); err != nil {
		t.Fatal(err)
	}
	if len(calls) != 0 {
		t.Fatalf("empty milestone called gh: %v", calls)
	}
}

// The visibility backstop needs the milestone of everything the account
// created, so both listings must ask gh for it — a field missing from
// --json is silently absent from the JSON, not an error.
func TestListCreatedSinceReadsTheMilestone(t *testing.T) {
	c := New("a/b")
	since := time.Date(2026, 8, 30, 12, 0, 0, 0, time.UTC)
	var fields []string
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		i := slices.Index(args, "--json")
		if i < 0 || i+1 >= len(args) {
			return nil, fmt.Errorf("no --json in %v", args)
		}
		fields = append(fields, args[i+1])
		if args[0] == "issue" {
			return json.Marshal([]Issue{{Number: 1, CreatedAt: since.Add(time.Minute),
				Milestone: &MilestoneRef{Title: "v0.2.0"}}})
		}
		return json.Marshal([]PR{{Number: 2, CreatedAt: since.Add(time.Minute),
			Milestone: &MilestoneRef{Title: "v0.1.0"}}})
	}

	created, err := c.ListCreatedSince(context.Background(), since)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range fields {
		if !strings.Contains(f, "milestone") {
			t.Errorf("--json %q does not ask for the milestone", f)
		}
	}
	if len(created) != 2 {
		t.Fatalf("created: %v", created)
	}
	if got := created[0].MilestoneTitle(); got != "v0.2.0" {
		t.Errorf("issue milestone: %q", got)
	}
	if got := created[1].MilestoneTitle(); got != "v0.1.0" {
		t.Errorf("pr milestone: %q", got)
	}
	// An item in no milestone reports "" rather than panicking.
	if got := (Created{}).MilestoneTitle(); got != "" {
		t.Errorf("no milestone: %q", got)
	}
}

func TestAwaitingBeeComment(t *testing.T) {
	created := time.Now().Add(-time.Hour)
	human := func(at time.Time) Comment {
		return Comment{Author: Author{Login: "kyle"}, Body: "what about X?", CreatedAt: at}
	}
	bee := func(at time.Time) Comment {
		return Comment{Author: Author{Login: "kyle"}, Body: "answered that.\n\n<!-- bees:reviewer -->", CreatedAt: at}
	}
	// A person replying to a bee quotes the comment they are answering,
	// marker and all: the marker is in the body but it is not the last
	// line, so the comment is still a person's and still needs an answer.
	quoting := func(at time.Time) Comment {
		return Comment{Author: Author{Login: "kyle"}, Body: "Replying to the bot:\n> looks good to me\n> <!-- bees:reviewer -->\n\nActually please hold off on merging.", CreatedAt: at}
	}
	// gh reports comment times at second resolution, so two comments written
	// in the same second come back with the same timestamp and only their
	// order in the list says who spoke last.
	tie := created.Add(time.Minute)

	cases := []struct {
		name string
		// comments is the issue's comment list, chronological as gh returns it.
		comments []Comment
		// want is what AwaitingBeeComment answers, wantBee what AwaitingBee
		// answers: it seeds the human side with the issue's creation, so an
		// issue nobody answered is awaiting a bee even with no comments.
		want, wantBee bool
	}{
		{"no comments", nil, false, true},
		{"a human comment", []Comment{human(created.Add(time.Minute))}, true, true},
		{"a bee answered it", []Comment{human(created.Add(time.Minute)), bee(created.Add(2 * time.Minute))}, false, false},
		{"a human came back", []Comment{human(created.Add(time.Minute)), bee(created.Add(2 * time.Minute)), human(created.Add(3 * time.Minute))}, true, true},
		{"a human commented in the bee's second", []Comment{bee(tie), human(tie)}, true, true},
		{"a bee answered within the second", []Comment{human(tie), bee(tie)}, false, false},
		{"a human came back within the second", []Comment{human(created.Add(time.Minute)), bee(tie), human(tie)}, true, true},
		{"a bee commented in the issue's second", []Comment{bee(created)}, false, false},
		// Not chronological: the bee comment is older than the human one
		// before it, so the human still had the last word. The second case
		// is the same guard against the seed: for AwaitingBee the issue's
		// creation is human activity at CreatedAt, and a comment older than
		// that does not get the last word off it.
		{"comments out of order", []Comment{human(created.Add(2 * time.Minute)), bee(created.Add(time.Minute))}, true, true},
		{"a bee comment older than the issue", []Comment{bee(created.Add(-time.Minute))}, false, true},
		// A quoted marker is not authorship: the person had the last word.
		{"a human quoted the bee they answer", []Comment{human(created.Add(time.Minute)), bee(created.Add(2 * time.Minute)), quoting(created.Add(3 * time.Minute))}, true, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			i := Issue{Number: 6, CreatedAt: created, Comments: c.comments}
			if got := i.AwaitingBeeComment(); got != c.want {
				t.Errorf("AwaitingBeeComment() = %v, want %v", got, c.want)
			}
			if got := i.AwaitingBee(); got != c.wantBee {
				t.Errorf("AwaitingBee() = %v, want %v", got, c.wantBee)
			}
		})
	}
}

// TestBeeRole pins the positional rule: only a body whose last non-empty line
// is a complete marker is a bee's, so quoting one is not authorship.
func TestBeeRole(t *testing.T) {
	cases := []struct {
		name, body, role string
	}{
		{"plain text", "looks fine to me", ""},
		{"a bee comment", "Done.\n\n<!-- bees:developer -->", "developer"},
		{"nothing but a marker", "<!-- bees:qa -->", "qa"},
		{"trailing newlines after the marker", "Done.\n\n<!-- bees:developer -->\n\n\n", "developer"},
		{"a marker quoted mid-body", "> <!-- bees:reviewer -->\n\nplease hold off", ""},
		{"a quoted marker on the last line", "answering you:\n\n> <!-- bees:reviewer -->", ""},
		{"a bee quoting another role", "> <!-- bees:developer -->\n\nmine\n\n<!-- bees:qa -->", "qa"},
		{"an unterminated marker", "hi\n\n<!-- bees:qa >", ""},
		{"a marker with no role", "hi\n\n<!-- bees: -->", ""},
		{"a marker with text after it on its line", "answered <!-- bees:reviewer -->", ""},
		{"an empty body", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			role, ok := BeeRole(c.body)
			if ok != (c.role != "") || role != c.role {
				t.Errorf("BeeRole(%q) = %q, %v, want %q", c.body, role, ok, c.role)
			}
			if got := (Comment{Body: c.body}).IsBee(); got != (c.role != "") {
				t.Errorf("Comment.IsBee() = %v, want %v", got, c.role != "")
			}
			if got := (Activity{Body: c.body}).IsBee(); got != (c.role != "") {
				t.Errorf("Activity.IsBee() = %v, want %v", got, c.role != "")
			}
		})
	}
}

// fakeGHOnPath puts a `gh` on PATH that reports the token it was given and
// what it read on stdin. Tests must never reach the real gh; this one is the
// only way to see the environment the client builds, because the Exec hooks
// replace command execution wholesale and so never see it.
func fakeGHOnPath(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	script := "#!/bin/sh\nprintf 'token=[%s] args=[%s] stdin=[%s]' \"$GH_TOKEN\" \"$*\" \"$(cat)\"\n"
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// TestTokenReachesBothExecPaths pins that a configured token is carried by
// every gh invocation the client makes. Both exec paths matter: run and
// runStdin build their own command, and a token applied to only one of them
// would leave EditBody (the --body-file - caller) acting as the wrong
// account.
func TestTokenReachesBothExecPaths(t *testing.T) {
	fakeGHOnPath(t)
	ctx := context.Background()

	c := NewWithToken("acme/widgets", "ghp_bot")
	out, err := c.Exec(ctx, "issue", "list")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "token=[ghp_bot] args=[issue list] stdin=[]" {
		t.Errorf("Exec: %q", got)
	}
	out, err = c.ExecStdin(ctx, "body text", "issue", "edit", "1")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "token=[ghp_bot] args=[issue edit 1] stdin=[body text]" {
		t.Errorf("ExecStdin: %q", got)
	}
}

// TestNoTokenInjectsNothing pins the default: with [github] unset the client
// runs gh exactly as it always has, inheriting the machine's own
// authentication rather than being handed an empty GH_TOKEN (which gh would
// read as "no credentials" and fail on).
func TestNoTokenInjectsNothing(t *testing.T) {
	fakeGHOnPath(t)
	t.Setenv("GH_TOKEN", "the-machines-own")
	ctx := context.Background()

	c := New("acme/widgets")
	if c.Token != "" {
		t.Fatalf("New set a token: %q", c.Token)
	}
	if cmd := c.command(ctx, "issue", "list"); cmd.Env != nil {
		t.Errorf("command builds an explicit environment without a token: %v", cmd.Env)
	}
	out, err := c.Exec(ctx, "issue", "list")
	if err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "token=[the-machines-own] args=[issue list] stdin=[]" {
		t.Errorf("Exec: %q", got)
	}
	// And a configured token wins over the one the machine already has.
	c = NewWithToken("acme/widgets", "ghp_bot")
	if out, err = c.Exec(ctx, "issue", "list"); err != nil {
		t.Fatal(err)
	}
	if got := string(out); got != "token=[ghp_bot] args=[issue list] stdin=[]" {
		t.Errorf("configured token does not win: %q", got)
	}
}
