package scheduler

import (
	"context"
	"encoding/json"
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
	for _, want := range []string{"## No issue, no acceptance criteria", "## Review stages", "### `implementation`", "`submit_review`"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if sys := systemPromptOf(t, h, 0); !strings.Contains(sys, "submitted with `submit_review`") || strings.Contains(sys, "Do not submit a GitHub review") {
		t.Errorf("the system prompt is not the requested-review one:\n%s", sys)
	}
	// The session had no issue and no developer to mail: its verdict is one
	// GitHub review on the pull request, and the record it leaves carries
	// no issue number.
	rev := reviewOf(t, dir)
	if rev.issue != "" {
		t.Errorf("the session ran with BEES_ISSUE=%q; a requested review has no issue", rev.issue)
	}
	if got, want := strings.Join(rev.args, " "), "pr review 42 -R acme/widgets --approve --body-file -"; got != want {
		t.Errorf("gh call: %q, want %q", got, want)
	}
	if role, ok := github.BeeRole(rev.body); !ok || role != config.RoleReviewer {
		t.Errorf("the review body does not end with the reviewer's marker:\n%s", rev.body)
	}
	if msgs, err := h.box.List(mail.Filter{To: config.RoleDeveloper}); err != nil || len(msgs) != 0 {
		t.Errorf("a requested review mailed the developer: %+v %v", msgs, err)
	}
	if !strings.Contains(h.logs.String(), "requested review finished") {
		t.Errorf("no log line about the outcome:\n%s", h.logs.String())
	}
	if !strings.Contains(h.logs.String(), "review requested on a pull request") {
		t.Errorf("no log line about the request:\n%s", h.logs.String())
	}
}

// review is the gh call the fake reviewer recorded when it submitted its
// review: the argument list, the body it sent on stdin, and the BEES_ISSUE
// it ran with.
type review struct {
	args  []string
	body  string
	issue string
}

func reviewOf(t *testing.T, sessionDir string) review {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sessionDir, "review.json"))
	if err != nil {
		t.Fatalf("the session submitted no review: %v", err)
	}
	var rec struct {
		Args  []string `json:"args"`
		Stdin string   `json:"stdin"`
		Issue string   `json:"issue"`
	}
	if err := json.Unmarshal(b, &rec); err != nil {
		t.Fatal(err)
	}
	return review{args: rec.Args, body: rec.Stdin, issue: rec.Issue}
}

// The verdict is approve when every stage passed, request-changes when one
// failed, and comment in place of approve when the pull request's author is
// the login the factory acts as, which GitHub would refuse to approve. The
// prompt tells the reviewer that login; the fake reads it there.
func TestARequestedReviewsVerdictIsTheEvent(t *testing.T) {
	for name, tc := range map[string]struct {
		actsAs  string
		changes bool
		want    string
		prompt  string
	}{
		"shared account":  {"", false, "--approve", "The factory has no GitHub account of its own"},
		"its own account": {"busybees-bot", false, "--approve", "The factory acts as `busybees-bot` on GitHub. The pull request's author is `kyle`"},
		"the author":      {"kyle", false, "--comment", "The factory acts as `kyle` on GitHub. That is this pull request's author"},
		// GitHub logins are case-insensitive: [github].login as a person
		// typed it against the author as GitHub spells it.
		"the author, other casing": {"Kyle", false, "--comment", "The factory acts as `Kyle` on GitHub. That is this pull request's author"},
		"a failed stage":           {"busybees-bot", true, "--request-changes", ""},
		"the author, failed":       {"kyle", true, "--request-changes", ""},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
			pushBranch(t, h.clone, "fix-widget")
			// What github.NewAs sets from [github].login.
			h.sched.gh.ActsAs = tc.actsAs
			if tc.changes {
				t.Setenv("FAKE_REVIEW_ALWAYS_CHANGES", "1")
			}
			h.gh.prs[42] = personsPR("bees", "bees:review-requested")
			runPass(t, h)
			dirs := h.sessions(config.RoleReviewer)
			if len(dirs) != 1 {
				t.Fatalf("reviewer sessions: %d, want 1", len(dirs))
			}
			if tc.prompt != "" && !strings.Contains(promptOf(t, h, 0), tc.prompt) {
				t.Errorf("prompt lacks %q:\n%s", tc.prompt, promptOf(t, h, 0))
			}
			rev := reviewOf(t, dirs[0])
			if got, want := strings.Join(rev.args, " "), "pr review 42 -R acme/widgets "+tc.want+" --body-file -"; got != want {
				t.Errorf("gh call: %q, want %q", got, want)
			}
			if !strings.Contains(h.logs.String(), "requested review finished") || strings.Contains(h.logs.String(), "requested review failed") {
				t.Errorf("outcome log:\n%s", h.logs.String())
			}
			if got := h.gh.callCount("issue comment"); got != 0 {
				t.Errorf("%d comments posted; a requested review escalates nothing", got)
			}
		})
	}
}

