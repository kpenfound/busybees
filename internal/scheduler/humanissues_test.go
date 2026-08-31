package scheduler

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/state"
)

// seedIssueComments serves comments from the fake gh for an issue's own
// comment endpoint — the one github.Client.IssueActivity reads. It is a
// different path from the pull request's conversation comments
// (repos/.../issues/<pr>/comments), which is what lets one test drive both
// streams on one work item.
func seedIssueComments(h *harness, n int, comments ...string) {
	h.gh.activity[fmt.Sprintf("repos/acme/widgets/issues/%d/comments", n)] = strings.Join(comments, ",\n")
}

// issueComment renders one comment as the fake gh serves it.
func issueComment(id int, author, body string, at time.Time) string {
	return fmt.Sprintf(`{"id": %d, "user": {"login": %q}, "body": %q, "html_url": "https://x/c/%d", "created_at": %q}`,
		id, author, body, id, at.UTC().Format(time.RFC3339))
}

// roleMail lists every message in a role's inbox, oldest first.
func roleMail(t *testing.T, h *harness, role string) []mail.Message {
	t.Helper()
	msgs, err := h.box.List(mail.Filter{To: role})
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// deliverIssueCommentsOnce polls the fake GitHub and runs the issue-comment
// delivery once, the way a full pass does, without dispatching anything.
func deliverIssueCommentsOnce(t *testing.T, h *harness) *snapshot {
	t.Helper()
	ctx := context.Background()
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.sched.deliverHumanIssueComments(ctx, snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

// TestAnIssueSeenInFlightForTheFirstTimeDeliversNothing is decision 2 of #304:
// a zero issue_human_seen_at must not mean "deliver every comment this issue
// ever received", which on the first tick after an upgrade would replay the
// project manager's whole triage conversation. The first pass records the
// poll time and says nothing; delivery starts from what is written after it.
func TestAnIssueSeenInFlightForTheFirstTimeDeliversNothing(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, noRolesTOML, now)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Under way", State: "OPEN",
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:in-progress"}, {Name: "bees:size/s"}},
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	seedIssueComments(h, 1, issueComment(901, "kyle", "this was said during triage", now.Add(-90*time.Minute)))

	deliverIssueCommentsOnce(t, h)

	if msgs := developerMail(t, h); len(msgs) != 0 {
		t.Fatalf("the first observation delivered %d messages, want none: %+v", len(msgs), msgs)
	}
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if !bk.IssueHumanSeenAt.Equal(now.UTC()) {
		t.Fatalf("issue_human_seen_at is %v, want the poll time %v", bk.IssueHumanSeenAt, now.UTC())
	}
	// Seeding costs no GitHub call either: the comments were never fetched.
	if n := h.gh.callCount("api --paginate"); n != 0 {
		t.Fatalf("%d comment fetches on the first observation, want none", n)
	}

	// A comment written after the seed does reach the developer, and a quiet
	// issue still costs no call.
	h.gh.issues[1].UpdatedAt = now.Add(time.Minute)
	seedIssueComments(h, 1,
		issueComment(901, "kyle", "this was said during triage", now.Add(-90*time.Minute)),
		issueComment(902, "kyle", "use the flag names the issue already lists", now.Add(time.Minute)))
	deliverIssueCommentsOnce(t, h)

	msgs := developerMail(t, h)
	if len(msgs) != 1 {
		t.Fatalf("%d messages after the fresh comment, want 1: %+v", len(msgs), msgs)
	}
	if !strings.Contains(msgs[0].Body, "use the flag names the issue already lists") {
		t.Errorf("the fresh comment was not delivered:\n%s", msgs[0].Body)
	}
	if strings.Contains(msgs[0].Body, "this was said during triage") {
		t.Errorf("a comment older than the seed was replayed:\n%s", msgs[0].Body)
	}
}

// TestAQuietIssueCostsNoCommentFetch: the delivery is gated on the issue's
// updatedAt, so an in-flight issue nobody has touched adds nothing to the
// per-poll API budget.
func TestAQuietIssueCostsNoCommentFetch(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, noRolesTOML, now)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Quiet", State: "OPEN",
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:review"}, {Name: "bees:size/s"}},
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now.Add(-time.Hour)}
	if err := h.store.SetIssueHumanSeenAt(1, now.Add(-30*time.Minute)); err != nil {
		t.Fatal(err)
	}
	seedIssueComments(h, 1, issueComment(901, "kyle", "old news", now.Add(-90*time.Minute)))

	deliverIssueCommentsOnce(t, h)

	if n := h.gh.callCount("api --paginate"); n != 0 {
		t.Fatalf("%d comment fetches for a quiet issue, want none", n)
	}
	if msgs := developerMail(t, h); len(msgs) != 0 {
		t.Fatalf("a quiet issue delivered %+v", msgs)
	}
}

