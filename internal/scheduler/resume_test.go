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
	"github.com/kpenfound/busybees/internal/state"
)

// TestAWorkerKilledInTheChecksStageResumesInIt is the point of remembering the
// stage: the workflow label says an issue is being worked on, never how far
// its worker had got. The first run dies on a checks read in the middle of a
// check-fix round; the second must go straight back to the checks it was
// waiting for, without re-running a review that has already happened.
func TestAWorkerKilledInTheChecksStageResumesInIt(t *testing.T) {
	h := newHarnessAt(t, checksTOML, time.Now())
	seedChecksIssue(t, h)
	h.gh.issues[1].CreatedAt = time.Now().Add(-time.Hour)
	h.gh.checks = []checksResponse{
		{failingJSON, fmt.Errorf("exit status 1")},     // the post-approval gate: a check failed
		{"", fmt.Errorf("gh: could not reach github")}, // the read the scheduler dies on
		{passingJSON, nil},                             // green again by the time it comes back
	}
	runPass(t, h)

	// The failing check cost a fix round, and the worker died in the checks
	// stage it returned to with the developer's fix pushed.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1", "reviewer-pr-101-checks1", "developer-issue-1-r1-checkfix1")
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.WorkerStage != "checks" || bk.AfterDevelop != "checks" {
		t.Fatalf("bookkeeping after the crash: %+v", bk)
	}
	if len(h.gh.merged) != 0 {
		t.Fatalf("nothing was merged yet, got %v", h.gh.merged)
	}
	// The label a restart would otherwise infer the stage from says only that
	// a developer is on it, which is where the old inference would restart.
	if got := h.stateOfIssue(1); got != "in-progress" {
		t.Fatalf("issue state label after the crash is %q", got)
	}

	// Restart. The failed worker set a backoff on the issue; a real restart
	// is a new process, so step over it.
	h.clock.advance(6 * h.cfg.Scheduler.PollInterval.Duration)
	forcePoll(h)
	runPass(t, h)

	if len(h.gh.merged) != 1 || h.gh.merged[0] != fakePR {
		t.Fatalf("the resumed checks stage did not merge: %v", h.gh.merged)
	}
	// The whole saving: not one session. Re-deriving the stage from the label
	// would have started a developer, and then a second review.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1", "reviewer-pr-101-checks1", "developer-issue-1-r1-checkfix1")
}

// TestAResumedWorkerDoesNotReadTheChecksAgain: the pre-review read belongs to
// the first review and happens once per pull request, and a restart is not a
// second pull request. The consequence is deliberate: the review that resumes
// gets no checks section, exactly like the second round of a review loop that
// was never interrupted, because the read it would quote was made against a
// head the developer may since have replaced.
func TestAResumedWorkerDoesNotReadTheChecksAgain(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Restarted between rounds")
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:review"}, {Name: "bees:size/s"}}
	// The pull request exists from the start: this worker is a resumption.
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// What the killed worker left behind: it was in its second review round,
	// and the pre-review read had already happened.
	if err := h.store.SaveIssue(state.IssueState{Number: 1, Round: 2, PR: fakePR, Branch: "bees/issue-1",
		WorkerStage: "review", AfterDevelop: "review", PreReviewDone: true}); err != nil {
		t.Fatal(err)
	}
	seedCounter(t, h, "review", 1) // approve on the first round
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	runPreReviewLoop(t, h)

	h.wantOrder("reviewer-pr-101-r2")
	if n := h.gh.callCount("pr checks"); n != 0 {
		t.Fatalf("a resumed worker read the checks %d times, want none: the read was already made", n)
	}
	if review := promptOf(t, h, 0); strings.Contains(review, "## Required checks") {
		t.Fatalf("the resumed review quotes a read this worker never made:\n%s", review)
	}
}

