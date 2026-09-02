package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/text"
)

// Outcome statuses reported by sessions with `bees done`.
const (
	OutcomePROpened         = "pr-opened"
	OutcomePRUpdated        = "pr-updated"
	OutcomeQuestion         = "question"
	OutcomeApproved         = "approved"
	OutcomeChangesRequested = "changes-requested"
	OutcomeDone             = "done"
	OutcomeIdle             = "idle"
	OutcomeFailed           = "failed"
)

// BranchFor returns the developer branch for an issue.
func (s *Scheduler) BranchFor(issue int) string {
	return fmt.Sprintf("%sissue-%d", s.cfg.Project.BranchPrefix, issue)
}

// workIssue is the developer worker: it owns one issue from ready (or a
// resumed in-progress/review state, or an approved one whose post-approval
// checks were interrupted) until the reviewer approves it, the developer asks
// a question, or the factory gives up.
func (s *Scheduler) workIssue(ctx context.Context, issue github.Issue, w *state.Worker) error {
	branch := s.BranchFor(issue.Number)
	log := s.log.With("worker", w.Name, "issue", issue.Number, "branch", branch)

	if err := s.ws.Fetch(ctx); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	ws, err := s.ws.Branch(ctx, w.Name, branch, s.cfg.Project.DefaultBranch)
	if err != nil {
		_ = s.escalate(ctx, issue.Number, "Could not create a worktree for branch `"+branch+"`: "+err.Error())
		return fmt.Errorf("workspace: %w", err)
	}
	defer func() {
		if err := s.ws.Remove(context.WithoutCancel(ctx), ws); err != nil {
			log.Warn("workspace cleanup failed", "err", err)
		}
	}()

	bookkeeping, err := s.store.Issue(issue.Number)
	if err != nil {
		return err
	}
	if bookkeeping.Round == 0 {
		bookkeeping.Round = 1
	}
	bookkeeping.Branch = branch

	// A session this issue's bookkeeping still records as running, whose
	// process is gone, was interrupted: a scheduler dying while it worked,
	// or a hard stop. The first session of that role gets told, and the
	// worker is marked resumed for `bees status` (interrupted.go).
	if in := s.takeInterrupted(log, &bookkeeping); in != nil {
		s.markResumed(w)
		s.holdInterrupted(issue.Number, in)
		defer s.forgetInterrupted(issue.Number)
	}

	// Resume: an open PR for the branch means we are in the review loop.
	pr, err := s.gh.FindPRForBranch(ctx, branch)
	if err != nil {
		return err
	}
	maxRounds := s.cfg.Scheduler.MaxReviewRounds
	policy := s.cfg.Merge()
	// stage is where the worker is; afterDevelop is where a developer session
	// leads: the first review (through the pre-review checks), or — when the
	// developer is fixing failing checks — straight back to the stage that
	// found them. prereviewDone records that the pre-review checks have been
	// read for this pull request: the read belongs to the first review, not to
	// every round, and afterDevelop cannot answer that question, because it is
	// a stage name and the changes-requested path leaves it on "review".
	//
	// All three are remembered in the issue's bookkeeping (see resumeStage),
	// so a scheduler killed mid-flight resumes where it stopped.
	stage, afterDevelop, prereviewDone := s.resumeStage(log, bookkeeping, issue, pr, policy)
	// The pre-review read, handed to the reviewer in its prompt.
	var reviewChecks []github.Check
	var reviewStatus string

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// Remember the stage before working it, so a scheduler killed here
		// resumes in it. Written once at the top of the loop rather than at
		// every `stage =`: every transition passes through here, so none of
		// them can be forgotten, and a stage nothing changed costs no write.
		if bookkeeping.WorkerStage != stage || bookkeeping.AfterDevelop != afterDevelop || bookkeeping.PreReviewDone != prereviewDone {
			bookkeeping.WorkerStage, bookkeeping.AfterDevelop, bookkeeping.PreReviewDone = stage, afterDevelop, prereviewDone
			_ = s.store.SaveIssue(bookkeeping)
		}
		// The per-issue budget is checked between stages, never while a
		// session runs: the one that took the issue over its budget has
		// finished and its work is on the branch for whoever picks it up.
		if reason, over := s.overIssueBudget(issue.Number); over {
			log.Warn("issue over its cost budget", "issue", issue.Number, "max_cost_per_issue", s.cfg.Scheduler.MaxCostPerIssue)
			return s.escalate(ctx, issue.Number, reason)
		}
		switch stage {
		case "develop":
			s.updateWorker(w, "developer", bookkeeping.Round)
			if err := s.setState(ctx, issue.Number, s.labels.InProgress); err != nil {
				return err
			}
			fresh, err := s.gh.GetIssue(ctx, issue.Number)
			if err != nil {
				return err
			}
			prNum := 0
			if pr != nil {
				prNum = pr.Number
			}
			inbox, err := s.inbox(config.RoleDeveloper, issue.Number, prNum)
			if err != nil {
				return err
			}
			parent, _ := s.gh.ParentIssue(ctx, issue.Number)
			name := fmt.Sprintf("developer-issue-%d-r%d", issue.Number, bookkeeping.Round)
			if afterDevelop == "checks" || afterDevelop == "prereview" {
				name += fmt.Sprintf("-checkfix%d", bookkeeping.CheckFixRounds)
			}
			log.Info("developer session", "round", bookkeeping.Round, "mail", len(inbox))
			started := s.now()
			res, err := s.runSessionWithRetry(ctx, sessionSpec{
				role: config.RoleDeveloper, name: name, workDir: ws.RepoDir, branch: branch, worker: w,
				data: prompts.Data{Issue: &fresh, PR: pr, Inbox: inbox, Round: bookkeeping.Round, MaxRounds: maxRounds, Parent: parent},
			})
			if err != nil {
				return err
			}
			readErr := s.mail.MarkRead(inbox...)
			s.opAs(log, slog.LevelWarn, "mail", readErr, "mark mail read", "err", readErr)
			status, note := outcomeOf(res)
			switch status {
			case OutcomePROpened, OutcomePRUpdated:
				found, err := s.locatePR(ctx, res.Outcome.PR, branch)
				if err != nil {
					return err
				}
				if found == nil {
					return s.escalate(ctx, issue.Number, fmt.Sprintf("The developer reported `%s` but no open pull request exists for branch `%s`. Note: %s", status, branch, note))
				}
				pr = found
				bookkeeping.PR = pr.Number
				_ = s.store.SaveIssue(bookkeeping)
				if err := s.ensureVisible(ctx, pr.Number, true, pr.Labels, pr.Assignees, pr.MilestoneTitle()); err != nil {
					log.Warn("the pull request may be invisible to the factory", "pr", pr.Number, "err", err)
				}
				if !s.roleEnabled(config.RoleReviewer) {
					log.Info("reviewer disabled; treating PR as approved")
					if err := s.approve(ctx, issue.Number, pr); err != nil || !policy.AutoMerge {
						return err
					}
					stage = "checks"
					continue
				}
				if afterDevelop == "checks" || afterDevelop == "prereview" {
					stage = afterDevelop
					continue
				}
				if err := s.setState(ctx, issue.Number, s.labels.Review); err != nil {
					return err
				}
				stage = "review"
				if !prereviewDone {
					stage = firstReviewStage(policy)
				}
			case OutcomeQuestion:
				if !s.sentSince(config.RoleProjectManager, issue.Number, 0, started) {
					return s.escalate(ctx, issue.Number, "The developer reported a question for the project manager but did not send one. Note: "+note)
				}
				log.Info("developer asked the project manager; issue blocked")
				return s.setState(ctx, issue.Number, s.labels.Blocked)
			default:
				return s.escalate(ctx, issue.Number, s.sessionFailure(config.RoleDeveloper, res, status, note))
			}

		case "review":
			s.updateWorker(w, "reviewer", bookkeeping.Round)
			if pr == nil {
				return s.escalate(ctx, issue.Number, "Issue is in review but no pull request exists for branch `"+branch+"`.")
			}
			freshPR, err := s.gh.GetPR(ctx, pr.Number)
			if err != nil {
				return err
			}
			freshIssue, err := s.gh.GetIssue(ctx, issue.Number)
			if err != nil {
				return err
			}
			previous, _ := s.mail.List(mail.Filter{To: config.RoleDeveloper, From: config.RoleReviewer, PR: pr.Number})
			// The checks section belongs to the review the read was made for.
			// The read happens once, before the first review, so a later round
			// must not be told "CI is green" about a head the developer has
			// since replaced: it gets no checks section at all.
			roundChecks, roundStatus := reviewChecks, reviewStatus
			reviewChecks, reviewStatus = nil, ""
			// Make sure the worktree has the developer's latest commits.
			if err := s.ws.Fetch(ctx); err == nil {
				_, _ = gitPull(ctx, ws.RepoDir)
			}
			// Read the mailbox here rather than once for the worker: this
			// stage runs again on every round, and each session must see the
			// mail that arrived since the previous one.
			inbox, err := s.inbox(config.RoleReviewer, issue.Number, pr.Number)
			if err != nil {
				return err
			}
			// product-fit is the only stage that judges the change against the
			// work item's parent feature, and it is off by default: looking the
			// parent up unconditionally would add a GraphQL query to every
			// review round of every repository for a section nobody renders.
			// A failed lookup is reported but never fatal: the stage still runs
			// against the README and the docs, which is worth more than a review
			// that dies because a GraphQL query flaked. It is reported because
			// the alternative — a silent nil — tells the reviewer the work item
			// belongs to no feature, and that lands in the verdict as a fact.
			stages := s.cfg.ReviewStages()
			var parent *github.Parent
			if slices.Contains(stages, config.StageProductFit) {
				p, err := s.gh.ParentIssue(ctx, issue.Number)
				if !s.op("work-item-parent", err, "work item parent", "issue", issue.Number, "err", err) {
					parent = p
				}
			}
			name := fmt.Sprintf("reviewer-pr-%d-r%d", pr.Number, bookkeeping.Round)
			log.Info("reviewer session", "pr", pr.Number, "round", bookkeeping.Round, "mail", len(inbox), "stages", strings.Join(stages, ","))
			started := s.now()
			res, err := s.runSessionWithRetry(ctx, sessionSpec{
				role: config.RoleReviewer, name: name, workDir: ws.RepoDir, branch: branch, worker: w,
				data: prompts.Data{Issue: &freshIssue, PR: &freshPR, Inbox: inbox, PreviousRounds: previous, Round: bookkeeping.Round, MaxRounds: maxRounds,
					Stages: stages, Parent: parent,
					Checks: roundChecks, ChecksStatus: roundStatus, ChecksTimeout: shortDuration(policy.PreReviewChecksTimeout)},
			})
			if err != nil {
				return err
			}
			readErr := s.mail.MarkRead(inbox...)
			s.opAs(log, slog.LevelWarn, "mail", readErr, "mark mail read", "err", readErr)
			status, note := outcomeOf(res)
			switch status {
			case OutcomeApproved:
				log.Info("pull request approved", "pr", pr.Number, "note", note)
				if err := s.approve(ctx, issue.Number, pr); err != nil || !policy.AutoMerge {
					return err
				}
				stage = "checks"
			case OutcomeChangesRequested:
				if !s.sentSince(config.RoleDeveloper, 0, pr.Number, started) {
					return s.escalate(ctx, issue.Number, "The reviewer requested changes but sent no feedback to the developer. Note: "+note)
				}
				if bookkeeping.Round >= maxRounds {
					return s.escalate(ctx, issue.Number, fmt.Sprintf("Pull request #%d was not approved after %s. The reviewer's last feedback is in the busybees mailbox.", pr.Number, text.Count(maxRounds, "review round")))
				}
				bookkeeping.Round++
				_ = s.store.SaveIssue(bookkeeping)
				log.Info("changes requested; back to developer", "round", bookkeeping.Round)
				stage = "develop"
			default:
				return s.escalate(ctx, issue.Number, s.sessionFailure(config.RoleReviewer, res, status, note))
			}

		case "prereview":
			s.updateWorker(w, "pre-review checks", bookkeeping.CheckFixRounds+1)
			if pr == nil {
				return s.escalate(ctx, issue.Number, "Issue is in review but no pull request exists for branch `"+branch+"`.")
			}
			log.Info("reading the checks before the review", "pr", pr.Number, "timeout", policy.PreReviewChecksTimeout)
			status, checks, _, err := s.awaitChecks(ctx, pr.Number, policy, checksWatch{timeout: policy.PreReviewChecksTimeout}, w, bookkeeping.CheckFixRounds+1)
			if err != nil {
				// The pre-review read is advisory: a broken `gh pr checks`
				// must not cost the pull request its review. The post-approval
				// stage, where the read is a merge gate, still fails hard. A
				// read that keeps failing is a degraded operation, so a
				// reviewer silently losing its checks section is visible.
				s.opAs(log, slog.LevelWarn, "pre-review-checks", err, "could not read the checks; reviewing anyway", "pr", pr.Number, "err", err)
				reviewChecks, reviewStatus = nil, ""
				prereviewDone = true
				afterDevelop, stage = "review", "review"
				continue
			}
			s.track("pre-review-checks", nil)
			if status != github.ChecksFailed {
				if status == github.ChecksPending {
					log.Info("checks still pending; reviewing anyway", "pr", pr.Number, "timeout", policy.PreReviewChecksTimeout)
				}
				// "Nothing reported" is not a failure: the reviewer is told the
				// repository has no checks, so nothing was verified for it.
				reviewChecks, reviewStatus = checks, string(status)
				if status == github.ChecksNone {
					reviewChecks, reviewStatus = nil, string(github.ChecksPassed)
				}
				prereviewDone = true
				afterDevelop, stage = "review", "review"
				continue
			}
			// A failing check goes to the reviewer in checks mode, exactly as
			// it does after approval; the review itself waits until it is green.
			next, err := s.fixFailedChecks(ctx, checksFix{
				issue: issue, pr: pr, repoDir: ws.RepoDir, branch: branch, worker: w,
				bookkeeping: &bookkeeping, checks: checks, policy: policy, stage: "prereview", log: log,
			})
			if err != nil || next == "" {
				return err
			}
			if next == "develop" {
				afterDevelop = "prereview"
			}
			stage = next

		case "checks":
			round := bookkeeping.CheckFixRounds + 1
			s.updateWorker(w, "checks", round)
			log.Info("waiting for checks", "pr", pr.Number, "wait", policy.ChecksWait)
			status, checks, gate, err := s.awaitChecks(ctx, pr.Number, policy, checksWatch{timeout: policy.ChecksTimeout, stage: "checks"}, w, round)
			if err != nil {
				return err
			}
			switch status {
			case github.ChecksNone:
				// A repository may legitimately have no CI at all. Merging is
				// still automatic, but it is logged as what it is: nothing was
				// verified.
				log.Info("no checks reported; merging without a check gate", "pr", pr.Number, "method", policy.Method)
				if err := s.gh.MergePR(ctx, pr.Number, policy.Method); err != nil {
					return s.escalate(ctx, issue.Number, fmt.Sprintf("No check was reported on #%d and merging it failed (branch protection may need a human): %v", pr.Number, err))
				}
				return nil
			case github.ChecksPassed:
				if gate == gateRequired {
					log.Info("required checks passed; merging", "pr", pr.Number, "method", policy.Method)
				} else {
					log.Info(fmt.Sprintf("no required checks; %d reported checks passed; merging", len(checks)), "pr", pr.Number, "method", policy.Method)
				}
				if err := s.gh.MergePR(ctx, pr.Number, policy.Method); err != nil {
					return s.escalate(ctx, issue.Number, fmt.Sprintf("Checks passed but merging #%d failed (branch protection may need a human): %v", pr.Number, err))
				}
				return nil
			case github.ChecksPending:
				return s.escalate(ctx, issue.Number, fmt.Sprintf("Checks on #%d were still pending after %s.", pr.Number, policy.ChecksTimeout))
			}
			// Checks failed: the reviewer diagnoses, the developer fixes.
			next, err := s.fixFailedChecks(ctx, checksFix{
				issue: issue, pr: pr, repoDir: ws.RepoDir, branch: branch, worker: w,
				bookkeeping: &bookkeeping, checks: checks, gate: gate, policy: policy, stage: "checks", log: log,
			})
			if err != nil || next == "" {
				return err
			}
			if next == "develop" {
				afterDevelop = "checks"
			}
			stage = next
		}
	}
}

