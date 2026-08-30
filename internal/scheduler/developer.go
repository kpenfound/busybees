package scheduler

import (
	"context"
	"errors"
	"fmt"
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
// resumed in-progress/review state) until the reviewer approves it, the
// developer asks a question, or the factory gives up.
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

	// Resume: an open PR for the branch means we are in the review loop.
	pr, err := s.gh.FindPRForBranch(ctx, branch)
	if err != nil {
		return err
	}
	stage := "develop"
	if pr != nil && s.stateOf(issue.Labels) == "review" {
		stage = "review"
	}
	// afterDevelop is where a developer session leads: a review, or — when
	// the developer is fixing failing checks after approval — straight back
	// to the checks.
	afterDevelop := "review"
	maxRounds := s.cfg.Scheduler.MaxReviewRounds
	policy := s.cfg.Merge()

	for {
		if ctx.Err() != nil {
			return ctx.Err()
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
			if afterDevelop == "checks" {
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
			if err := s.mail.MarkRead(inbox...); err != nil {
				log.Warn("mark mail read", "err", err)
			}
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
				if afterDevelop == "checks" {
					stage = "checks"
					continue
				}
				if err := s.setState(ctx, issue.Number, s.labels.Review); err != nil {
					return err
				}
				stage = "review"
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
			// Make sure the worktree has the developer's latest commits.
			if err := s.ws.Fetch(ctx); err == nil {
				_, _ = gitPull(ctx, ws.RepoDir)
			}
			name := fmt.Sprintf("reviewer-pr-%d-r%d", pr.Number, bookkeeping.Round)
			log.Info("reviewer session", "pr", pr.Number, "round", bookkeeping.Round)
			started := s.now()
			res, err := s.runSessionWithRetry(ctx, sessionSpec{
				role: config.RoleReviewer, name: name, workDir: ws.RepoDir, branch: branch, worker: w,
				data: prompts.Data{Issue: &freshIssue, PR: &freshPR, PreviousRounds: previous, Round: bookkeeping.Round, MaxRounds: maxRounds},
			})
			if err != nil {
				return err
			}
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

		case "checks":
			round := bookkeeping.CheckFixRounds + 1
			s.updateWorker(w, "checks", round)
			log.Info("waiting for checks", "pr", pr.Number, "wait", policy.ChecksWait)
			status, checks, gate, err := s.awaitChecks(ctx, pr.Number, policy, w, round)
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
			if bookkeeping.CheckFixRounds >= policy.MaxCheckFixRounds {
				return s.escalate(ctx, issue.Number, fmt.Sprintf("Checks on #%d still fail after %d fix rounds: %s", pr.Number, policy.MaxCheckFixRounds, checkNames(github.Failed(checks))))
			}
			bookkeeping.CheckFixRounds++
			_ = s.store.SaveIssue(bookkeeping)
			freshPR, err := s.gh.GetPR(ctx, pr.Number)
			if err != nil {
				return err
			}
			freshIssue, err := s.gh.GetIssue(ctx, issue.Number)
			if err != nil {
				return err
			}
			if err := s.ws.Fetch(ctx); err == nil {
				_, _ = gitPull(ctx, ws.RepoDir)
			}
			name := fmt.Sprintf("reviewer-pr-%d-checks%d", pr.Number, bookkeeping.CheckFixRounds)
			log.Info("checks failed; reviewer diagnosing", "pr", pr.Number, "gate", string(gate), "round", bookkeeping.CheckFixRounds, "checks", checkNames(github.Failed(checks)))
			started := s.now()
			res, err := s.runSessionWithRetry(ctx, sessionSpec{
				role: config.RoleReviewer, name: name, task: "reviewer_checks", workDir: ws.RepoDir, branch: branch, worker: w,
				data: prompts.Data{Issue: &freshIssue, PR: &freshPR, FailedChecks: github.Failed(checks), Round: bookkeeping.CheckFixRounds, MaxRounds: policy.MaxCheckFixRounds},
				env:  map[string]string{"BEES_REVIEW_MODE": "checks"},
			})
			if err != nil {
				return err
			}
			outcome, note := outcomeOf(res)
			switch outcome {
			case OutcomeChangesRequested:
				if !s.sentSince(config.RoleDeveloper, 0, pr.Number, started) {
					return s.escalate(ctx, issue.Number, "The reviewer diagnosed failing checks but sent nothing to the developer. Note: "+note)
				}
				afterDevelop = "checks"
				stage = "develop"
			case OutcomeApproved:
				log.Info("reviewer re-ran the checks; waiting again", "pr", pr.Number)
			default:
				return s.escalate(ctx, issue.Number, fmt.Sprintf("The failing checks on #%d were not diagnosed. %s", pr.Number, s.sessionFailure(config.RoleReviewer, res, outcome, note)))
			}
		}
	}
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

// awaitChecks waits policy.ChecksWait, then polls the PR's checks every
// ChecksPollInterval until they pass, fail, or ChecksTimeout elapses, and
// reports which gate it settled on.
//
// The required checks are asked for first and win outright: when the branch
// requires anything, the second `gh pr checks` call is never made and the
// behaviour is exactly what it was before #117. Only when nothing is required
// does the wait fall back to every reported check — a repository with no
// branch protection would otherwise merge with nothing green at all. Two
// consecutive empty observations are needed before concluding there is no CI,
// because a workflow can take longer than checks_wait to register.
func (s *Scheduler) awaitChecks(ctx context.Context, pr int, policy config.MergePolicy, w *state.Worker, round int) (github.ChecksStatus, []github.Check, checksGate, error) {
	if err := sleepCtx(ctx, policy.ChecksWait); err != nil {
		return "", nil, gateUnknown, err
	}
	deadline := s.now().Add(policy.ChecksTimeout)
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
				s.setChecksGate(w, gateNone, round)
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
		s.setChecksGate(w, gate, round)
		if status != github.ChecksPending || !s.now().Before(deadline) {
			return status, checks, gate, nil
		}
		if err := sleepCtx(ctx, policy.ChecksPollInterval); err != nil {
			return "", nil, gate, err
		}
	}
}

// setChecksGate names the gate in the worker stage, so a worker sitting in a
// 30-minute wait shows in `bees status` what it is waiting for.
func (s *Scheduler) setChecksGate(w *state.Worker, gate checksGate, round int) {
	if w == nil || gate == gateUnknown {
		return
	}
	stage := "checks (" + string(gate) + ")"
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
	var errs []error
	if !github.HasLabel(labels, s.labels.Base) {
		if err := s.gh.EditLabels(ctx, number, []string{s.labels.Base}, nil); err != nil {
			errs = append(errs, fmt.Errorf("add the %s label: %w", s.labels.Base, err))
		}
	}
	if a := s.cfg.Filter.Assignee; a != "" && !github.HasAssignee(assignees, a) {
		if err := s.gh.Assign(ctx, number, a); err != nil {
			errs = append(errs, fmt.Errorf("assign it to %s: %w", a, err))
		}
	}
	if m := s.cfg.Filter.Milestone; isPR && m != "" && milestone != m {
		if err := s.gh.SetMilestone(ctx, number, m); err != nil {
			errs = append(errs, fmt.Errorf("put it in milestone %s: %w", m, err))
		}
	}
	return errors.Join(errs...)
}

func gitPull(ctx context.Context, dir string) (string, error) {
	out, err := gitRun(ctx, dir, "pull", "--ff-only", "--quiet")
	if err != nil && strings.Contains(err.Error(), "no tracking information") {
		return out, nil
	}
	return out, err
}
