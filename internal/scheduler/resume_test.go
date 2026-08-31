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
			stage, after, done := h.sched.resumeStage(tc.bk, labelled(tc.state), tc.pr, h.cfg.Merge())
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