// checksFix is one round of "the checks failed": the reviewer diagnoses the
// failure in checks mode and hands the developer a fix request. Both the
// pre-review and the post-approval stage go through it, and they share the
// CheckFixRounds counter.
type checksFix struct {
	issue       github.Issue
	pr          *github.PR
	repoDir     string
	branch      string
	worker      *state.Worker
	bookkeeping *state.IssueState
	checks      []github.Check
	gate        checksGate
	policy      config.MergePolicy
	// stage is the stage that found the failure ("prereview" or "checks");
	// the worker returns to it once the developer has pushed a fix.
	stage string
	log   *slog.Logger
}

// fixFailedChecks runs the reviewer in checks mode and reports the stage the
// worker continues with: "develop" when the reviewer mailed the developer a
// fix request, f.stage when the reviewer re-ran the checks itself, or "" when
// the issue was escalated and the worker is done.
func (s *Scheduler) fixFailedChecks(ctx context.Context, f checksFix) (string, error) {
	if f.bookkeeping.CheckFixRounds >= f.policy.MaxCheckFixRounds {
		return "", s.escalate(ctx, f.issue.Number, fmt.Sprintf("Checks on #%d still fail after %d fix rounds: %s", f.pr.Number, f.policy.MaxCheckFixRounds, checkNames(github.Failed(f.checks))))
	}
	f.bookkeeping.CheckFixRounds++
	_ = s.store.SaveIssue(*f.bookkeeping)
	freshPR, err := s.gh.GetPR(ctx, f.pr.Number)
	if err != nil {
		return "", err
	}
	freshIssue, err := s.gh.GetIssue(ctx, f.issue.Number)
	if err != nil {
		return "", err
	}
	if err := s.ws.Fetch(ctx); err == nil {
		_, _ = gitPull(ctx, f.repoDir)
	}
	// Read the mailbox here rather than once for the worker: this stage runs
	// again on every fix round, and each session must see the mail that
	// arrived since the previous one.
	inbox, err := s.inbox(config.RoleReviewer, f.issue.Number, f.pr.Number)
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("reviewer-pr-%d-checks%d", f.pr.Number, f.bookkeeping.CheckFixRounds)
	f.log.Info("checks failed; reviewer diagnosing", "pr", f.pr.Number, "stage", f.stage, "gate", string(f.gate), "round", f.bookkeeping.CheckFixRounds, "checks", checkNames(github.Failed(f.checks)), "mail", len(inbox))
	started := s.now()
	res, err := s.runSessionWithRetry(ctx, sessionSpec{
		role: config.RoleReviewer, name: name, task: "reviewer_checks", workDir: f.repoDir, branch: f.branch, worker: f.worker,
		data: prompts.Data{Issue: &freshIssue, PR: &freshPR, Inbox: inbox, FailedChecks: github.Failed(f.checks), Round: f.bookkeeping.CheckFixRounds, MaxRounds: f.policy.MaxCheckFixRounds},
		env:  map[string]string{"BEES_REVIEW_MODE": "checks"},
	})
	if err != nil {
		return "", err
	}
	readErr := s.mail.MarkRead(inbox...)
	s.opAs(f.log, slog.LevelWarn, "mail", readErr, "mark mail read", "err", readErr)
	outcome, note := outcomeOf(res)
	switch outcome {
	case OutcomeChangesRequested:
		if !s.sentSince(config.RoleDeveloper, 0, f.pr.Number, started) {
			return "", s.escalate(ctx, f.issue.Number, "The reviewer diagnosed failing checks but sent nothing to the developer. Note: "+note)
		}
		return "develop", nil
	case OutcomeApproved:
		f.log.Info("reviewer re-ran the checks; waiting again", "pr", f.pr.Number)
		return f.stage, nil
	}
	return "", s.escalate(ctx, f.issue.Number, fmt.Sprintf("The failing checks on #%d were not diagnosed. %s", f.pr.Number, s.sessionFailure(config.RoleReviewer, res, outcome, note)))
}