// TestARememberedStageThatContradictsTheLabelLosesToIt: labels are what a
// person edits, so they outrank the scheduler's own memory. An approved issue
// whose pull request fell behind goes back to bees:ready — a developer's job —
// and the review stage the last worker remembered must not survive it.
func TestARememberedStageThatContradictsTheLabelLosesToIt(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Sent back to a developer")
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := h.store.SaveIssue(state.IssueState{Number: 1, Round: 1, PR: fakePR, Branch: "bees/issue-1",
		WorkerStage: "review", AfterDevelop: "review", PreReviewDone: true}); err != nil {
		t.Fatal(err)
	}
	seedCounter(t, h, "review", 1) // approve the round that does happen
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	runPreReviewLoop(t, h)

	// A developer runs first, and the pre-review read the remembered state
	// claimed was done happens after it, because this is a first review.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
	if !strings.Contains(h.logs.String(), "the remembered stage contradicts the issue's labels") {
		t.Fatalf("the contradiction was not logged:\n%s", h.logs.String())
	}
	// The sub-state goes with the stage: the remembered "the checks have been
	// read" is dropped too, so this pull request gets its pre-review read.
	if n := h.gh.callCount("pr checks"); n != 1 {
		t.Fatalf("the checks were read %d times, want 1: the dropped stage takes its sub-state with it", n)
	}
}

// TestAResumedDeveloperReturnsToTheStageThatSentIt: the stage a worker is in
// is not the whole of its state machine. A developer session run to fix a
// failing check goes back to the checks, not into a review round nobody asked
// for, and a restart in the middle of that session must not lose which of the
// two it was.
func TestAResumedDeveloperReturnsToTheStageThatSentIt(t *testing.T) {
	h := newHarness(t, checksTOML)
	seedChecksIssue(t, h)
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:in-progress"}, {Name: "bees:size/s"}}
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Killed during the developer session of the first check-fix round.
	if err := h.store.SaveIssue(state.IssueState{Number: 1, Round: 1, PR: fakePR, Branch: "bees/issue-1",
		CheckFixRounds: 1, WorkerStage: "develop", AfterDevelop: "checks"}); err != nil {
		t.Fatal(err)
	}
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	runChecksLoop(t, h)

	// The developer session is named as the check-fix round it is, and the
	// pull request goes straight back to the gate that sent it: no reviewer.
	h.wantOrder("developer-issue-1-r1-checkfix1")
	if len(h.gh.merged) != 1 || h.gh.merged[0] != fakePR {
		t.Fatalf("the fixed pull request was not merged: %v", h.gh.merged)
	}
}

// TestAResumedDeveloperRoundDoesNotPayForTheReadAgain: the read is remembered
// as well as the stage. A worker killed after the reviewer asked for changes
// comes back into its developer round, and the pull request the developer
// pushes goes straight to the review — a resumed changes-requested round pays
// neither the extra `gh pr checks` nor the wait, exactly like one that was
// never interrupted.
func TestAResumedDeveloperRoundDoesNotPayForTheReadAgain(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Killed between the rounds")
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:review"}, {Name: "bees:size/s"}}
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The reviewer requested changes and the worker died before the developer
	// finished; the pre-review read belongs to the pull request and is done.
	if err := h.store.SaveIssue(state.IssueState{Number: 1, Round: 2, PR: fakePR, Branch: "bees/issue-1",
		WorkerStage: "develop", AfterDevelop: "review", PreReviewDone: true}); err != nil {
		t.Fatal(err)
	}
	seedCounter(t, h, "review", 1) // approve the round that follows
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	runPreReviewLoop(t, h)

	h.wantOrder("developer-issue-1-r2", "reviewer-pr-101-r2")
	if n := h.gh.callCount("pr checks"); n != 0 {
		t.Fatalf("the resumed round read the checks %d times, want none", n)
	}
}