// TestACommentOnABlockedIssueGoesToWhoeverIsWaiting is decision 3 of #304.
// reconcile lifts bees:blocked by recipient — developer mail means the
// question was a developer's and the issue is ready, project-manager mail
// means it was triage's and the issue goes back to triage — so mailing the
// developer whatever the block was about would hand an unrefined issue to a
// developer. The developer worker's bookkeeping says which it was: a branch
// or a pull request means a developer session asked.
func TestACommentOnABlockedIssueGoesToWhoeverIsWaiting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		bk        *state.IssueState
		wantTo    string
		wantState string
	}{
		{
			name:      "blocked out of a developer session",
			bk:        &state.IssueState{Number: 1, Round: 1, Branch: "bees/issue-1"},
			wantTo:    config.RoleDeveloper,
			wantState: "ready",
		},
		{
			name:      "blocked out of triage",
			bk:        nil,
			wantTo:    config.RoleProjectManager,
			wantState: "triage",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			h := newHarnessAt(t, noRolesTOML, now)
			h.gh.issues[1] = &github.Issue{Number: 1, Title: "Waiting on an answer", State: "OPEN",
				Labels:    []github.Label{{Name: "bees"}, {Name: "bees:blocked"}, {Name: "bees:size/s"}},
				CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now}
			if tc.bk != nil {
				if err := h.store.SaveIssue(*tc.bk); err != nil {
					t.Fatal(err)
				}
			}
			if err := h.store.SetIssueHumanSeenAt(1, now.Add(-time.Hour)); err != nil {
				t.Fatal(err)
			}
			seedIssueComments(h, 1, issueComment(901, "kyle", "yes, keep both flags", now))

			snap := deliverIssueCommentsOnce(t, h)

			msgs := roleMail(t, h, tc.wantTo)
			if len(msgs) != 1 {
				t.Fatalf("%d messages for %s, want 1: %+v", len(msgs), tc.wantTo, msgs)
			}
			if msgs[0].From != HumanSender || msgs[0].Issue != 1 {
				t.Errorf("mail: %+v", msgs[0])
			}
			if !strings.Contains(msgs[0].Body, "yes, keep both flags") {
				t.Errorf("mail body missing the comment:\n%s", msgs[0].Body)
			}
			for _, other := range []string{config.RoleDeveloper, config.RoleProjectManager} {
				if other == tc.wantTo {
					continue
				}
				if got := roleMail(t, h, other); len(got) != 0 {
					t.Errorf("%s was mailed too: %+v", other, got)
				}
			}
			// The mail is the answer that lifts bees:blocked, and reconcile
			// runs after this in the same pass.
			if err := h.sched.reconcile(context.Background(), snap); err != nil {
				t.Fatal(err)
			}
			if got := h.stateOfIssue(1); got != tc.wantState {
				t.Fatalf("issue state is %q, want %q", got, tc.wantState)
			}
		})
	}
}

