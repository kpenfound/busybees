package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/session"
)

const notifyTOML = baseTOML + "notify = [\"kpenfound\"]\n"

// requestedReviewers returns the review-request calls the fake gh recorded.
func requestedReviewers(h *harness) [][]string {
	var out [][]string
	for _, c := range h.gh.calls {
		if len(c) > 0 && c[0] == "api" && strings.Contains(strings.Join(c, " "), "/requested_reviewers") {
			out = append(out, c)
		}
	}
	return out
}

// escalate mentions scheduler.notify: by default the factory and the people it
// works for share one GitHub account, so a comment notifies nobody by itself.
func TestEscalationMentionsNotify(t *testing.T) {
	h := newHarness(t, notifyTOML)
	h.gh.issues[12] = &github.Issue{Number: 12, Title: "Build the thing", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:in-progress"}}}

	res := &session.Result{IsError: true, ErrorSubtype: "error_during_execution", ResultText: "it broke"}
	status, note := outcomeOf(res)
	if err := h.sched.escalate(context.Background(), 12, h.sched.sessionFailure(config.RoleDeveloper, res, status, note)); err != nil {
		t.Fatal(err)
	}

	if len(h.gh.comments[12]) != 1 {
		t.Fatalf("comments: %v", h.gh.comments[12])
	}
	body := h.gh.comments[12][0]
	if !strings.HasPrefix(body, "@kpenfound\n\n🐝 **busybees needs a human.**") {
		t.Errorf("escalation comment does not start with the mentions:\n%s", body)
	}
	if !strings.Contains(body, "it broke") {
		t.Errorf("escalation comment lost its reason:\n%s", body)
	}
}

// With notify unset the comment is exactly what it always was.
func TestEscalationWithoutNotify(t *testing.T) {
	h := newHarness(t, baseTOML)
	h.gh.issues[12] = &github.Issue{Number: 12, Title: "Build the thing", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:in-progress"}}}

	if err := h.sched.escalate(context.Background(), 12, "it broke"); err != nil {
		t.Fatal(err)
	}
	if len(h.gh.comments[12]) != 1 || !strings.HasPrefix(h.gh.comments[12][0], "🐝 **busybees needs a human.**") {
		t.Fatalf("comments: %v", h.gh.comments[12])
	}
	if strings.Contains(h.gh.comments[12][0], "@") {
		t.Errorf("notify is unset but the comment mentions somebody:\n%s", h.gh.comments[12][0])
	}
}

// An approved pull request waits for a person to merge it, so everyone in
// scheduler.notify is asked to review it.
func TestApprovedPRRequestsAReview(t *testing.T) {
	h := newHarness(t, baseTOML+"notify = [\"kpenfound\", \"myorg/bees-team\"]\n")
	h.gh.issues[1] = &github.Issue{Number: 1, State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:review"}}}
	pr := &github.PR{Number: 101, State: "OPEN", HeadRefName: "bees/issue-1", Labels: []github.Label{{Name: "bees"}}}
	h.gh.prs[101] = pr

	if err := h.sched.approve(context.Background(), 1, pr); err != nil {
		t.Fatal(err)
	}

	calls := requestedReviewers(h)
	if len(calls) != 1 {
		t.Fatalf("want one review request, got %v", calls)
	}
	got := strings.Join(calls[0], " ")
	// The login goes in reviewers[], the org/team's slug in team_reviewers[].
	for _, want := range []string{"-X POST", "repos/acme/widgets/pulls/101/requested_reviewers", "reviewers[]=kpenfound", "team_reviewers[]=bees-team"} {
		if !strings.Contains(got, want) {
			t.Errorf("review request %q does not contain %q", got, want)
		}
	}
	if !github.HasLabel(h.gh.issues[1].Labels, "bees:approved") {
		t.Errorf("issue labels: %v", h.gh.issues[1].Labels)
	}
}

// Requesting a review is best effort: GitHub refuses one from the pull
// request's own author, and with a shared account the configured login often
// is the author. A failure must not hold the approval back.
func TestFailedReviewRequestStillApproves(t *testing.T) {
	h := newHarness(t, notifyTOML)
	h.gh.errFor["requested_reviewers"] = errors.New("HTTP 422: Review cannot be requested from pull request author")
	h.gh.issues[1] = &github.Issue{Number: 1, State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:review"}}}
	pr := &github.PR{Number: 101, State: "OPEN", HeadRefName: "bees/issue-1", Labels: []github.Label{{Name: "bees"}}}
	h.gh.prs[101] = pr

	if err := h.sched.approve(context.Background(), 1, pr); err != nil {
		t.Fatalf("a failed review request must not fail the approval: %v", err)
	}
	if !github.HasLabel(h.gh.issues[1].Labels, "bees:approved") {
		t.Errorf("issue labels: %v", h.gh.issues[1].Labels)
	}
	if !github.HasLabel(h.gh.prs[101].Labels, "bees:approved") {
		t.Errorf("pr labels: %v", h.gh.prs[101].Labels)
	}
	if !strings.Contains(h.logs.String(), "could not request a review") {
		t.Errorf("the failure was not warned about:\n%s", h.logs.String())
	}
}

// With notify unset nobody is asked to review.
func TestApprovedPRWithoutNotify(t *testing.T) {
	h := newHarness(t, baseTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:review"}}}
	pr := &github.PR{Number: 101, State: "OPEN", HeadRefName: "bees/issue-1", Labels: []github.Label{{Name: "bees"}}}
	h.gh.prs[101] = pr

	if err := h.sched.approve(context.Background(), 1, pr); err != nil {
		t.Fatal(err)
	}
	if calls := requestedReviewers(h); len(calls) != 0 {
		t.Errorf("notify is unset but a review was requested: %v", calls)
	}
	if !github.HasLabel(h.gh.issues[1].Labels, "bees:approved") {
		t.Errorf("issue labels: %v", h.gh.issues[1].Labels)
	}
}

// The mentions reach a real session, not just `bees prompts show`: the
// product manager's system prompt is rendered from prompts.Data.Notify, which
// runSession fills from the config.
func TestProductManagerSessionIsToldTheMentions(t *testing.T) {
	h := newHarness(t, notifyTOML+"\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	now := time.Now()
	h.gh.issues[3] = &github.Issue{Number: 3, Title: "Dark mode please", Body: "idea", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feedback"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	pm := h.sessions(config.RoleProductManager)
	if len(pm) != 1 {
		t.Fatalf("product manager sessions: %d", len(pm))
	}
	prompt, err := os.ReadFile(filepath.Join(pm[0], "system-prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "Start the comment with `@kpenfound`") {
		t.Fatalf("product manager system prompt does not carry the mentions:\n%s", prompt)
	}
}