// The review the factory submits on a person's pull request must not come
// back in as feedback: deliverHumanFeedback reads reviews and comments only
// on a pull request that closes a visible factory issue, and a person's
// closes none, so it never asks GitHub about it.
func TestAPersonsPullRequestIsNeverHumanFeedback(t *testing.T) {
	h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
	pr := personsPR("bees")
	// Something happened on it since it was opened: a review, from a
	// person or from the factory, either of which would be mailed to a
	// developer if the pull request were the factory's.
	pr.UpdatedAt = time.Date(2026, 9, 2, 9, 0, 0, 0, time.UTC)
	h.gh.prs[42] = pr
	at := pr.UpdatedAt.Format(time.RFC3339)
	h.gh.activity["repos/acme/widgets/pulls/42/reviews"] = `[
		{"id": 1, "user": {"login": "kyle"}, "body": "looks wrong", "state": "CHANGES_REQUESTED", "html_url": "https://x/1", "submitted_at": "` + at + `"},
		{"id": 2, "user": {"login": "kyle"}, "body": "implementation: pass\n\n<!-- bees:reviewer -->", "state": "APPROVED", "html_url": "https://x/2", "submitted_at": "` + at + `"}
	]`
	h.gh.activity["repos/acme/widgets/pulls/42/comments"] = `[
		{"id": 3, "user": {"login": "kyle"}, "body": "and this", "path": "a.go", "line": 1, "html_url": "https://x/3", "created_at": "` + at + `"}
	]`
	h.gh.activity["repos/acme/widgets/issues/42/comments"] = `[
		{"id": 4, "user": {"login": "kyle"}, "body": "ping", "html_url": "https://x/4", "created_at": "` + at + `"}
	]`
	runPass(t, h)

	if msgs, err := h.box.List(mail.Filter{From: HumanSender}); err != nil || len(msgs) != 0 {
		t.Errorf("human feedback mailed for a person's pull request: %+v %v", msgs, err)
	}
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	for _, c := range h.gh.calls {
		if len(c) >= 2 && c[0] == "api" && strings.Contains(c[1], "/42/") {
			t.Errorf("the activity of a person's pull request was read: gh %s", strings.Join(c, " "))
		}
	}
}

// Without the label, and with scheduler.review_assigned_prs off, a visible
// pull request is not reviewed: the factory reviews the pull requests its
// developers opened, and a person's stays invisible to the reviewer until
// asked.
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

// assignedReviewTOML turns scheduler.review_assigned_prs on and restricts the
// filter to the factory's assignee, the shape the key is meant for.
// assignedOffTOML and assignedAbsentTOML are the other two readings of the
// key: written out as false, and not written at all.
var (
	assignedReviewTOML = assignedTOML("review_assigned_prs = true")
	assignedOffTOML    = assignedTOML("review_assigned_prs = false")
	assignedAbsentTOML = assignedTOML("")
)

// assignedTOML is reviewOnlyTOML with filter.assignee set and one extra line
// in the existing [scheduler] table: a second [scheduler] header would be a
// duplicate table.
func assignedTOML(schedulerLine string) string {
	s := strings.Replace(reviewOnlyTOML, "max_review_rounds = 3\n", "max_review_rounds = 3\n"+schedulerLine+"\n", 1)
	return s + "\n[filter]\nassignee = \"beebot\"\n"
}

// assignedPR is a person's pull request assigned to the factory: the same
// fixture as personsPR with the assignee and a head commit, which is what
// the assignment trigger reads.
func assignedPR(labels ...string) *github.PR {
	pr := personsPR(labels...)
	pr.Assignees = []github.Author{{Login: "beebot"}}
	pr.HeadSHA = "aaa1111"
	return pr
}

// reviewedSHA is the head the scheduler recorded for a pull request.
func reviewedSHA(t *testing.T, h *harness, n int) string {
	t.Helper()
	is, err := h.store.Issue(n)
	if err != nil {
		t.Fatal(err)
	}
	return is.ReviewedSHA
}