// TestResumeStage pins the whole decision a worker makes before it does
// anything: what it remembered, filtered through what the labels say. The two
// cases the loop tests cannot reach are here — a remembered review-loop stage
// whose pull request has gone, and a state file another version wrote — along
// with the one that matters most, an issue with nothing remembered at all,
// which must start exactly where it always did.
func TestResumeStage(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	pr := &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1"}
	labelled := func(state string) github.Issue {
		return github.Issue{Number: 1, Labels: []github.Label{{Name: "bees"}, {Name: "bees:" + state}}}
	}
	for _, tc := range []struct {
		name          string
		bk            state.IssueState
		state         string
		pr            *github.PR
		stage, after  string
		prereviewDone bool
	}{
		{name: "nothing remembered, no pull request", state: "ready",
			stage: "develop", after: "review"},
		{name: "nothing remembered, a pull request in review", state: "review", pr: pr,
			stage: "prereview", after: "review"},
		{name: "remembered mid-checks", bk: state.IssueState{WorkerStage: "checks", AfterDevelop: "checks", PreReviewDone: true},
			state: "in-progress", pr: pr, stage: "checks", after: "checks", prereviewDone: true},
		{name: "remembered mid-round, the read already made", bk: state.IssueState{WorkerStage: "develop", AfterDevelop: "review", PreReviewDone: true},
			state: "review", pr: pr, stage: "develop", after: "review", prereviewDone: true},
		// develop fits any label, so its sub-state is dropped on the same
		// test the stages are: this issue is starting a fresh round.
		{name: "a develop stage on an issue sent back to ready", bk: state.IssueState{WorkerStage: "develop", AfterDevelop: "checks", PreReviewDone: true},
			state: "ready", pr: pr, stage: "develop", after: "review"},
		// The label wins, and takes the sub-state with it.
		{name: "a review stage on an issue sent back to ready", bk: state.IssueState{WorkerStage: "review", AfterDevelop: "review", PreReviewDone: true},
			state: "ready", pr: pr, stage: "develop", after: "review"},
		{name: "a review stage whose pull request has gone", bk: state.IssueState{WorkerStage: "prereview", PreReviewDone: true},
			state: "review", stage: "develop", after: "review"},
		{name: "a stage no version of this worker runs", bk: state.IssueState{WorkerStage: "reviewing", PreReviewDone: true},
			state: "review", pr: pr, stage: "prereview", after: "review"},
		// Nothing in the file can send the worker to a stage that does not
		// exist: both consumers of afterDevelop read anything else as review.
		{name: "an after_develop no version of this worker runs", bk: state.IssueState{WorkerStage: "develop", AfterDevelop: "reviewing"},
			state: "review", pr: pr, stage: "develop", after: "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stage, after, done := h.sched.resumeStage(h.sched.log, tc.bk, labelled(tc.state), tc.pr, h.cfg.Merge())
			if stage != tc.stage || after != tc.after || done != tc.prereviewDone {
				t.Errorf("resumeStage = (%q, %q, %v), want (%q, %q, %v)", stage, after, done, tc.stage, tc.after, tc.prereviewDone)
			}
		})
	}
}

// TestExecReviewerForgetsARecordedStage: `bees exec reviewer` is a person
// saying "review this now", not a resumption. It rewrites the workflow label
// to force the review stage, and the stage the last worker recorded has to go
// with it — a recorded develop stage would otherwise outlive the instruction
// and run a developer session instead.
func TestExecReviewerForgetsARecordedStage(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Review this now")
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:review"}, {Name: "bees:size/s"}}
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// The last worker stopped in a developer round of this pull request.
	if err := h.store.SaveIssue(state.IssueState{Number: 1, Round: 1, PR: fakePR, Branch: "bees/issue-1",
		WorkerStage: "develop", AfterDevelop: "review", PreReviewDone: true}); err != nil {
		t.Fatal(err)
	}
	seedCounter(t, h, "review", 1) // approve straight away
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.RunRole(ctx, config.RoleReviewer, 1, 0); err != nil {
		t.Fatal(err)
	}

	h.wantOrder("reviewer-pr-101-r1")
	// The forced review is a first review, so it pays for the read the
	// forgotten state claimed had already happened.
	if n := h.gh.callCount("pr checks"); n != 1 {
		t.Fatalf("the checks were read %d times, want 1", n)
	}
}

// TestASubStateDoesNotSurviveTheIssueGoingBackToReady: develop matches any
// label, so its sub-state has to be checked separately. A worker in a
// post-approval check-fix round records develop with after_develop "checks";
// when its pull request then gets human feedback, reopenApproved sends the
// issue back to bees:ready — a fresh developer round, which must end in a
// review, not in the merge gate the remembered after_develop points at.
func TestASubStateDoesNotSurviveTheIssueGoingBackToReady(t *testing.T) {
	h := newHarness(t, checksTOML)
	seedChecksIssue(t, h)
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// What the killed worker left: it had approved the pull request, a check
	// failed, and the reviewer sent the developer a fix request.
	if err := h.store.SaveIssue(state.IssueState{Number: 1, Round: 1, PR: fakePR, Branch: "bees/issue-1",
		WorkerStage: "develop", AfterDevelop: "checks", PreReviewDone: true}); err != nil {
		t.Fatal(err)
	}
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	runChecksLoop(t, h)

	// The label says ready, so this is a first round: the developer's push is
	// reviewed before the checks gate merges it.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
	if len(h.gh.merged) != 1 || h.gh.merged[0] != fakePR {
		t.Fatalf("merged %v", h.gh.merged)
	}
}