// TestPRAndIssueCommentsAreBothDelivered: the two streams have a clock each
// (human_seen_at and issue_human_seen_at) precisely so neither suppresses the
// other. A person who comments on the pull request and on the issue in the
// same breath must have both reach the developer.
//
// It drives the whole pass rather than the two deliveries by hand, because
// their *order* is load-bearing: the issue here is approved, and PR feedback
// on an approved issue runs reopenApproved, which takes it out of every
// in-flight bucket the issue-comment loop reads.
func TestPRAndIssueCommentsAreBothDelivered(t *testing.T) {
	ctx := context.Background()
	now := time.Now()
	h := newHarnessAt(t, noRolesTOML, now)
	seedApprovedPR(t, h, "MERGEABLE", "CLEAN", "aaa")
	h.gh.issues[1].UpdatedAt = now
	h.gh.prs[fakePR].UpdatedAt = now
	if err := h.store.SetIssueHumanSeenAt(1, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	seedIssueComments(h, 1, issueComment(901, "kyle", "hold off, the API is changing", now))
	h.gh.activity["repos/acme/widgets/pulls/101/comments"] = issueComment(555, "kyle", "and rename this variable", now)

	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}

	msgs := developerMail(t, h)
	if len(msgs) != 2 {
		t.Fatalf("%d messages in the developer's inbox, want the issue comment and the PR feedback: %+v", len(msgs), msgs)
	}
	var sawIssue, sawPR bool
	for _, m := range msgs {
		if strings.Contains(m.Body, "hold off, the API is changing") {
			sawIssue = true
		}
		if strings.Contains(m.Body, "and rename this variable") {
			sawPR = true
		}
	}
	if !sawIssue || !sawPR {
		t.Fatalf("issue comment delivered: %v, PR feedback delivered: %v — one clock suppressed the other", sawIssue, sawPR)
	}
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.HumanSeenAt.IsZero() || bk.IssueHumanSeenAt.IsZero() {
		t.Fatalf("both clocks must be recorded: %+v", bk)
	}
}

// TestACommentOnAnIssueInReviewReachesTheDeveloperAndTheReviewer: during a
// review the developer is not the only bee who needs a person's direction —
// the reviewer is about to rule on the pull request. Both are mailed, and the
// reviewer session renders it.
//
// A bee's own comment is never delivered by either mechanism, and a person
// quoting one still is: the marker rule is positional.
func TestACommentOnAnIssueInReviewReachesTheDeveloperAndTheReviewer(t *testing.T) {
	now := time.Now()
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Steered mid-review")
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:review"}, {Name: "bees:size/s"}}
	h.gh.issues[1].UpdatedAt = now
	// The pull request exists from the start: this worker is a resumption.
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveIssue(state.IssueState{Number: 1, Round: 1, PR: fakePR, Branch: "bees/issue-1",
		WorkerStage: "review", AfterDevelop: "review", PreReviewDone: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SetIssueHumanSeenAt(1, now.Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	seedIssueComments(h, 1,
		issueComment(901, "kyle", "keep the CLI flag names as they are", now),
		issueComment(902, "kyle", "on it\n\n<!-- bees:developer -->", now),
		issueComment(903, "kyle", "Quoting the bot:\n> <!-- bees:developer -->\n\nand ship it behind a flag", now))
	seedCounter(t, h, "review", 1) // approve on the first round
	h.gh.checks = []checksResponse{{passingJSON, nil}}

	runPreReviewLoop(t, h)

	h.wantOrder("reviewer-pr-101-r1")
	for _, tc := range []struct {
		role   string
		wantPR int
	}{
		{config.RoleDeveloper, 0},
		{config.RoleReviewer, fakePR},
	} {
		msgs := roleMail(t, h, tc.role)
		if len(msgs) != 1 {
			t.Fatalf("%d messages for %s, want 1: %+v", len(msgs), tc.role, msgs)
		}
		m := msgs[0]
		if m.From != HumanSender || m.Issue != 1 || m.PR != tc.wantPR {
			t.Errorf("%s mail: %+v", tc.role, m)
		}
		if m.Subject != "Comment on issue #1 from kyle" {
			t.Errorf("%s subject: %q", tc.role, m.Subject)
		}
		for _, want := range []string{"keep the CLI flag names as they are", "and ship it behind a flag"} {
			if !strings.Contains(m.Body, want) {
				t.Errorf("%s mail body missing %q:\n%s", tc.role, want, m.Body)
			}
		}
		if strings.Contains(m.Body, "on it") {
			t.Errorf("%s was sent a bee's own comment:\n%s", tc.role, m.Body)
		}
	}
	// The reviewer session that ran renders it.
	section := mailSection(t, promptOf(t, h, 0))
	for _, want := range []string{"Comment on issue #1 from kyle", "keep the CLI flag names as they are"} {
		if !strings.Contains(section, want) {
			t.Errorf("the reviewer's prompt is missing %q:\n%s", want, section)
		}
	}
	if unread, _ := h.box.List(mail.Filter{To: config.RoleReviewer, UnreadOnly: true}); len(unread) != 0 {
		t.Errorf("reviewer mail left unread: %+v", unread)
	}
}
