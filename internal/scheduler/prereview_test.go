package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/github"
)

// prereviewTOML runs the developer/reviewer loop alone with the pre-review
// checks read at its default: on. The timings come from newHarness.
const prereviewTOML = baseTOML + `
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
`

func seedPreReviewIssue(t *testing.T, h *harness, title string) {
	t.Helper()
	h.gh.issues[1] = &github.Issue{Number: 1, Title: title, State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}}
}

func runPreReviewLoop(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

func promptOf(t *testing.T, h *harness, i int) string {
	t.Helper()
	order := h.sessionOrder()
	if i >= len(order) {
		t.Fatalf("no session %d in %v", i, order)
	}
	b, err := os.ReadFile(filepath.Join(h.store.SessionsDir(), order[i], "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// TestPreReviewChecksFailBeforeTheFirstReview is the point of the stage: a
// failing check costs a check fix round, not a whole review round, and the
// reviewer's first real review starts from a green pull request.
func TestPreReviewChecksFailBeforeTheFirstReview(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Ship it")
	pending := `[{"name":"go / test","bucket":"pending","state":"PENDING","link":"https://ci.example.com/run/1","workflow":"CI"}]`
	failing := `[{"name":"go / test","bucket":"fail","state":"FAILURE","link":"https://ci.example.com/run/1","description":"1 test failed","workflow":"CI"}]`
	passing := `[{"name":"go / test","bucket":"pass","state":"SUCCESS","link":"https://ci.example.com/run/2"}]`
	h.gh.checks = []checksResponse{
		{pending, fmt.Errorf("exit status 8")},
		{failing, fmt.Errorf("exit status 1")},
		{passing, nil},
	}
	runPreReviewLoop(t, h)

	// The failing check reached the reviewer in checks mode before any review.
	// Review round 2 is an ordinary feedback round: it must not be named as a
	// check-fix round just because round 1 came back through the checks.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-checks1", "developer-issue-1-r1-checkfix1",
		"reviewer-pr-101-r1", "developer-issue-1-r2", "reviewer-pr-101-r2")
	if last := h.gh.history[1][len(h.gh.history[1])-1]; last != "bees:approved" {
		t.Fatalf("history: %v", h.gh.history[1])
	}
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("unexpected escalation: %v", h.gh.comments[1])
	}
	if diagnose := promptOf(t, h, 1); !strings.Contains(diagnose, "**go / test** (CI) — fail: 1 test failed") {
		t.Fatalf("checks prompt missing the failing check:\n%s", diagnose)
	}
	review := promptOf(t, h, 3)
	for _, want := range []string{"## Required checks", "go / test — pass", "https://ci.example.com/run/2", "CI is green"} {
		if !strings.Contains(review, want) {
			t.Errorf("reviewer prompt missing %q:\n%s", want, review)
		}
	}
	// The fix rounds are the checks counter, not review rounds.
	if bk, _ := h.store.Issue(1); bk.CheckFixRounds != 1 {
		t.Fatalf("bookkeeping: %+v", bk)
	}
}

// TestPreReviewChecksReadOncePerPullRequest: the read belongs to the first
// review, not to every round. A second review round is an ordinary round —
// no second read, no second wait, and no stale checks section claiming a head
// the developer has already replaced is green.
func TestPreReviewChecksReadOncePerPullRequest(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Two rounds")
	h.gh.checks = []checksResponse{{`[{"name":"go / test","bucket":"pass","state":"SUCCESS"}]`, nil}}
	runPreReviewLoop(t, h)

	// Round 1 requests changes, round 2 approves; the prereview stage runs
	// once, between the first developer session and the first review.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1", "developer-issue-1-r2", "reviewer-pr-101-r2")
	if n := h.gh.callCount("pr checks"); n != 1 {
		t.Fatalf("the checks were read %d times for one pull request, want 1", n)
	}
	if review := promptOf(t, h, 1); !strings.Contains(review, "CI is green") {
		t.Fatalf("the first review is missing the pre-review read:\n%s", review)
	}
	if review := promptOf(t, h, 3); strings.Contains(review, "## Required checks") {
		t.Fatalf("the second review has a checks section read before the fix:\n%s", review)
	}
}

// TestPreReviewChecksReadOnceAcrossThreeRounds: still once when the reviewer
// keeps asking for changes, up to the escalation at max_review_rounds.
func TestPreReviewChecksReadOnceAcrossThreeRounds(t *testing.T) {
	t.Setenv("FAKE_REVIEW_ALWAYS_CHANGES", "1")
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Never approved")
	h.gh.checks = []checksResponse{{`[{"name":"go / test","bucket":"pass","state":"SUCCESS"}]`, nil}}
	runPreReviewLoop(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1", "developer-issue-1-r2", "reviewer-pr-101-r2",
		"developer-issue-1-r3", "reviewer-pr-101-r3")
	if n := h.gh.callCount("pr checks"); n != 1 {
		t.Fatalf("the checks were read %d times over three review rounds, want 1", n)
	}
	if len(h.gh.comments[1]) != 1 {
		t.Fatalf("want the max_review_rounds escalation, got: %v", h.gh.comments[1])
	}
}

// TestPreReviewChecksOnAResumedWorker: a worker that finds an issue already in
// review still reads the checks. The process has no memory across restarts, so
// the reviewer it runs gets the same checks section a fresh worker would.
func TestPreReviewChecksOnAResumedWorker(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Restarted mid-review")
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:review"}, {Name: "bees:size/s"}}
	// The pull request exists from the start: this worker is a resumption.
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	seedCounter(t, h, "review", 1) // approve on the first round
	h.gh.checks = []checksResponse{{`[{"name":"go / test","bucket":"pass","state":"SUCCESS"}]`, nil}}
	runPreReviewLoop(t, h)

	h.wantOrder("reviewer-pr-101-r1")
	if n := h.gh.callCount("pr checks"); n != 1 {
		t.Fatalf("a resumed worker read the checks %d times, want 1", n)
	}
	if review := promptOf(t, h, 0); !strings.Contains(review, "CI is green") {
		t.Fatalf("the resumed review is missing the checks section:\n%s", review)
	}
}

// TestPreReviewChecksPendingReviewsAnyway: the read is bounded, and a slow CI
// delays a review by pre_review_checks_timeout at most.
func TestPreReviewChecksPendingReviewsAnyway(t *testing.T) {
	h := newHarness(t, prereviewTOML+"[roles.reviewer]\npre_review_checks_timeout = \"1ms\"\n")
	seedPreReviewIssue(t, h, "Slow CI")
	seedCounter(t, h, "review", 1)
	h.gh.checks = []checksResponse{{`[{"name":"slow","bucket":"pending","state":"PENDING"}]`, fmt.Errorf("exit status 8")}}
	runPreReviewLoop(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("pending checks must not escalate before the review: %v", h.gh.comments[1])
	}
	review := promptOf(t, h, 1)
	for _, want := range []string{"## Required checks", "slow — pending", "still pending after `1ms`", "run the repository's test-suite yourself"} {
		if !strings.Contains(review, want) {
			t.Errorf("reviewer prompt missing %q:\n%s", want, review)
		}
	}
}

// TestPreReviewChecksErrorReviewsAnyway: the read is advisory. A `gh` that
// fails outright costs the reviewer its checks section, not its review.
func TestPreReviewChecksErrorReviewsAnyway(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Broken gh")
	seedCounter(t, h, "review", 1)
	h.gh.errFor["pr checks"] = fmt.Errorf("HTTP 503: Service Unavailable")
	runPreReviewLoop(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("a failed checks read must not escalate: %v", h.gh.comments[1])
	}
	if review := promptOf(t, h, 1); strings.Contains(review, "## Required checks") {
		t.Fatalf("reviewer prompt has a checks section after a failed read:\n%s", review)
	}
	if !strings.Contains(h.logs.String(), "could not read the checks; reviewing anyway") {
		t.Fatalf("the failed read is not logged:\n%s", h.logs.String())
	}
	// A read that keeps failing costs every reviewer its checks section
	// silently, so it is a degraded operation like any other.
	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if f := degradedOp(t, st, "pre-review-checks"); f.Count == 0 {
		t.Fatalf("degraded entry: %+v", f)
	}
}

// TestPreReviewChecksDisabled: pre_review_checks = false is exactly the
// behaviour that predates the stage — no read, no checks section.
func TestPreReviewChecksDisabled(t *testing.T) {
	h := newHarness(t, prereviewTOML+"[roles.reviewer]\npre_review_checks = false\n")
	seedPreReviewIssue(t, h, "No pre-review")
	h.gh.checks = []checksResponse{{`[{"name":"go / test","bucket":"fail","state":"FAILURE"}]`, fmt.Errorf("exit status 1")}}
	runPreReviewLoop(t, h)

	// Today's sequence: review round 1 requests changes, round 2 approves.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1", "developer-issue-1-r2", "reviewer-pr-101-r2")
	if n := h.gh.callCount("pr checks"); n != 0 {
		t.Fatalf("the checks were read %d times with pre_review_checks = false", n)
	}
	for i := range h.sessionOrder() {
		if p := promptOf(t, h, i); strings.Contains(p, "## Required checks") {
			t.Fatalf("session %d has a checks section:\n%s", i, p)
		}
	}
}

// TestPreReviewChecksRecoverClearsTheDegradedEntry: the streak is per
// operation and lives across issues, so a read that works again must clear it.
// The read happens once per pull request, so the recovery is the next issue's.
func TestPreReviewChecksRecoverClearsTheDegradedEntry(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Flaky gh")
	h.gh.checks = []checksResponse{{"", fmt.Errorf("HTTP 503: Service Unavailable")}}
	runPreReviewLoop(t, h)

	// Issue 1: the read failed, so its review has no checks section.
	if review := promptOf(t, h, 1); strings.Contains(review, "## Required checks") {
		t.Fatalf("the failed read must not produce a checks section:\n%s", review)
	}
	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if f := degradedOp(t, st, "pre-review-checks"); f.Count == 0 {
		t.Fatalf("degraded entry after the failed read: %+v", f)
	}

	// A second issue, whose read works, clears the streak.
	seedReady(h, 2, "s", time.Now())
	h.gh.checks = []checksResponse{{`[{"name":"go / test","bucket":"pass","state":"SUCCESS"}]`, nil}}
	forcePoll(h)
	runPreReviewLoop(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1", "developer-issue-1-r2", "reviewer-pr-101-r2",
		"developer-issue-2-r1", "reviewer-pr-202-r1")
	if review := promptOf(t, h, 5); !strings.Contains(review, "CI is green") {
		t.Fatalf("the recovered read is missing from the second issue's review:\n%s", review)
	}
	h.sched.writeStatus()
	st, err = h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range st.Degraded {
		if f.Op == "pre-review-checks" {
			t.Fatalf("a read that worked again left a degraded entry: %+v", f)
		}
	}
}

func TestShortDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{10 * time.Minute, "10m"},
		{time.Hour, "1h"},
		{90 * time.Second, "1m30s"},
		{time.Millisecond, "1ms"},
		{0, "0s"},
	} {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("shortDuration(%s) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
