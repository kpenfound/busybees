package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/workspace"
)

// ---- project manager -------------------------------------------------------

func (s *Scheduler) projectManagerHasWork(snap *snapshot) bool {
	return len(snap.byState["triage"]) > 0 || s.hasUnreadMail(config.RoleProjectManager, 0, 0)
}

func (s *Scheduler) runProjectManager(ctx context.Context, snap *snapshot) error {
	triage := snap.byState["triage"]
	if n := s.cfg.Scheduler.TriageBatchSize; len(triage) > n {
		triage = triage[:n]
	}
	var full []github.Issue
	parents := map[int]github.Parent{}
	for _, i := range triage {
		fi, err := s.gh.GetIssue(ctx, i.Number)
		if err != nil {
			return err
		}
		full = append(full, fi)
		if p, err := s.gh.ParentIssue(ctx, i.Number); err == nil && p != nil {
			parents[i.Number] = *p
		}
	}
	var others []github.Issue
	inTriage := map[int]bool{}
	for _, i := range triage {
		inTriage[i.Number] = true
	}
	for _, i := range snap.issues {
		if !inTriage[i.Number] {
			others = append(others, i)
		}
	}
	// Open blockers of every visible work item, so the prompt can show what
	// is waiting on what.
	blockers := map[int][]int{}
	for _, i := range snap.issues {
		if github.HasLabel(i.Labels, s.labels.Feature) || github.HasLabel(i.Labels, s.labels.Feedback) {
			continue
		}
		if w := waitingOn(i, snap.open); len(w) > 0 {
			blockers[i.Number] = w
		}
	}
	inbox, err := s.inbox(config.RoleProjectManager, 0, 0)
	if err != nil {
		return err
	}
	return s.runSingleton(ctx, config.RoleProjectManager, prompts.Data{TriageIssues: full, Issues: others, Inbox: inbox, PRs: snap.prs, Parents: parents, Blockers: blockers})
}

// ---- product manager -------------------------------------------------------

func (s *Scheduler) productManagerHasWork(ctx context.Context, snap *snapshot) bool {
	if s.hasUnreadMail(config.RoleProductManager, 0, 0) {
		return true
	}
	rs, err := s.store.Role(config.RoleProductManager)
	if err != nil || rs.LastRun.IsZero() || s.now().Sub(rs.LastRun) >= s.cfg.Scheduler.ProductManagerInterval.Duration {
		return true
	}
	fresh, err := s.freshIssues(ctx, append(append([]github.Issue{}, snap.feedback...), snap.features...), rs.LastRun)
	return err == nil && len(fresh) > 0
}

// freshIssues returns the issues (feedback or feature) a human has created
// or commented on since the product manager last replied on them. Comments
// are only fetched for issues updated since lastRun. An answered question
// loses its question label so people can see it is no longer waiting.
func (s *Scheduler) freshIssues(ctx context.Context, issues []github.Issue, lastRun time.Time) ([]github.Issue, error) {
	var fresh []github.Issue
	for _, i := range issues {
		if !lastRun.IsZero() && !i.UpdatedAt.After(lastRun) {
			continue
		}
		full, err := s.gh.GetIssue(ctx, i.Number)
		if err != nil {
			return nil, err
		}
		if !full.AwaitingBee() {
			continue
		}
		if github.HasLabel(full.Labels, s.labels.Question) {
			s.log.Info("person answered the product manager", "issue", full.Number)
			if err := s.gh.EditLabels(ctx, full.Number, nil, []string{s.labels.Question}); err != nil {
				s.log.Warn("remove question label", "issue", full.Number, "err", err)
			}
		}
		fresh = append(fresh, full)
	}
	return fresh, nil
}

func (s *Scheduler) runProductManager(ctx context.Context, snap *snapshot) error {
	milestones, err := s.gh.ListMilestones(ctx)
	if err != nil {
		return err
	}
	inbox, err := s.inbox(config.RoleProductManager, 0, 0)
	if err != nil {
		return err
	}
	feedback, err := s.freshIssues(ctx, snap.feedback, time.Time{})
	if err != nil {
		return err
	}
	freshFeatures, err := s.freshIssues(ctx, snap.features, time.Time{})
	if err != nil {
		return err
	}
	// Sub-issue progress of every feature, from GitHub.
	progress := map[int]github.SubIssueSummary{}
	for _, f := range snap.features {
		if d, err := s.gh.GetIssueDetails(ctx, f.Number); err == nil {
			progress[f.Number] = d.SubIssues
		} else {
			s.log.Warn("feature progress", "issue", f.Number, "err", err)
		}
	}
	// Work items only: feature and feedback issues are listed separately.
	var work []github.Issue
	for _, i := range snap.issues {
		if !github.HasLabel(i.Labels, s.labels.Feature) && !github.HasLabel(i.Labels, s.labels.Feedback) {
			work = append(work, i)
		}
	}
	return s.runSingleton(ctx, config.RoleProductManager, prompts.Data{
		Issues: work, PRs: snap.prs, Milestones: milestones, Inbox: inbox,
		Feedback: feedback, FreshFeatures: freshFeatures, Features: snap.features, Progress: progress,
	})
}

// ---- QA --------------------------------------------------------------------

// qaWindow is how far back QA looks on its very first run.
const qaWindow = 7 * 24 * time.Hour

func (s *Scheduler) qaSince() time.Time {
	rs, _ := s.store.Role(config.RoleQA)
	if rs.LastRun.IsZero() {
		return s.now().Add(-qaWindow)
	}
	return rs.LastRun
}