// firstReviewStage is where a worker goes when a pull request is ready for its
// first review: through the pre-review checks unless they are turned off.
func firstReviewStage(policy config.MergePolicy) string {
	if policy.PreReviewChecks {
		return "prereview"
	}
	return "review"
}

// workerStages are the stages workIssue runs, and the values IssueState
// remembers. They are the developer worker's own state machine, not
// roles.reviewer.stages, which are sections of one reviewer session's prompt.
var workerStages = []string{"develop", "prereview", "review", "checks"}

// postApprovalFixRound reports whether a record is the develop round of a
// post-approval check fix: the checks stage found a failing check, the
// reviewer diagnosed it in checks mode and mailed the developer, and the
// worker went back to develop with the checks as the gate it returns to.
//
// That record is written at the top of the loop, before the develop stage
// relabels the issue bees:in-progress, so a worker killed — or a single
// failing `gh issue edit` — in between leaves it behind a bees:approved
// label: work in flight exactly as a record of the checks stage itself is,
// one stage on. Only the record is tested here; each caller adds its own
// label and pull request tests, because they have different things in hand
// (a snapshot in resumableChecks, the pull request in resumeStage).
func postApprovalFixRound(bk state.IssueState) bool {
	return bk.WorkerStage == "develop" && bk.AfterDevelop == "checks"
}

