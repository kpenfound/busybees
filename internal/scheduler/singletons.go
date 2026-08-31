package scheduler

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	if len(s.approvedSince(snap.features, rs.LastRun)) > 0 {
		return true
	}
	if len(s.completedFeatures(snap)) > 0 {
		return true
	}
	fresh, err := s.freshIssues(ctx, append(append([]github.Issue{}, snap.feedback...), snap.features...), rs.LastRun)
	if err != nil {
		return false
	}
	// A proposal only counts as work once a person has commented on it: it
	// waits for a person to approve it, and AwaitingBee counts an issue's
	// creation as human activity, so an untouched bee-written proposal stays
	// "fresh" forever and would wake the product manager on every single poll
	// for a decision it cannot make. A person's question, on the other hand,
	// deserves an answer as promptly as on any other issue; freshIssues only
	// looks at issues updated since the last run, so the proposal goes quiet
	// again on the poll after that.
	//
	// An issue in planning mode is the same shape for the same reason: it
	// waits for a person too, the run it would start can only reply to what
	// they wrote, and a fresh comment is exactly the event worth waking for.
	for _, i := range fresh {
		if github.HasLabel(i.Labels, s.labels.Proposal) || github.HasLabel(i.Labels, s.labels.Planning) {
			if s.gh.AwaitingBeeComment(i) {
				return true
			}
			continue
		}
		return true
	}
	return false
}

// approvedSince returns the features a person approved (removed
// bees:proposal from) after t, as recorded by reconcile. Approval leaves no
// comment, so this is the only signal that brings a proposal back to the
// product manager.
func (s *Scheduler) approvedSince(features []github.Issue, t time.Time) []int {
	var out []int
	for _, f := range features {
		is, err := s.store.Issue(f.Number)
		if err != nil {
			s.log.Warn("read issue state", "issue", f.Number, "err", err)
			continue
		}
		if !is.ProposalApprovedAt.IsZero() && is.ProposalApprovedAt.After(t) {
			out = append(out, f.Number)
		}
	}
	return out
}

// completedFeatures returns the open features whose work is done: every
// sub-issue the product manager last saw open has closed since, and the
// product manager has not been told about it yet. Nothing here asks GitHub:
// the child numbers were recorded by the last runProductManager and
// snap.open comes from the poll, so the check costs no call on the polling
// path — which is the whole point, since it runs on every pass.
//
// A feature whose children all closed before the scheduler ever recorded
// them, or that no product manager session has ever seen, has no recorded
// set and does not fire. It reaches the product manager on the next run for
// any other reason, which product_manager_interval guarantees.
func (s *Scheduler) completedFeatures(snap *snapshot) []github.Issue {
	var out []github.Issue
	for _, f := range snap.features {
		is, err := s.store.Issue(f.Number)
		if err != nil {
			s.log.Warn("read issue state", "issue", f.Number, "err", err)
			continue
		}
		if len(is.OpenChildren) == 0 || !is.CompleteReportedAt.IsZero() {
			continue
		}
		done := true
		for _, c := range is.OpenChildren {
			if snap.open[c] {
				done = false
				break
			}
		}
		if done {
			out = append(out, f)
		}
	}
	return out
}

