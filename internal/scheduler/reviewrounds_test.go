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
			t.Setenv("FAKE_REVIEW_ALWAYS_CHANGES", "1")
			h := newHarness(t, reviewRoundsTOML(c.rounds))
			h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
				Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
			h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
				HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := h.sched.Run(ctx); err != nil {
				t.Fatal(err)
			}

			hist := h.gh.history[1]
			if len(hist) == 0 || hist[len(hist)-1] != "bees:needs-human" {
				t.Fatalf("history: %v", hist)
			}
			if len(h.gh.comments[1]) != 1 {
				t.Fatalf("comments: %v", h.gh.comments[1])
			}
			body := h.gh.comments[1][0]
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
