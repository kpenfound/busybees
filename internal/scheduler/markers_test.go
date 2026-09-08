package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/session"
)

// markerWarning is the message auditMarkers logs for a comment of the
// factory's that carries no role marker.
const markerWarning = "a comment the factory posted is missing its bees marker"

// seedComments serves comments for an issue or pull request from the fake
// GitHub, in the shape the REST comments endpoint returns them.
func seedComments(h *harness, number int, comments ...string) {
	h.gh.activity[fmt.Sprintf("repos/acme/widgets/issues/%d/comments", number)] =
		"[" + strings.Join(comments, ",\n") + "]"
}

// comment renders one comment fixture.
func commentJSON(id int, login, body string, at time.Time) string {
	return fmt.Sprintf(`{"id": %d, "user": {"login": %q}, "body": %q, "html_url": "https://x/%d", "created_at": %q}`,
		id, login, body, id, at.UTC().Format(time.RFC3339))
}

// devSpec is a developer session on one issue, as the worker builds it.
func devSpec(n int) sessionSpec {
	spec := sessionSpec{role: config.RoleDeveloper, name: fmt.Sprintf("developer-issue-%d-r1", n)}
	spec.data.Issue = &github.Issue{Number: n, Title: "Issue"}
	return spec
}

// A comment the factory left while a session ran, without the marker every
// bee comment must end with, is reported once. The three comments beside it
// are not: a person's comment needs no marker, a bee comment that has one is
// what the comment tool posts, and a comment from before the session began
// is not this session's to answer for.
func TestAMarkerlessCommentIsReportedOnce(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	h.sched.gh.ActsAs = "busybees-bot"
	started := time.Now()
	after, before := started.Add(time.Minute), started.Add(-time.Hour)
	seedComments(h, 1,
		commentJSON(41, "kyle", "I would rather you did not.", after),
		commentJSON(42, "busybees-bot", "Closing: superseded by #7.", after),
		commentJSON(43, "busybees-bot", "Done.\n\n<!-- bees:developer -->", after),
		commentJSON(44, "busybees-bot", "An older one of mine.", before),
	)

	h.sched.auditMarkers(context.Background(), devSpec(1), nil, 0, started)

	logs := h.logs.String()
	if got := strings.Count(logs, markerWarning); got != 1 {
		t.Fatalf("%d markerless comments reported, want exactly 1:\n%s", got, logs)
	}
	for _, want := range []string{"issue=1", "role=developer", "comment=42"} {
		if !strings.Contains(logs, want) {
			t.Errorf("the warning does not name %s:\n%s", want, logs)
		}
	}
}