// resumeStage decides where a developer worker starts, and with what loop
// state. A worker that has run before left its stage in the issue's
// bookkeeping, and resuming from it is the whole point: the workflow label
// says an issue is in review, never whether its review has already happened,
// so a scheduler killed in the checks stage or halfway through a check-fix
// round would otherwise restart as a full first review and pay for a reviewer
// session that has already run.
//
// The label stays the human-facing truth. A remembered stage it contradicts —
// a review-loop stage on an issue with no open pull request, or one a person
// has put back to bees:ready or bees:triage — is dropped, with a log line, and
// the worker starts where the label says. develop contradicts no label, so the
// loop state remembered with it is dropped on the same test instead: an issue
// whose labels have left the review loop starts a fresh round. The one
// exception is the develop round the post-approval checks send back
// (postApprovalFixRound): it is recorded before the develop stage can relabel
// the issue, so bees:approved is where it legitimately sits, and the gate it
// returns to is the whole reason to resume it. An issue with nothing
// remembered (the first run, and every issue that existed before the stage was
// recorded) starts exactly where it always did.
//
// The whole record belongs to the pull request it was written for, and is
// dropped for any other one: both tests below compare the recorded number with
// the open pull request the branch has now. A person can close a pull request
// and open another on the same branch while no worker is running, and a record
// left for the first says nothing about the second — its review has not
// happened and its checks have not been read. A record with no number at all,
// written before one was known, is dropped the same way: starting a stage
// later than the truth is the expensive mistake, starting one earlier only
// costs a session.
func (s *Scheduler) resumeStage(log *slog.Logger, bk state.IssueState, issue github.Issue, pr *github.PR, policy config.MergePolicy) (stage, afterDevelop string, prereviewDone bool) {
	fromLabel := "develop"
	if pr != nil && s.stateOf(issue.Labels) == "review" {
		// A worker resuming into a review it has no record of reads the
		// checks first, so the reviewer it runs gets the same status a fresh
		// one would.
		fromLabel = "review"
		if s.roleEnabled(config.RoleReviewer) {
			fromLabel = firstReviewStage(policy)
		}
	}
	if bk.WorkerStage == "" {
		return fromLabel, "review", false
	}
	if !s.stageMatchesLabels(bk.WorkerStage, issue, pr) {
		log.Info("the remembered stage contradicts the issue's labels; resuming from the label",
			"remembered", bk.WorkerStage, "state", s.stateOf(issue.Labels), "stage", fromLabel)
		return fromLabel, "review", false
	}
	// Both pr.Number reads are safe by short-circuit: this one is only reached
	// when stageMatchesLabels was true for a stage other than develop, which
	// needs an open pull request either way it can be true, and the one below
	// only when inReviewLoop was true, which needs one too.
	if bk.WorkerStage != "develop" && bk.PR != pr.Number {
		log.Info("the remembered stage belongs to another pull request; resuming from the label",
			"remembered", bk.WorkerStage, "remembered_pr", bk.PR, "pr", pr.Number, "stage", fromLabel)
		return fromLabel, "review", false
	}
	afterDevelop, prereviewDone = bk.AfterDevelop, bk.PreReviewDone
	// The post-approval fix round is the one develop record whose labels are
	// legitimately outside the review loop: bees:approved, because the record
	// was written before the develop stage could relabel the issue. Its gate
	// is the whole reason to resume it, so it survives the drop below — and
	// this is the third way a pr.Number read here is safe by short-circuit.
	approvedFixRound := pr != nil && s.stateOf(issue.Labels) == "approved" && postApprovalFixRound(bk)
	if (!s.inReviewLoop(issue, pr) && !approvedFixRound) || bk.PR != pr.Number {
		// The sub-state belongs to a pull request under review: which gate a
		// developer round goes back to, and whether that pull request's checks
		// have been read. develop matches any label, so without this an issue
		// whose labels have left the review loop — bees:ready after
		// reopenApproved sent an approved pull request back for more work —
		// would carry a remembered after_develop of "checks" into a fresh
		// round and merge the pull request without reviewing it. The number
		// test is the same rule for the other way the sub-state can be stale:
		// the pull request it was written for is not the one open now.
		afterDevelop, prereviewDone = "review", false
	} else if approvedFixRound {
		// pre_review_done says the pre-review checks were read for this pull
		// request, which is a question about the first review; the round
		// resuming here goes straight back to the checks gate without asking
		// it, so keeping it would be dead state under a label the review loop
		// has left.
		prereviewDone = false
	}
	if afterDevelop != "prereview" && afterDevelop != "checks" {
		afterDevelop = "review"
	}
	return bk.WorkerStage, afterDevelop, prereviewDone
}