// TestExecReviewerReviewsAnIssueThatIsNotYetInReview: `bees exec reviewer`
// forces the review stage by rewriting the issue's state label, and the local
// copy the worker reads has to be rewritten too. relabel matches a full label
// name while stateOf returns the short state, so spelling the state back out
// is what actually removes the old label: given "in-progress" nothing was
// removed, the copy carried bees:in-progress and bees:review at once, and
// stateOf — first hit in StateLabels() order — read in-progress back out and
// started a developer session instead of a review.
func TestExecReviewerReviewsAnIssueThatIsNotYetInReview(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Review what is already pushed")
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:in-progress"}, {Name: "bees:size/s"}}
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	seedCounter(t, h, "review", 1) // approve straight away
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.RunRole(ctx, config.RoleReviewer, 1, 0); err != nil {
		t.Fatal(err)
	}

	h.wantOrder("reviewer-pr-101-r1")
	if n := len(h.sessions(config.RoleDeveloper)); n != 0 {
		t.Fatalf("%d developer sessions ran, want none: exec reviewer asked for a review", n)
	}
}

// TestAWorkerKilledInThePostApprovalChecksIsResumed: approve() labels the
// issue bees:approved before the worker enters the checks stage, so a
// scheduler killed while waiting out checks_timeout leaves work in flight
// behind a label no dispatch pass used to look at — with auto_merge on,
// nothing merged the pull request and nothing escalated it. The issue is a
// resumption like any other: the second run goes straight back to the checks
// it was waiting for and merges, without a developer or a reviewer session.
func TestAWorkerKilledInThePostApprovalChecksIsResumed(t *testing.T) {
	h := newHarnessAt(t, checksTOML, time.Now())
	seedChecksIssue(t, h)
	h.gh.issues[1].CreatedAt = time.Now().Add(-time.Hour)
	h.gh.checks = []checksResponse{
		{"", fmt.Errorf("gh: could not reach github")}, // the post-approval read the scheduler dies on
		{passingJSON, nil}, // green by the time it comes back
	}
	runPass(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
	if got := h.stateOfIssue(1); got != "approved" {
		t.Fatalf("issue state label after the crash is %q, want approved", got)
	}
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.WorkerStage != "checks" {
		t.Fatalf("bookkeeping after the crash: %+v", bk)
	}
	if len(h.gh.merged) != 0 {
		t.Fatalf("nothing was merged yet, got %v", h.gh.merged)
	}

	// Restart. The failed worker set a backoff on the issue; a real restart is
	// a new process, so step over it.
	h.clock.advance(6 * h.cfg.Scheduler.PollInterval.Duration)
	forcePoll(h)
	runPass(t, h)

	if len(h.gh.merged) != 1 || h.gh.merged[0] != fakePR {
		t.Fatalf("the resumed checks stage did not merge: %v", h.gh.merged)
	}
	// Not one extra session: the review has already happened and is paid for.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
}

// TestAnApprovedIssueThatIsNotAResumptionIsNotDispatched: bees:approved still
// means "waiting for a person to merge" for every issue a person would
// recognise as approved. Only the stage a worker recorded says otherwise, so
// an approved issue with nothing remembered — or with a stage that is not the
// post-approval checks — is left exactly where it is.
func TestAnApprovedIssueThatIsNotAResumptionIsNotDispatched(t *testing.T) {
	checks := &state.IssueState{Number: 1, Round: 1, PR: fakePR, Branch: "bees/issue-1",
		WorkerStage: "checks", AfterDevelop: "checks"}
	for _, tc := range []struct {
		name string
		bk   *state.IssueState
		// openPR is whether a pull request for the branch is open. The
		// remembered checks stage belongs to one, and without it the worker
		// would fall back to a developer round on a pull request that has
		// already been merged or closed.
		openPR bool
	}{
		{"nothing remembered", nil, true},
		{"a stage other than checks", &state.IssueState{Number: 1, Round: 1, PR: fakePR, Branch: "bees/issue-1",
			WorkerStage: "review", AfterDevelop: "review"}, true},
		{"a checks stage whose pull request has gone", checks, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, checksTOML)
			seedChecksIssue(t, h)
			h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:approved"}, {Name: "bees:size/s"}}
			if tc.openPR {
				if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			if tc.bk != nil {
				if err := h.store.SaveIssue(*tc.bk); err != nil {
					t.Fatal(err)
				}
			}
			h.gh.checks = []checksResponse{{passingJSON, nil}}
			runPass(t, h)

			if n := sessionCount(h); n != 0 {
				t.Fatalf("%d sessions ran on an approved issue waiting for a person: %v", n, h.sessionNames())
			}
			if got := h.stateOfIssue(1); got != "approved" {
				t.Fatalf("the issue was relabelled %q", got)
			}
			if len(h.gh.merged) != 0 {
				t.Fatalf("an approved pull request was merged behind a person's back: %v", h.gh.merged)
			}
			if n := h.gh.callCount("pr checks"); n != 0 {
				t.Fatalf("the checks were read %d times, want none: no worker was resumed", n)
			}
		})
	}
}

