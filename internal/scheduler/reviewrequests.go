package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/state"
)

// requestedReviewStage is what `bees status` and the live view show for a
// worker that is reviewing a pull request a person asked about, where a
// developer worker would show its stage.
const requestedReviewStage = "requested review"

// requestedReviewKey is the backoff key of a requested review, per pull
// request, next to the developer workers' issue-N keys.
func requestedReviewKey(pr int) string { return fmt.Sprintf("requested-review-pr-%d", pr) }

// dispatchRequestedReviews runs one reviewer session for every open pull
// request in the poll that carries bees:review-requested: a person's way
// to ask the factory for a review of a pull request it did not write,
// their own or anyone's. The pull request needs the factory label (and the
// assignee, with filter.assignee set) to be in the poll at all.
//
// Every review takes a slot from the developer pool, so
// scheduler.max_developers stays the one number that bounds how many
// sessions the factory runs at once, and it runs after dispatchDevelopers
// so a review request never starves a ready issue. The worker is recorded
// in s.owned under the pull request's number — GitHub numbers issues and
// pull requests from one sequence, so it cannot collide with an issue a
// developer holds — which is what puts it in status.json, the live view
// and `bees kill`.
//
// The label is removed before the session starts, not after: removing it
// claims the request, so one label is exactly one pass whatever the
// session then does, a failure or a scheduler killed mid-session included.
// A crash between the removal and the session loses that one request, and
// a person adds the label again; the alternative, removing it afterwards,
// would re-run the review on every poll for as long as the crash repeats.
// Only a full pass calls this: a local pass classifies the pull requests
// cached from the last poll, which still carry a label that has since
// been removed on GitHub, and would dispatch the same review twice.
func (s *Scheduler) dispatchRequestedReviews(ctx context.Context, snap *snapshot) {
	// The same gates as dispatchDevelopers, for the same reasons.
	if ctx.Err() != nil || !s.roleEnabled(config.RoleReviewer) || s.limitPaused() || s.dayBudgetReached() {
		return
	}
	// The poll lists open pull requests only, so there is no state to check.
	for _, pr := range snap.prs {
		if !github.HasLabel(pr.Labels, s.labels.ReviewRequested) {
			continue
		}
		s.mu.Lock()
		_, taken := s.owned[pr.Number]
		s.mu.Unlock()
		if taken {
			continue
		}
		key := requestedReviewKey(pr.Number)
		if until, ok := s.backoffUntil(key); ok && s.now().Before(until) {
			continue
		}
		select {
		case <-s.slots:
		default:
			return // pool is full
		}
		if err := s.gh.EditLabels(ctx, pr.Number, nil, []string{s.labels.ReviewRequested}); err != nil {
			// Nothing was claimed: the label is still there, and the next
			// poll tries again.
			s.slots <- struct{}{}
			s.log.Warn("could not claim the review request", "pr", pr.Number, "err", err)
			continue
		}
		s.log.Info("review requested on a pull request", "pr", pr.Number, "title", pr.Title, "author", pr.Author.Login)
		// Issue holds the pull request's number: see the comment above.
		w := &state.Worker{Name: fmt.Sprintf("review-%d", pr.Number), Issue: pr.Number, Stage: requestedReviewStage, Round: 1, Since: s.now()}
		s.mu.Lock()
		s.owned[pr.Number] = w
		s.mu.Unlock()
		s.writeStatus()
		s.wg.Add(1)
		go func(pr github.PR, w *state.Worker) {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.owned, pr.Number)
				s.mu.Unlock()
				s.slots <- struct{}{}
				s.writeStatus()
				// As in dispatchDevelopers: the slot is back in the pool
				// only now, after the session's own signal.
				s.signal()
			}()
			// There is no issue to escalate: a failed review is logged and
			// the pull request backed off, and the person asks again.
			if err := s.runRequestedReview(ctx, pr, w); err != nil && s.sessionContext(ctx).Err() == nil {
				s.log.Error("requested review failed", "pr", pr.Number, "err", err)
				s.setBackoff(key, 5*s.cfg.Scheduler.PollInterval.Duration)
			}
		}(pr, w)
	}
}

// runRequestedReview runs the reviewer once on a pull request a person
// asked about, in a detached checkout of its head branch. There is no issue
// and no developer session: prompts.Data.Issue stays nil, and the reviewer
// renders the reviewer_requested task rather than the review-loop one.
//
// The checkout is read-only, the form QA uses: a reviewer commits nothing.
// A head branch the remote does not have — a pull request from a fork, or a
// branch deleted since — falls back to a checkout of the default branch,
// logged rather than fatal: `gh pr diff` reads a fork's diff, and the
// session is worth more than a checkout.
func (s *Scheduler) runRequestedReview(ctx context.Context, pr github.PR, w *state.Worker) error {
	// Under the sessions' context, as a developer worker is: a cool-down
	// lets a review that has started finish.
	ctx = s.sessionContext(ctx)
	log := s.log.With("worker", w.Name, "pr", pr.Number, "branch", pr.HeadRefName)
	if err := s.ws.Fetch(ctx); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	ws, err := s.ws.Detached(ctx, w.Name, pr.HeadRefName)
	if err != nil {
		log.Warn("head branch is not on the remote; reviewing from the default branch", "err", err)
		if ws, err = s.ws.Detached(ctx, w.Name, s.cfg.Project.DefaultBranch); err != nil {
			return fmt.Errorf("workspace: %w", err)
		}
	}
	defer func() {
		if err := s.ws.Remove(context.WithoutCancel(ctx), ws); err != nil {
			log.Warn("workspace cleanup failed", "err", err)
		}
	}()
	freshPR, err := s.gh.GetPR(ctx, pr.Number)
	if err != nil {
		return err
	}
	inbox, err := s.inbox(config.RoleReviewer, 0, pr.Number)
	if err != nil {
		return err
	}
	stages := s.cfg.ReviewStages()
	name := fmt.Sprintf("reviewer-requested-pr-%d", pr.Number)
	log.Info("requested review session", "mail", len(inbox))
	res, err := s.runSessionWithRetry(ctx, sessionSpec{
		role: config.RoleReviewer, name: name, task: "reviewer_requested", workDir: ws.RepoDir, worker: w,
		// Mode switches the reviewer's prompts to the requested review; ActsAs
		// tells it whose approval GitHub would refuse (its own author's).
		data: prompts.Data{PR: &freshPR, Inbox: inbox, Stages: stages, Round: 1, Mode: prompts.ModeRequested, ActsAs: s.gh.ActsAs},
	})
	if err != nil {
		return err
	}
	readErr := s.mail.MarkRead(inbox...)
	s.op("mail", readErr, "mark mail read", "err", readErr)
	status, note := outcomeOf(res)
	switch status {
	case OutcomeApproved, OutcomeChangesRequested:
		log.Info("requested review finished", "outcome", status, "note", oneLine(note, 200))
		return nil
	default:
		return errors.New(s.sessionFailure(config.RoleReviewer, res, status, note))
	}
}