// stageMatchesLabels reports whether a remembered stage still agrees with what
// the issue's labels say. Only develop needs nothing: the three stages of the
// review loop are meaningless without an open pull request, and an issue back
// in a state that has not reached one — bees:ready after a conflict reopened
// it, or anything a person set by hand — is one whose review is over whatever
// the last worker remembered. An unknown stage (a state file written by
// another version) matches nothing and is dropped the same way. The loop state
// remembered alongside develop is tested separately, in resumeStage.
//
// bees:approved admits checks, and only checks: approve() sets that label
// before the worker enters the post-approval wait, so the label an interrupted
// checks stage comes back to is legitimately approved rather than in review.
// Without it the worker would restart in develop and pay for a developer
// session — and a second review — on a pull request that has already been
// approved. The same reason as in-progress, one stage later.
func (s *Scheduler) stageMatchesLabels(stage string, issue github.Issue, pr *github.PR) bool {
	if stage == "develop" {
		return true
	}
	if !slices.Contains(workerStages, stage) {
		return false
	}
	if stage == "checks" && pr != nil && s.stateOf(issue.Labels) == "approved" {
		return true
	}
	return s.inReviewLoop(issue, pr)
}

// inReviewLoop reports whether the issue's labels still say a pull request of
// this issue is being worked on. in-progress as well as review: the developer
// session of a check-fix round sets bees:in-progress and returns to the stage
// that found the failure, so a worker legitimately sits in checks under either
// label.
func (s *Scheduler) inReviewLoop(issue github.Issue, pr *github.PR) bool {
	st := s.stateOf(issue.Labels)
	return pr != nil && (st == "review" || st == "in-progress")
}

