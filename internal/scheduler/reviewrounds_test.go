package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// reviewRoundsTOML is baseTOML with its own max_review_rounds (baseTOML
// already sets one, and TOML forbids a duplicate key) and only the developer
// and reviewer enabled.
func reviewRoundsTOML(rounds int) string {
	return fmt.Sprintf(`
version = 1
[project]
repo = "acme/widgets"
[scheduler]
poll_interval = "1s"
max_developers = 2
max_review_rounds = %d
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
`, rounds)
}

// A person reads the escalation comment on GitHub, so it must say
// "after 1 review round" when max_review_rounds = 1 — and keep the plural
// for every other count.
func TestUnapprovedEscalationSaysOneReviewRound(t *testing.T) {
	for _, c := range []struct {
		rounds int
		want   string
	}{
		{1, "after 1 review round."},
		{2, "after 2 review rounds."},
	} {
		t.Run(fmt.Sprintf("%d", c.rounds), func(t *testing.T) {
			// The reviewer never approves, so the loop runs out of rounds.
			// FAKE_REVIEW_ALWAYS_CHANGES scripts that verdict directly; it
			// leaves unverified whether a model would actually keep finding
			// something to request changes over for every round.
			t.Setenv("FAKE_REVIEW_ALWAYS_CHANGES", "1")
			h := newHarness(t, reviewRoundsTOML(c.rounds))
			h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
				Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
			h.gh.PRs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
				HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := h.sched.Run(ctx); err != nil {
				t.Fatal(err)
			}

			hist := h.gh.History[1]
			if len(hist) == 0 || hist[len(hist)-1] != "bees:needs-human" {
				t.Fatalf("history: %v", hist)
			}
			if len(h.gh.Comments[1]) != 1 {
				t.Fatalf("comments: %v", h.gh.Comments[1])
			}
			body := h.gh.Comments[1][0]
			if !strings.Contains(body, c.want) {
				t.Fatalf("comment does not say %q:\n%s", c.want, body)
			}
			// The reviewer really did use up every round: rounds count from 1,
			// so the escalation fires on the max_review_rounds-th review.
			if got := len(h.sessions(config.RoleReviewer)); got != c.rounds {
				t.Fatalf("reviewer sessions: got %d want %d", got, c.rounds)
			}
		})
	}
}

// TestFinalReviewRoundIsNamedHonestlyInThePrompt: the reviewer's task names
// the round only once it is the last one max_review_rounds allows, and that
// framing is what tells the session it may request changes anyway instead of
// holding out for a round that will never come — an earlier round's prompt
// must not say the loop is out of rounds when it is not.
func TestFinalReviewRoundIsNamedHonestlyInThePrompt(t *testing.T) {
	// FAKE_REVIEW_ALWAYS_CHANGES scripts the reviewer's verdict on both
	// rounds directly; it leaves unverified whether a model reading the
	// final-round framing would actually act on it rather than hold out.
	t.Setenv("FAKE_REVIEW_ALWAYS_CHANGES", "1")
	h := newHarness(t, reviewRoundsTOML(2))
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
		HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1", "developer-issue-1-r2", "reviewer-pr-101-r2")
	if round1 := promptOf(t, h, 1); strings.Contains(round1, "final review round") {
		t.Fatalf("round 1 of 2 was told it was the final round:\n%s", round1)
	}
	round2 := promptOf(t, h, 3)
	if !strings.Contains(round2, "This is the final review round. If a finding still needs fixing, request changes anyway;") {
		t.Fatalf("round 2 of 2 lacks the final-round framing:\n%s", round2)
	}
	// The framing is the only reason an out-of-rounds verdict is still
	// requesting changes rather than silence: the escalation that follows
	// proves the session acted on it instead of holding the pull request
	// open for a round max_review_rounds will never grant.
	if last := h.gh.History[1][len(h.gh.History[1])-1]; last != "bees:needs-human" {
		t.Fatalf("history: %v", h.gh.History[1])
	}
}
