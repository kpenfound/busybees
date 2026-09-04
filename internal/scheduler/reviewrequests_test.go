package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/workspace"
)

// reviewOnlyTOML leaves the reviewer as the one enabled role: a requested
// review is the only thing that can start a session.
const reviewOnlyTOML = baseTOML + `
[roles.developer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

// requestedReviewClock is the fixed time every test here starts at, so a
// second Run inside poll_interval is a local pass by construction.
var requestedReviewClock = time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)

// personsPR is a pull request a person opened on a branch of their own,
// with the label that asks for a review. CreatedAt and UpdatedAt are set
// because a fixture without them is dropped on the floor, and a test that
// then sees no session passes for the wrong reason.
func personsPR(labels ...string) *github.PR {
	created := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	var ls []github.Label
	for _, l := range labels {
		ls = append(ls, github.Label{Name: l})
	}
	return &github.PR{Number: 42, Title: "Fix the widget", Body: "It was broken.", State: "OPEN",
		URL: "https://github.com/acme/widgets/pull/42", HeadRefName: "fix-widget", BaseRefName: "main",
		Author: github.Author{Login: "kyle"}, Labels: ls, CreatedAt: created, UpdatedAt: created}
}

// pushBranch creates branch from main in the clone and pushes it to the
// test origin, so a detached checkout of origin/<branch> can succeed.
func pushBranch(t *testing.T, clone, branch string) {
	t.Helper()
	ctx := context.Background()
	for _, args := range [][]string{{"branch", branch, "main"}, {"push", "-q", "origin", branch}} {
		if _, err := workspace.Git(ctx, clone, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
}

// removeLabelCalls counts the `gh issue edit` calls that removed the label
// from the pull request.
func removeLabelCalls(h *harness, pr string, label string) int {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	n := 0
	for _, c := range h.gh.calls {
		if len(c) >= 3 && c[0] == "issue" && c[1] == "edit" && c[2] == pr {
			for i, a := range c {
				if a == "--remove-label" && i+1 < len(c) && c[i+1] == label {
					n++
				}
			}
		}
	}
	return n
}

func prHasLabel(h *harness, n int, label string) bool {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	return github.HasLabel(h.gh.prs[n].Labels, label)
}

func addPRLabel(h *harness, n int, label string) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	h.gh.prs[n].Labels = append(h.gh.prs[n].Labels, github.Label{Name: label})
}

// A pull request the factory did not write, carrying bees:review-requested,
// gets one reviewer session with no issue behind it, and the label is
// removed before that session starts.
func TestReviewRequestedLabelDispatchesOneReviewer(t *testing.T) {
	h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = personsPR("bees", "bees:review-requested")
	runPass(t, h)

	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions: %d, want 1", got)
	}
	if got := removeLabelCalls(h, "42", "bees:review-requested"); got != 1 {
		t.Errorf("--remove-label bees:review-requested edits on #42: %d, want 1", got)
	}
	if prHasLabel(h, 42, "bees:review-requested") {
		t.Errorf("the pull request still carries bees:review-requested")
	}
	dir := h.sessions(config.RoleReviewer)[0]
	if !strings.Contains(filepath.Base(dir), "reviewer-requested-pr-42") {
		t.Errorf("session name: %s", filepath.Base(dir))
	}
	prompt := promptOf(t, h, 0)
	for _, want := range []string{"# Task: review pull request #42 (requested by a person)", "Fix the widget", "author: kyle", "It was broken.", "branch `fix-widget` → `main`"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "## Issue") {
		t.Errorf("a requested review rendered an issue section:\n%s", prompt)
	}
	// The session had no issue: the fake records its environment through
	// the mail it sends, which carries the PR and no issue.
	msgs, err := h.box.List(mail.Filter{To: config.RoleDeveloper})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || msgs[0].PR != 42 || msgs[0].Issue != 0 {
		t.Errorf("reviewer mail: %+v, want one message about PR 42 and no issue", msgs)
	}
	if !strings.Contains(h.logs.String(), "review requested on a pull request") {
		t.Errorf("no log line about the request:\n%s", h.logs.String())
	}
}

// A visible pull request without the label is not reviewed: the factory
// only ever reviewed the pull requests its developers opened, and a
// person's pull request stays invisible to the reviewer until asked.
func TestNoReviewForAPullRequestWithoutTheLabel(t *testing.T) {
	h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = personsPR("bees")
	runPass(t, h)

	if got := sessionCount(h); got != 0 {
		t.Fatalf("sessions: %d, want 0", got)
	}
	if got := removeLabelCalls(h, "42", "bees:review-requested"); got != 0 {
		t.Errorf("label edits on #42: %d, want 0", got)
	}
}

// A local pass classifies the pull requests cached from the last poll,
// which still carry the label the full pass removed on GitHub: it must not
// dispatch from them, or every review request would run twice.
func TestALocalPassNeverDispatchesARequestedReview(t *testing.T) {
	h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = personsPR("bees", "bees:review-requested")
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions after the poll: %d, want 1", got)
	}
	// The cached list is the stale one: it carries the label the fake no
	// longer has.
	h.sched.mu.Lock()
	cached := h.sched.lastPRs
	h.sched.mu.Unlock()
	if len(cached) != 1 || !github.HasLabel(cached[0].Labels, "bees:review-requested") {
		t.Fatalf("the cached pull request list does not carry the label the test is about: %+v", cached)
	}
	if prHasLabel(h, 42, "bees:review-requested") {
		t.Fatal("the label was not removed on GitHub")
	}

	// Inside poll_interval: a local pass, from the cached list.
	h.clock.advance(100 * time.Millisecond)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions after the local pass: %d, want 1 (dispatched again from the stale cache)", got)
	}
	if got := removeLabelCalls(h, "42", "bees:review-requested"); got != 1 {
		t.Errorf("--remove-label edits on #42: %d, want 1", got)
	}
}

// While a requested review is running, a full pass that sees the label
// again (a person re-added it) does not start a second session on the
// same pull request.
func TestARequestedReviewInFlightIsNotDispatchedTwice(t *testing.T) {
	h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("FAKE_WAIT_FOR", release)
	h.gh.prs[42] = personsPR("bees", "bees:review-requested")
	ctx := context.Background()
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the reviewer session to start", func() bool {
		return len(h.sessions(config.RoleReviewer)) == 1
	})
	h.sched.mu.Lock()
	_, owned := h.sched.owned[42]
	h.sched.mu.Unlock()
	if !owned {
		t.Fatal("the pull request is not in s.owned while its review runs")
	}

	addPRLabel(h, 42, "bees:review-requested")
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Errorf("reviewer sessions with one in flight: %d, want 1", got)
	}
	if !prHasLabel(h, 42, "bees:review-requested") {
		t.Errorf("the re-added label was removed while a review was still running; it is the next pass's request")
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.sched.wg.Wait()
	h.sched.mu.Lock()
	_, owned = h.sched.owned[42]
	h.sched.mu.Unlock()
	if owned {
		t.Error("the pull request is still in s.owned after its review finished")
	}
}

// One label is one pass even when the pass fails: the label is removed
// before the session starts, the failure is logged, and nothing is
// escalated because there is no issue to escalate.
func TestAFailedRequestedReviewStillRemovesTheLabel(t *testing.T) {
	h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	t.Setenv("FAKE_REVIEW_FAIL", "1")
	h.gh.prs[42] = personsPR("bees", "bees:review-requested")
	runPass(t, h)

	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions: %d, want 1", got)
	}
	if prHasLabel(h, 42, "bees:review-requested") {
		t.Errorf("a failed review left bees:review-requested on the pull request")
	}
	if !strings.Contains(h.logs.String(), "requested review failed") {
		t.Errorf("the failure was not logged:\n%s", h.logs.String())
	}
	if got := h.gh.callCount("issue comment"); got != 0 {
		t.Errorf("a failed requested review commented %d times; there is no issue to escalate", got)
	}
	if until, ok := h.sched.backoffUntil(requestedReviewKey(42)); !ok || !until.After(h.clock.now()) {
		t.Errorf("no backoff recorded for the failed review: %v %v", until, ok)
	}
}

// Adding the label again asks for another pass.
func TestReAddingTheLabelAsksForAnotherReview(t *testing.T) {
	h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = personsPR("bees", "bees:review-requested")
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions after the first request: %d, want 1", got)
	}

	// A later poll with the label back on.
	h.clock.advance(time.Hour)
	forcePoll(h)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions with the label gone: %d, want 1", got)
	}

	addPRLabel(h, 42, "bees:review-requested")
	h.clock.advance(time.Hour)
	forcePoll(h)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 2 {
		t.Fatalf("reviewer sessions after the second request: %d, want 2", got)
	}
	if got := removeLabelCalls(h, "42", "bees:review-requested"); got != 2 {
		t.Errorf("--remove-label edits on #42: %d, want 2", got)
	}
	names := []string{}
	for _, d := range h.sessions(config.RoleReviewer) {
		names = append(names, filepath.Base(d))
	}
	if slices.ContainsFunc(names, func(n string) bool { return !strings.Contains(n, "reviewer-requested-pr-42") }) {
		t.Errorf("session names: %v", names)
	}
}

// A head branch the remote does not have (a fork, or a branch deleted
// since) does not fail the dispatch: the review runs from a checkout of
// the default branch and the fallback is logged.
func TestARequestedReviewFallsBackToTheDefaultBranch(t *testing.T) {
	h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
	// No pushBranch: origin/fix-widget does not exist.
	h.gh.prs[42] = personsPR("bees", "bees:review-requested")
	runPass(t, h)

	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions: %d, want 1", got)
	}
	if !strings.Contains(h.logs.String(), "head branch is not on the remote; reviewing from the default branch") {
		t.Errorf("the fallback was not logged:\n%s", h.logs.String())
	}
	if prHasLabel(h, 42, "bees:review-requested") {
		t.Errorf("the label was not removed")
	}
}