// checksGate is the set of checks a wait is gated on. It is chosen on the
// first observation that reports anything and never changes afterwards.
type checksGate string

const (
	// gateUnknown is the state before the first observation.
	gateUnknown checksGate = ""
	// gateRequired: the branch protection rules require checks, and those are
	// the gate. This is the only gate that existed before #117.
	gateRequired checksGate = "required"
	// gateReported: nothing is required, so every check the pull request
	// reports is the gate. Gating on the checks that exist beats gating on
	// nothing; the escape hatch is GitHub's, not a bees.toml key: mark the
	// checks that must block a merge as required.
	gateReported checksGate = "reported"
	// gateNone: no check was reported at all, twice in a row.
	gateNone checksGate = "none"
)

// checksWatch is what one call to awaitChecks does differently from another:
// the post-approval stage waits ChecksTimeout and names its gate in the worker
// stage, the pre-review read waits PreReviewChecksTimeout and keeps the stage
// name the worker already set.
type checksWatch struct {
	// timeout bounds the wait; ChecksTimeout or PreReviewChecksTimeout.
	timeout time.Duration
	// stage prefixes the gate in the worker stage ("checks (required)"). An
	// empty stage leaves the worker stage alone.
	stage string
}

// awaitChecks waits policy.ChecksWait, then polls the PR's checks every
// ChecksPollInterval until they pass, fail, or watch.timeout elapses, and
// reports which gate it settled on.
//
// The required checks are asked for first and win outright: when the branch
// requires anything, the second `gh pr checks` call is never made and the
// behaviour is exactly what it was before #117. Only when nothing is required
// does the wait fall back to every reported check — a repository with no
// branch protection would otherwise merge with nothing green at all. Two
// consecutive empty observations are needed before concluding there is no CI,
// because a workflow can take longer than checks_wait to register.
func (s *Scheduler) awaitChecks(ctx context.Context, pr int, policy config.MergePolicy, watch checksWatch, w *state.Worker, round int) (github.ChecksStatus, []github.Check, checksGate, error) {
	if err := sleepCtx(ctx, policy.ChecksWait); err != nil {
		return "", nil, gateUnknown, err
	}
	deadline := s.now().Add(watch.timeout)
	gate := gateUnknown
	empty := 0
	for {
		checks, err := s.gh.RequiredChecks(ctx, pr)
		if err != nil {
			return "", nil, gate, err
		}
		if len(checks) > 0 {
			gate = gateRequired
		}
		if gate != gateRequired {
			// No required check: fall back to everything the PR reports.
			checks, err = s.gh.Checks(ctx, pr)
			if err != nil {
				return "", nil, gate, err
			}
			if len(checks) > 0 {
				gate = gateReported
			}
		}
		status := github.Summarize(checks)
		if status == github.ChecksNone && gate == gateUnknown {
			empty++
			if empty >= 2 {
				s.setChecksGate(w, watch.stage, gateNone, round)
				return github.ChecksNone, nil, gateNone, nil
			}
			// One empty observation proves nothing: poll once more before
			// concluding the repository has no CI.
			if err := sleepCtx(ctx, policy.ChecksPollInterval); err != nil {
				return "", nil, gate, err
			}
			continue
		}
		if status == github.ChecksNone {
			// The gate was chosen and the checks then vanished from the
			// report. Keep polling the tier we settled on rather than
			// merging: the gate never switches back.
			status = github.ChecksPending
		}
		s.setChecksGate(w, watch.stage, gate, round)
		if status != github.ChecksPending || !s.now().Before(deadline) {
			return status, checks, gate, nil
		}
		if err := sleepCtx(ctx, policy.ChecksPollInterval); err != nil {
			return "", nil, gate, err
		}
	}
}