// Without the key, an assigned pull request carrying no label is not
// reviewed: today's behaviour, unchanged.
func TestAnAssignedPullRequestIsNotReviewedByDefault(t *testing.T) {
	h := newHarnessAt(t, assignedAbsentTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = assignedPR("bees")
	runPass(t, h)

	if got := sessionCount(h); got != 0 {
		t.Fatalf("sessions: %d, want 0", got)
	}
	if got := reviewedSHA(t, h, 42); got != "" {
		t.Errorf("a head was recorded with the key off: %q", got)
	}
}

// review_assigned_prs = false is the default written out: it must behave
// exactly like the key being absent.
func TestReviewAssignedPRsExplicitlyOffDispatchesNothing(t *testing.T) {
	h := newHarnessAt(t, assignedOffTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = assignedPR("bees")
	runPass(t, h)

	if got := sessionCount(h); got != 0 {
		t.Fatalf("sessions: %d, want 0", got)
	}
}

// With the key on, an assigned pull request the factory did not write gets
// one reviewer session — the same requested-review session the label
// starts — and its head is recorded. A second pass over the same
// unchanged pull request dispatches nothing.
func TestAnAssignedPullRequestIsReviewedOncePerHead(t *testing.T) {
	h := newHarnessAt(t, assignedReviewTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = assignedPR("bees")
	runPass(t, h)

	dirs := h.sessions(config.RoleReviewer)
	if len(dirs) != 1 {
		t.Fatalf("reviewer sessions: %d, want 1", len(dirs))
	}
	if !strings.Contains(filepath.Base(dirs[0]), "reviewer-requested-pr-42") {
		t.Errorf("session name: %s", filepath.Base(dirs[0]))
	}
	// The same session the label starts: the requested-review task, in
	// ModeRequested.
	prompt := promptOf(t, h, 0)
	for _, want := range []string{"# Task: review pull request #42 (requested by a person)", "## No issue, no acceptance criteria", "`submit_review`"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if got := reviewedSHA(t, h, 42); got != "aaa1111" {
		t.Errorf("recorded head: %q, want aaa1111", got)
	}
	// Nothing was labelled or unlabelled: no label was involved.
	if got := removeLabelCalls(h, "42", "bees:review-requested"); got != 0 {
		t.Errorf("label edits on #42: %d, want 0", got)
	}

	h.clock.advance(time.Hour)
	forcePoll(h)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions after a second pass over the same head: %d, want 1", got)
	}
}

// A push earns exactly one further review.
func TestAPushToAnAssignedPullRequestEarnsAnotherReview(t *testing.T) {
	h := newHarnessAt(t, assignedReviewTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = assignedPR("bees")
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions: %d, want 1", got)
	}

	h.gh.mu.Lock()
	h.gh.prs[42].HeadSHA = "bbb2222"
	h.gh.mu.Unlock()
	h.clock.advance(time.Hour)
	forcePoll(h)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 2 {
		t.Fatalf("reviewer sessions after a push: %d, want 2", got)
	}
	if got := reviewedSHA(t, h, 42); got != "bbb2222" {
		t.Errorf("recorded head: %q, want bbb2222", got)
	}

	// And the new head is reviewed once, not on every poll after it.
	h.clock.advance(time.Hour)
	forcePoll(h)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 2 {
		t.Fatalf("reviewer sessions after a third pass: %d, want 2", got)
	}
}

// A scheduler that comes back up with a head already recorded on disk does
// not review it again.
func TestARecordedHeadSurvivesARestart(t *testing.T) {
	h := newHarnessAt(t, assignedReviewTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = assignedPR("bees")
	if err := h.store.SetReviewedSHA(42, "aaa1111"); err != nil {
		t.Fatal(err)
	}
	runPass(t, h)

	if got := len(h.sessions(config.RoleReviewer)); got != 0 {
		t.Fatalf("reviewer sessions: %d, want 0", got)
	}
}

// A pull request the factory wrote is never dispatched by the assignment
// trigger, whatever the key says: its head branch carries the branch
// prefix, and it goes through the developer worker's own review loop.
func TestTheFactorysOwnPullRequestIsNotReviewedByAssignment(t *testing.T) {
	h := newHarnessAt(t, assignedReviewTOML, requestedReviewClock)
	pushBranch(t, h.clone, "bees/issue-7")
	pr := assignedPR("bees")
	pr.HeadRefName = "bees/issue-7"
	h.gh.prs[42] = pr
	runPass(t, h)

	if got := sessionCount(h); got != 0 {
		t.Fatalf("sessions: %d, want 0", got)
	}
	if got := reviewedSHA(t, h, 42); got != "" {
		t.Errorf("a head was recorded for the factory's own pull request: %q", got)
	}
}

// A draft is not ready for review: it is skipped, and undrafting it at the
// same head then dispatches once.
func TestADraftAssignedPullRequestIsNotReviewedUntilItIsReady(t *testing.T) {
	h := newHarnessAt(t, assignedReviewTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	pr := assignedPR("bees")
	pr.IsDraft = true
	h.gh.prs[42] = pr
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 0 {
		t.Fatalf("reviewer sessions on a draft: %d, want 0", got)
	}

	h.gh.mu.Lock()
	h.gh.prs[42].IsDraft = false
	h.gh.mu.Unlock()
	h.clock.advance(time.Hour)
	forcePoll(h)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions once it is ready: %d, want 1", got)
	}
}

// The label trigger keeps its own semantics: it is dispatched even when the
// head has already been reviewed, and it records the head too, so removing
// the label and leaving the assignment does not immediately produce a
// second review of the same head.
func TestTheLabelIsDispatchedOverAnAlreadyReviewedHead(t *testing.T) {
	h := newHarnessAt(t, assignedReviewTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = assignedPR("bees", "bees:review-requested")
	if err := h.store.SetReviewedSHA(42, "aaa1111"); err != nil {
		t.Fatal(err)
	}
	runPass(t, h)

	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions: %d, want 1", got)
	}
	if got := removeLabelCalls(h, "42", "bees:review-requested"); got != 1 {
		t.Errorf("--remove-label edits on #42: %d, want 1", got)
	}
}

// The label trigger records the head with the key off too, so turning the
// key on later does not re-review a head the label has already had.
func TestTheLabelTriggerRecordsTheHeadWithTheKeyOff(t *testing.T) {
	h := newHarnessAt(t, reviewOnlyTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = assignedPR("bees", "bees:review-requested")
	runPass(t, h)

	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions: %d, want 1", got)
	}
	if got := removeLabelCalls(h, "42", "bees:review-requested"); got != 1 {
		t.Errorf("--remove-label edits on #42: %d, want 1", got)
	}
	if got := reviewedSHA(t, h, 42); got != "aaa1111" {
		t.Errorf("recorded head: %q, want aaa1111", got)
	}
}

// The assignment trigger runs from a full pass only, like the label one: a
// local pass classifies pull requests cached from the last poll.
func TestALocalPassNeverDispatchesAnAssignedReview(t *testing.T) {
	h := newHarnessAt(t, assignedReviewTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = assignedPR("bees")
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions after the poll: %d, want 1", got)
	}
	// The cached list still carries the head the pass just recorded; a
	// local pass that dispatched from it would review it twice. Forget the
	// record, so only the full/local distinction can stop a second session.
	if err := h.store.SetReviewedSHA(42, ""); err != nil {
		t.Fatal(err)
	}

	h.clock.advance(100 * time.Millisecond)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Fatalf("reviewer sessions after the local pass: %d, want 1", got)
	}
}

// A record that cannot be read cannot be written either, so reviewing on
// the strength of it would pay for the same review on every poll.
func TestAnUnreadableRecordDoesNotReviewOnEveryPoll(t *testing.T) {
	h := newHarnessAt(t, assignedReviewTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	h.gh.prs[42] = assignedPR("bees")
	p := filepath.Join(h.store.Dir, "issues", "42.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		runPass(t, h)
		h.clock.advance(time.Hour)
		forcePoll(h)
	}
	if got := len(h.sessions(config.RoleReviewer)); got != 0 {
		t.Errorf("reviewer sessions over three polls with an unreadable record: %d, want 0 — the head cannot be recorded either, so every poll pays for the same review", got)
	}
	// A person still gets a pass: the label short-circuits the record.
	h.gh.mu.Lock()
	h.gh.prs[42].Labels = []github.Label{{Name: "bees"}, {Name: "bees:review-requested"}}
	h.gh.mu.Unlock()
	forcePoll(h)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReviewer)); got != 1 {
		t.Errorf("reviewer sessions after the label: %d, want 1", got)
	}
}

// A pull request whose head commit the API did not report is skipped: an
// empty head says nothing about whether this change has been reviewed, and
// recording it would make every later empty head match.
func TestAnAssignedPullRequestWithNoHeadCommitIsSkipped(t *testing.T) {
	h := newHarnessAt(t, assignedReviewTOML, requestedReviewClock)
	pushBranch(t, h.clone, "fix-widget")
	pr := assignedPR("bees")
	pr.HeadSHA = ""
	h.gh.prs[42] = pr
	// A head was remembered before: without the guard the comparison with
	// the empty head asks for one more review and then records the empty
	// head over the real one.
	if err := h.store.SetReviewedSHA(42, "aaa1111"); err != nil {
		t.Fatal(err)
	}
	runPass(t, h)

	if got := sessionCount(h); got != 0 {
		t.Fatalf("sessions: %d, want 0", got)
	}
	if got := reviewedSHA(t, h, 42); got != "aaa1111" {
		t.Errorf("recorded head: %q, want aaa1111 — an empty head overwrote a real one", got)
	}
}