// qaHasWork asks GitHub for merged PRs at most once per qa_interval.
func (s *Scheduler) qaHasWork(ctx context.Context) bool {
	rs, _ := s.store.Role(config.RoleQA)
	if rs.LastRun.IsZero() {
		return true
	}
	interval := s.cfg.Scheduler.QAInterval.Duration
	last := rs.LastRun
	if rs.LastCheck.After(last) {
		last = rs.LastCheck
	}
	if s.now().Sub(last) < interval {
		return false
	}
	rs.LastCheck = s.now()
	_ = s.store.SaveRole(config.RoleQA, rs)
	merged, err := s.gh.ListMergedPRsSince(ctx, s.query, rs.LastRun)
	if err != nil {
		s.log.Warn("list merged PRs", "err", err)
		return false
	}
	return len(merged) > 0
}

func (s *Scheduler) runQA(ctx context.Context, snap *snapshot) error {
	since := s.qaSince()
	merged, err := s.gh.ListMergedPRsSince(ctx, s.query, since)
	if err != nil {
		return err
	}
	var bugs []github.Issue
	for _, i := range snap.issues {
		if github.HasLabel(i.Labels, s.labels.Bug) {
			bugs = append(bugs, i)
		}
	}
	last := ""
	if rs, _ := s.store.Role(config.RoleQA); !rs.LastRun.IsZero() {
		last = rs.LastRun.Format(time.RFC1123)
	}
	return s.runSingleton(ctx, config.RoleQA, prompts.Data{MergedPRs: merged, Issues: bugs, LastRun: last})
}

// ---- shared ----------------------------------------------------------------

// runSingleton runs one session for a singleton role in a detached checkout
// of the default branch, marks its mail read and records the run.
func (s *Scheduler) runSingleton(ctx context.Context, role string, data prompts.Data) error {
	if err := s.ws.Fetch(ctx); err != nil {
		return fmt.Errorf("fetch: %w", err)
	}
	ws, err := s.ws.Detached(ctx, role, s.cfg.Project.DefaultBranch)
	if err != nil {
		return fmt.Errorf("workspace: %w", err)
	}
	defer func() {
		if err := s.ws.Remove(context.WithoutCancel(ctx), ws); err != nil {
			s.log.Warn("workspace cleanup failed", "role", role, "err", err)
		}
	}()
	started := s.now()
	name := role + "-" + started.Format("0102-1504")
	res, err := s.runSessionWithRetry(ctx, sessionSpec{role: role, name: name, workDir: ws.RepoDir, data: data})
	if err != nil {
		return err
	}
	if err := s.mail.MarkRead(data.Inbox...); err != nil {
		s.log.Warn("mark mail read", "role", role, "err", err)
	}
	if err := s.store.SaveRole(role, state.RoleState{LastRun: started}); err != nil {
		return err
	}
	status, note := outcomeOf(res)
	s.log.Info("singleton finished", "role", role, "outcome", status, "note", oneLine(note, 200))
	if status == OutcomeFailed {
		return errors.New(s.sessionFailure(role, res, status, note))
	}
	return nil
}

// RunRole runs a single session for a role outside the scheduler loop
// (`bees exec`). Developer and reviewer runs go through the full worker
// loop for the given issue so label transitions stay consistent.
func (s *Scheduler) RunRole(ctx context.Context, role string, issue, pr int) error {
	if err := s.store.Init(); err != nil {
		return err
	}
	switch role {
	case config.RoleDeveloper, config.RoleReviewer:
		if issue == 0 && pr > 0 {
			p, err := s.gh.GetPR(ctx, pr)
			if err != nil {
				return err
			}
			if refs := p.ClosingIssues(); len(refs) > 0 {
				issue = refs[0]
			}
		}
		if issue == 0 {
			return fmt.Errorf("%s needs --issue (or --pr that closes an issue)", role)
		}
		i, err := s.gh.GetIssue(ctx, issue)
		if err != nil {
			return err
		}
		if role == config.RoleReviewer && s.stateOf(i.Labels) != "review" {
			// Force the worker into the review stage.
			if err := s.setState(ctx, issue, s.labels.Review); err != nil {
				return err
			}
			i.Labels = relabel(i.Labels, s.stateOf(i.Labels), s.labels.Review)
		}
		w := &state.Worker{Name: "exec-" + role, Issue: issue, Size: s.sizeOf(i.Labels), Since: s.now()}
		s.mu.Lock()
		s.owned[issue] = w
		s.mu.Unlock()
		defer func() {
			s.mu.Lock()
			delete(s.owned, issue)
			s.mu.Unlock()
		}()
		return s.workIssue(ctx, i, w)
	case config.RoleProjectManager, config.RoleProductManager, config.RoleQA:
		snap, err := s.poll(ctx)
		if err != nil {
			return err
		}
		if err := s.reconcile(ctx, snap); err != nil {
			s.log.Warn("reconcile", "err", err)
		}
		switch role {
		case config.RoleProjectManager:
			return s.runProjectManager(ctx, snap)
		case config.RoleProductManager:
			return s.runProductManager(ctx, snap)
		default:
			return s.runQA(ctx, snap)
		}
	}
	return fmt.Errorf("unknown role %q", role)
}

func gitRun(ctx context.Context, dir string, args ...string) (string, error) {
	return workspace.Git(ctx, dir, args...)
}