// setChecksGate names the gate in the worker stage, so a worker sitting in a
// 30-minute wait shows in `bees status` what it is waiting for. A caller that
// passes no stage name keeps its own.
func (s *Scheduler) setChecksGate(w *state.Worker, prefix string, gate checksGate, round int) {
	if w == nil || prefix == "" || gate == gateUnknown {
		return
	}
	stage := prefix + " (" + string(gate) + ")"
	s.mu.Lock()
	same := w.Stage == stage
	s.mu.Unlock()
	if !same {
		s.updateWorker(w, stage, round)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

func checkNames(checks []github.Check) string {
	names := make([]string, 0, len(checks))
	for _, c := range checks {
		names = append(names, c.Name)
	}
	return strings.Join(names, ", ")
}

// approve labels an approved PR and its issue. Merging, when enabled,
// happens in the worker's checks stage.
func (s *Scheduler) approve(ctx context.Context, issue int, pr *github.PR) error {
	if err := s.gh.EditLabels(ctx, pr.Number, []string{s.labels.Approved}, nil); err != nil {
		return err
	}
	// An approved pull request waits for a person to merge it. Requesting a
	// review is best effort: GitHub refuses one from the PR's own author, and
	// with a shared account the configured login often is the author.
	if notify := s.cfg.Scheduler.Notify; len(notify) > 0 {
		if err := s.gh.RequestReview(ctx, pr.Number, notify...); err != nil {
			s.log.Warn("could not request a review on the approved pull request", "pr", pr.Number, "err", err)
		}
	}
	return s.setState(ctx, issue, s.labels.Approved)
}

// locatePR finds the PR a developer session produced.
func (s *Scheduler) locatePR(ctx context.Context, number int, branch string) (*github.PR, error) {
	if number > 0 {
		pr, err := s.gh.GetPR(ctx, number)
		if err == nil && pr.State == "OPEN" && pr.HeadRefName == branch {
			return &pr, nil
		}
	}
	return s.gh.FindPRForBranch(ctx, branch)
}

// ensureVisible makes sure an item the factory created matches the filter, so
// the factory keeps seeing it: a PR that misses the base label, the assignee
// or the milestone the filter asks for is never polled again, the reviewer is
// never dispatched, and the PR strands its branch and its issue.
//
// Everything already in place is left alone, so a PR the session labelled and
// assigned itself costs no gh calls. Errors are collected rather than
// returned at the first failure: the three fixes are independent.
//
// The milestone is set on pull requests only. A milestone on an issue is a
// person's decision, and an issue the factory creates inherits one through
// `bees issue create`; a bee must not put an issue into a milestone a person
// left it out of. A PR has no such inheritance path and its milestone is pure
// filter bookkeeping.
func (s *Scheduler) ensureVisible(ctx context.Context, number int, isPR bool, labels []github.Label, assignees []github.Author, milestone string) error {
	// Each mutation records its own failure streak (see degraded.go) but logs
	// nothing: the caller reports the joined error as one line naming the item.
	var errs []error
	if !github.HasLabel(labels, s.labels.Base) {
		err := s.gh.EditLabels(ctx, number, []string{s.labels.Base}, nil)
		if s.track("label", err) {
			errs = append(errs, fmt.Errorf("add the %s label: %w", s.labels.Base, err))
		}
	}
	if a := s.cfg.Filter.Assignee; a != "" && !github.HasAssignee(assignees, a) {
		err := s.gh.Assign(ctx, number, a)
		if s.track("assign", err) {
			errs = append(errs, fmt.Errorf("assign it to %s: %w", a, err))
		}
	}
	if m := s.cfg.Filter.Milestone; isPR && m != "" && milestone != m {
		err := s.gh.SetMilestone(ctx, number, m)
		if s.track("milestone", err) {
			errs = append(errs, fmt.Errorf("put it in milestone %s: %w", m, err))
		}
	}
	return errors.Join(errs...)
}

// shortDuration renders a duration the way it is written in bees.toml: "10m"
// rather than time.Duration's "10m0s".
func shortDuration(d time.Duration) string {
	s := d.String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

func gitPull(ctx context.Context, dir string) (string, error) {
	out, err := gitRun(ctx, dir, "pull", "--ff-only", "--quiet")
	if err != nil && strings.Contains(err.Error(), "no tracking information") {
		return out, nil
	}
	return out, err
}
