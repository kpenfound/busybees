package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/state"
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
			res, err := s.runSession(ctx, sessionSpec{
				role: config.RoleDeveloper, name: name, workDir: ws.RepoDir, branch: branch,
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
				if err := s.ensureVisible(ctx, pr.Number, pr.Labels); err != nil {
					log.Warn("label PR", "err", err)
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
				return s.escalate(ctx, issue.Number, fmt.Sprintf("The developer session ended with `%s`: %s", status, note))
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
			res, err := s.runSession(ctx, sessionSpec{
				role: config.RoleReviewer, name: name, workDir: ws.RepoDir, branch: branch,
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
					return s.escalate(ctx, issue.Number, fmt.Sprintf("Pull request #%d was not approved after %d review rounds. The reviewer's last feedback is in the busybees mailbox.", pr.Number, maxRounds))
				}
				bookkeeping.Round++
				_ = s.store.SaveIssue(bookkeeping)
				log.Info("changes requested; back to developer", "round", bookkeeping.Round)
				stage = "develop"
			default:
				return s.escalate(ctx, issue.Number, fmt.Sprintf("The reviewer session ended with `%s`: %s", status, note))
			}

		case "checks":
			s.updateWorker(w, "checks", bookkeeping.CheckFixRounds+1)
			log.Info("waiting for required checks", "pr", pr.Number, "wait", policy.ChecksWait)
			status, checks, err := s.awaitChecks(ctx, pr.Number, policy)
			if err != nil {
				return err
			}
			switch status {
			case github.ChecksPassed:
				log.Info("required checks passed; merging", "pr", pr.Number, "method", policy.Method)
				if err := s.gh.MergePR(ctx, pr.Number, policy.Method); err != nil {
					return s.escalate(ctx, issue.Number, fmt.Sprintf("Required checks passed but merging #%d failed (branch protection may need a human): %v", pr.Number, err))
				}
				return nil
			case github.ChecksPending:
				return s.escalate(ctx, issue.Number, fmt.Sprintf("Required checks on #%d were still pending after %s.", pr.Number, policy.ChecksTimeout))
			}
			// Checks failed: the reviewer diagnoses, the developer fixes.
			if bookkeeping.CheckFixRounds >= policy.MaxCheckFixRounds {
				return s.escalate(ctx, issue.Number, fmt.Sprintf("Required checks on #%d still fail after %d fix rounds: %s", pr.Number, policy.MaxCheckFixRounds, checkNames(github.Failed(checks))))
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
			log.Info("required checks failed; reviewer diagnosing", "pr", pr.Number, "round", bookkeeping.CheckFixRounds, "checks", checkNames(github.Failed(checks)))
			started := s.now()
			res, err := s.runSession(ctx, sessionSpec{
				role: config.RoleReviewer, name: name, task: "reviewer_checks", workDir: ws.RepoDir, branch: branch,
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
				return s.escalate(ctx, issue.Number, fmt.Sprintf("The reviewer could not diagnose the failing checks on #%d (`%s`): %s", pr.Number, outcome, note))
			}
		}
	}
}

// awaitChecks waits policy.ChecksWait, then polls the PR's required checks
// every ChecksPollInterval until they pass, fail, or ChecksTimeout elapses.
func (s *Scheduler) awaitChecks(ctx context.Context, pr int, policy config.MergePolicy) (github.ChecksStatus, []github.Check, error) {
	if err := sleepCtx(ctx, policy.ChecksWait); err != nil {
		return "", nil, err
	}
	deadline := s.now().Add(policy.ChecksTimeout)
	for {
		checks, err := s.gh.RequiredChecks(ctx, pr)
		if err != nil {
			return "", nil, err
		}
		status := github.Summarize(checks)
		if status != github.ChecksPending || !s.now().Before(deadline) {
			return status, checks, nil
		}
		if err := sleepCtx(ctx, policy.ChecksPollInterval); err != nil {
			return "", nil, err
		}
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

// ensureVisible makes sure a PR created by a session matches the filter.
func (s *Scheduler) ensureVisible(ctx context.Context, number int, labels []github.Label) error {
	if !github.HasLabel(labels, s.labels.Base) {
		if err := s.gh.EditLabels(ctx, number, []string{s.labels.Base}, nil); err != nil {
			return err
		}
	}
	if a := s.cfg.Filter.Assignee; a != "" {
		return s.gh.Assign(ctx, number, a)
	}
	return nil
}

func gitPull(ctx context.Context, dir string) (string, error) {
	out, err := gitRun(ctx, dir, "pull", "--ff-only", "--quiet")
	if err != nil && strings.Contains(err.Error(), "no tracking information") {
		return out, nil
	}
	return out, err
}