// The audit runs when a session ends, on the work item it was given: a
// comment left with `gh` during the session is reported without anybody
// calling the check by hand.
func TestASessionsCommentsAreCheckedWhenItEnds(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	h.sched.gh.ActsAs = "busybees-bot"
	seedComments(h, 1, commentJSON(42, "busybees-bot", "Closing this.", time.Now().Add(time.Minute)))

	spec := devSpec(1)
	spec.workDir = h.cfg.Dir()
	if _, err := h.sched.runSession(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	if logs := h.logs.String(); !strings.Contains(logs, markerWarning) {
		t.Fatalf("a session that left a markerless comment was not reported:\n%s", logs)
	}
}

// On the shared account the factory and the people it works for use, a
// comment without a marker is a person's as far as anything can tell, so
// there is nothing to check and the audit spends no call finding that out.
func TestASharedAccountIsNotAudited(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	seedComments(h, 1, commentJSON(42, "kyle", "Closing this.", time.Now().Add(time.Minute)))
	before := h.gh.total()

	h.sched.auditMarkers(context.Background(), devSpec(1), nil, 0, time.Now())

	if got := h.gh.total(); got != before {
		t.Errorf("the audit made %d gh calls without a [github] login, want 0", got-before)
	}
	if logs := h.logs.String(); strings.Contains(logs, markerWarning) {
		t.Errorf("a person's comment was reported as the factory's:\n%s", logs)
	}
}

// A pull request's comments are audited as well as the issue's, and the
// warning names it as a pull request.
func TestAMarkerlessCommentOnAPullRequestIsReported(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	h.sched.gh.ActsAs = "busybees-bot"
	started := time.Now()
	seedComments(h, 201, commentJSON(42, "busybees-bot", "Rebased.", started.Add(time.Minute)))

	spec := devSpec(1)
	spec.data.PR = &github.PR{Number: 201}
	h.sched.auditMarkers(context.Background(), spec, nil, 0, started)

	logs := h.logs.String()
	if !strings.Contains(logs, markerWarning) || !strings.Contains(logs, "pr=201") {
		t.Fatalf("the comment on the pull request was not reported as a pull request's:\n%s", logs)
	}
}

// Where a session could have commented: the work item it was given, the pull
// request it reported opening, and every issue it changed through the MCP
// server, each one once.
func TestCommentTargetsAreTheWorkItemAndTheIssuesTouched(t *testing.T) {
	spec := devSpec(1)
	spec.data.PR = &github.PR{Number: 201}

	got := commentTargets(spec, []int{7, 1}, 202)

	want := []commentTarget{{number: 1}, {number: 201, pr: true}, {number: 202, pr: true}, {number: 7}}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("targets %v, want %v", got, want)
	}
}

// The round that opens a pull request is handed none: the spec carries the
// pull request the session was given, and there was not one yet. What the
// session reported opening is audited instead, so a comment it leaves on its
// own new pull request is covered by the session that made it.
func TestThePullRequestASessionOpensIsAudited(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	h.sched.gh.ActsAs = "busybees-bot"
	seedComments(h, fakePR, commentJSON(42, "busybees-bot", "Opened.", time.Now().Add(time.Minute)))

	spec := devSpec(1)
	spec.workDir = h.cfg.Dir()
	if spec.data.PR != nil {
		t.Fatal("the first round is dispatched with a pull request")
	}
	if _, err := h.sched.runSession(context.Background(), spec); err != nil {
		t.Fatal(err)
	}

	logs := h.logs.String()
	if !strings.Contains(logs, markerWarning) || !strings.Contains(logs, fmt.Sprintf("pr=%d", fakePR)) {
		t.Fatalf("a markerless comment on the pull request the session opened was not reported:\n%s", logs)
	}
}

// A session that reported no pull request adds no target, so nothing is read
// for it.
func TestASessionThatOpensNoPullRequestAddsNoTarget(t *testing.T) {
	if got := commentTargets(sessionSpec{}, nil, 0); len(got) != 0 {
		t.Fatalf("targets %v, want none", got)
	}
	if got := openedPR(t.TempDir()); got != 0 {
		t.Fatalf("openedPR of a session with no outcome file = %d, want 0", got)
	}
}

// The issues a session changed are read from its directory once: the refresh
// reads the file and the audit is handed what it read.
func TestTheTouchedIssuesAreAuditedToo(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, noRolesTOML)
	h.sched.gh.ActsAs = "busybees-bot"
	h.gh.issues[9] = &github.Issue{Number: 9, Title: "Filed by the session", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	seedComments(h, 9, commentJSON(42, "busybees-bot", "Filed as #9.", time.Now().Add(time.Minute)))
	dir := t.TempDir()
	if err := session.RecordTouched(dir, 9); err != nil {
		t.Fatal(err)
	}

	touched := h.sched.refreshTouched(ctx, dir)
	h.sched.auditMarkers(ctx, sessionSpec{role: config.RoleProjectManager, name: "project_manager-1"}, touched, 0, time.Now().Add(-time.Minute))

	logs := h.logs.String()
	if !strings.Contains(logs, markerWarning) || !strings.Contains(logs, "issue=9") {
		t.Fatalf("a markerless comment on an issue the session changed was not reported:\n%s", logs)
	}
}