// TestWithoutAutoMergeAnApprovedIssueStaysWithThePerson pins what makes
// gating the resumption on roles.reviewer.auto_merge redundant: with it off,
// approve() returns before the checks stage, so worker_stage never becomes
// "checks" and the approved issue is never a candidate — the merge is the
// person's, exactly as it was.
func TestWithoutAutoMergeAnApprovedIssueStaysWithThePerson(t *testing.T) {
	h := newHarnessAt(t, strings.Replace(checksTOML, "auto_merge = true", "auto_merge = false", 1), time.Now())
	seedChecksIssue(t, h)
	h.gh.issues[1].CreatedAt = time.Now().Add(-time.Hour)
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	runPass(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.WorkerStage != "review" {
		t.Fatalf("the worker stopped at %+v, want the review stage: without auto_merge it never enters checks", bk)
	}

	h.clock.advance(6 * h.cfg.Scheduler.PollInterval.Duration)
	forcePoll(h)
	runPass(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
	if got := h.stateOfIssue(1); got != "approved" {
		t.Fatalf("the issue was relabelled %q", got)
	}
	if len(h.gh.merged) != 0 {
		t.Fatalf("a pull request was merged with auto_merge off: %v", h.gh.merged)
	}
	if n := h.gh.callCount("pr checks"); n != 0 {
		t.Fatalf("the checks were read %d times with auto_merge off, want none", n)
	}
}

// TestALocalPassResumesThePostApprovalChecks: a local pass dispatches from the
// snapshot of the last poll and confirms each candidate with one `gh issue
// view` before spending a session on it. That confirmation has to admit
// bees:approved under the same gate the candidate list uses, or the resumption
// is fetched and dropped on every local pass and only ever starts at a full
// poll.
func TestALocalPassResumesThePostApprovalChecks(t *testing.T) {
	h := newHarnessAt(t, checksTOML, time.Now())
	seedChecksIssue(t, h)
	h.gh.issues[1].CreatedAt = time.Now().Add(-time.Hour)
	h.gh.checks = []checksResponse{
		{"", fmt.Errorf("gh: could not reach github")},
		{passingJSON, nil},
	}
	runPass(t, h)
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")

	// A full poll that sees the crashed state: the issue is approved and its
	// pull request is open. Nothing is dispatched from it, because the failed
	// worker's backoff is still running.
	forcePoll(h)
	runPass(t, h)
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
	if len(h.gh.merged) != 0 {
		t.Fatalf("dispatched while backed off: %v", h.gh.merged)
	}

	// The backoff expires. The next pass is a local one — nextPoll is set —
	// so it works from that snapshot and confirms the issue live.
	h.sched.setBackoff("issue-1", -time.Hour)
	runPass(t, h)

	if n := polls(h); n != 2 {
		t.Fatalf("%d polls, want 2: the resumption must come from a local pass", n)
	}
	if len(h.gh.merged) != 1 || h.gh.merged[0] != fakePR {
		t.Fatalf("the local pass did not resume the checks stage: %v", h.gh.merged)
	}
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-101-r1")
}