// recordFeatureProgress remembers, for every open feature, which of its
// sub-issues are open right now, so a later pass can notice locally that they
// have all closed. The numbers come from the parents map runProductManager
// already builds; reported are the features presented as complete in this
// run, which are marked so the trigger fires once.
//
// A feature with no open children is not recorded over the set it had: that
// is the state the completeness check exists to spot, and forgetting the
// children would also lose what the re-arm rule compares against. For the
// same reason a run whose ParentIssue queries did not all answer (complete is
// false) records nothing: a truncated answer is indistinguishable from
// children that closed, and would report a feature complete while a real
// sub-issue is still open. A set that did change clears the marker, so a
// feature that gains a sub-issue after being reported complete is reported
// again when that one closes.
func (s *Scheduler) recordFeatureProgress(snap *snapshot, parents map[int]github.Parent, reported []github.Issue, complete bool) {
	children := map[int][]int{}
	for item, p := range parents {
		children[p.Number] = append(children[p.Number], item)
	}
	done := map[int]bool{}
	for _, f := range reported {
		done[f.Number] = true
	}
	for _, f := range snap.features {
		openNow := children[f.Number]
		slices.Sort(openNow)
		is, err := s.store.Issue(f.Number)
		if err != nil {
			s.log.Warn("read issue state", "issue", f.Number, "err", err)
			continue
		}
		changed := complete && len(openNow) > 0 && !slices.Equal(is.OpenChildren, openNow)
		if !changed && !done[f.Number] {
			continue
		}
		if changed {
			is.OpenChildren = openNow
			is.CompleteReportedAt = time.Time{}
		}
		if done[f.Number] {
			is.CompleteReportedAt = s.now()
		}
		if err := s.store.SaveIssue(is); err != nil {
			s.log.Warn("save issue state", "issue", f.Number, "err", err)
		}
	}
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
		if !s.gh.AwaitingBee(full) {
			continue
		}
		if github.HasLabel(full.Labels, s.labels.Question) {
			s.log.Info("person answered the product manager", "issue", full.Number)
			err := s.gh.EditLabels(ctx, full.Number, nil, []string{s.labels.Question})
			s.op("label", err, "remove question label", "issue", full.Number, "err", err)
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
	fresh, err := s.freshIssues(ctx, snap.features, time.Time{})
	if err != nil {
		return err
	}
	// Sub-issue progress of every feature, from GitHub. Read before the fresh
	// lists are partitioned: it is also the record that says whether a
	// planned feature has already been broken down.
	progress := map[int]github.SubIssueSummary{}
	for _, f := range snap.features {
		d, err := s.gh.GetIssueDetails(ctx, f.Number)
		if !s.op("feature-progress", err, "feature progress", "issue", f.Number, "err", err) {
			progress[f.Number] = d.SubIssues
		}
	}
	// Issues a person has agreed with the product manager (bees:planned) and
	// that still need acting on. A planned feature drops off this list by
	// gaining sub-issues — the summary above is the "has it been broken down
	// already?" answer, so a later run cannot break it down twice — and a
	// planned feedback issue by being closed, which takes it out of the poll.
	fetched := map[int]github.Issue{}
	for _, i := range append(append([]github.Issue{}, feedback...), fresh...) {
		fetched[i.Number] = i
	}
	var planned []github.Issue
	for _, i := range append(append([]github.Issue{}, snap.features...), snap.feedback...) {
		if !github.HasLabel(i.Labels, s.labels.Planned) || github.HasLabel(i.Labels, s.labels.Planning) {
			continue
		}
		// A proposal is agreed by a person removing bees:proposal. While it
		// is still there the issue is not approved, and issues.Create refuses
		// it as a parent, so presenting it as agreed would ask for a
		// breakdown the tools go on to refuse. It stays a proposal.
		if github.HasLabel(i.Labels, s.labels.Proposal) {
			continue
		}
		// The sub-issue summary answers "has this been broken down already?"
		// only when it could be read. A failed lookup leaves no entry at all,
		// and a missing entry is not evidence of no sub-issues: taking it for
		// one would hand back a feature broken down weeks ago, under a
		// heading saying the scope is settled. Fail closed; it comes back on
		// the next run.
		if github.HasLabel(i.Labels, s.labels.Feature) {
			if p, ok := progress[i.Number]; !ok || p.Total > 0 {
				continue
			}
		}
		full, ok := fetched[i.Number]
		if !ok {
			full, err = s.gh.GetIssue(ctx, i.Number)
			if err != nil {
				return err
			}
		}
		planned = append(planned, full)
	}
	// An issue in planning mode is a conversation: it leaves both fresh lists
	// for a section of its own, which lists no breakdown step. An agreed one
	// leaves them for the same reason — its section is the one that says the
	// scope is settled, and two sections would give two sets of instructions
	// for one issue.
	var planning []github.Issue
	agreed := map[int]bool{}
	for _, i := range planned {
		agreed[i.Number] = true
	}
	split := func(list []github.Issue) []github.Issue {
		var rest []github.Issue
		for _, i := range list {
			switch {
			case github.HasLabel(i.Labels, s.labels.Planning):
				planning = append(planning, i)
			case agreed[i.Number]:
			default:
				rest = append(rest, i)
			}
		}
		return rest
	}
	feedback, fresh = split(feedback), split(fresh)
	// Proposals are partitioned out of the fresh features rather than kept
	// out of freshIssues: that call is also what clears bees:question when a
	// person answers, and the product manager may well have asked its
	// question on a proposal.
	var freshFeatures, proposals []github.Issue
	seen := map[int]bool{}
	for _, f := range fresh {
		if github.HasLabel(f.Labels, s.labels.Proposal) {
			proposals = append(proposals, f)
			continue
		}
		seen[f.Number] = true
		freshFeatures = append(freshFeatures, f)
	}
	// A feature a person approved since the last run reaches the product
	// manager on this run whatever AwaitingBee says: the approval is a label
	// edit, so no comment ever makes the feature fresh again.
	rs, _ := s.store.Role(config.RoleProductManager)
	for _, n := range s.approvedSince(snap.features, rs.LastRun) {
		if seen[n] {
			continue
		}
		full, err := s.gh.GetIssue(ctx, n)
		if err != nil {
			return err
		}
		if github.HasLabel(full.Labels, s.labels.Planning) || agreed[n] {
			continue
		}
		s.log.Info("approved proposal goes to the product manager", "issue", n)
		freshFeatures = append(freshFeatures, full)
	}
	// Work items only: feature and feedback issues are listed separately.
	var work []github.Issue
	for _, i := range snap.issues {
		if !github.HasLabel(i.Labels, s.labels.Feature) && !github.HasLabel(i.Labels, s.labels.Feedback) {
			work = append(work, i)
		}
	}
	// The feature each work item is attached to, so the prompt can show what
	// is still loose. GitHub's sub-issue summary carries counts only, not the
	// child numbers, so the progress calls above cannot answer this: it is one
	// GraphQL query per work item, as runProjectManager does for its triage
	// items.
	parents := map[int]github.Parent{}
	parentsComplete := true
	for _, i := range work {
		p, err := s.gh.ParentIssue(ctx, i.Number)
		if s.op("work-item-parent", err, "work item parent", "issue", i.Number, "err", err) {
			parentsComplete = false
			continue
		}
		if p != nil {
			parents[i.Number] = *p
		}
	}
	// Features whose every recorded sub-issue has closed: the prompt asks the
	// product manager to decide whether to close them, and recording the open
	// children (and that these were reported) is what makes the next pass's
	// local check possible.
	complete := s.completedFeatures(snap)
	for _, f := range complete {
		s.log.Info("feature work is complete", "issue", f.Number)
	}
	if err := s.runSingleton(ctx, config.RoleProductManager, prompts.Data{
		Issues: work, PRs: snap.prs, Milestones: milestones, Inbox: inbox,
		Feedback: feedback, FreshFeatures: freshFeatures, Proposals: proposals,
		Planning: planning, Planned: planned,
		Features: snap.features, Progress: progress, Parents: parents,
		CompletedFeatures: complete,
	}); err != nil {
		// No session delivered the report, so the trigger is not spent by
		// this run: remember the children, but leave the feature armed. That
		// also covers a session that reported failed — the role is backed off
		// five poll intervals, so re-presenting costs at most one extra run,
		// while losing the report costs the feature its only presentation.
		s.recordFeatureProgress(snap, parents, nil, parentsComplete)
		return err
	}
	s.recordFeatureProgress(snap, parents, complete, parentsComplete)
	return nil
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
	// A message addressed to qa is a direction, not something QA chose to do:
	// qa_interval bounds the runs QA starts by itself, not the ones a person
	// or the product manager asks for.
	if s.hasUnreadMail(config.RoleQA, 0, 0) {
		return true
	}
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
	if s.op("list-prs", err, "list merged PRs", "err", err) {
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
	inbox, err := s.inbox(config.RoleQA, 0, 0)
	if err != nil {
		return err
	}
	return s.runSingleton(ctx, config.RoleQA, prompts.Data{MergedPRs: merged, Issues: bugs, LastRun: last, Inbox: inbox})
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
	err = s.mail.MarkRead(data.Inbox...)
	s.op("mail", err, "mark mail read", "role", role, "err", err)
	if err := s.markRun(role, started); err != nil {
		return err
	}
	status, note := outcomeOf(res)
	s.log.Info("singleton finished", "role", role, "outcome", status, "note", oneLine(note, 200))
	if status == OutcomeFailed {
		return errors.New(s.sessionFailure(role, res, status, note))
	}
	return nil
}

// markRun records that a singleton role ran. It is a read-modify-write:
// the same file carries the role's session counters, which the session that
// just finished has already updated.
func (s *Scheduler) markRun(role string, started time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.store.Role(role)
	if err != nil {
		return err
	}
	rs.LastRun = started
	return s.store.SaveRole(role, rs)
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
		if role == config.RoleReviewer {
			// Force the worker into the review stage. `bees exec reviewer` is
			// an instruction, not a resumption, so the stage the last worker
			// recorded is forgotten as well as the label rewritten: otherwise
			// an issue whose worker stopped in develop or checks would resume
			// there and run anything but a review.
			if s.stateOf(i.Labels) != "review" {
				if err := s.setState(ctx, issue, s.labels.Review); err != nil {
					return err
				}
				i.Labels = relabel(i.Labels, s.stateOf(i.Labels), s.labels.Review)
			}
			if bk, err := s.store.Issue(issue); err == nil && bk.WorkerStage != "" {
				bk.WorkerStage, bk.AfterDevelop, bk.PreReviewDone = "", "", false
				_ = s.store.SaveIssue(bk)
			}
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
		err = s.reconcile(ctx, snap)
		s.op("reconcile", err, "reconcile", "err", capErrors(err))
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
