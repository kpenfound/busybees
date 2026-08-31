package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/state"
)

// HumanSender is the mailbox sender name used for feedback that came from
// people on GitHub.
const HumanSender = "human"

// deliverHumanFeedback looks at every open factory PR for reviews and
// comments written by people since the last check, mails them to the
// developer for that PR, and — when the issue was parked in approved —
// moves it back to ready so a developer worker picks it up.
//
// Bee comments are dropped by github.Client.PRActivity: a comment is a bee's
// when its last line is the marker (github.BeeRole) or when its author is the
// login the factory acts as, and with [github] unset — humans and bees then
// share one GitHub account — the marker is the only signal. The marker rule
// is positional, so a person quoting the bee they answer still reaches the
// developer.
func (s *Scheduler) deliverHumanFeedback(ctx context.Context, snap *snapshot) error {
	var errs []string
	for _, pr := range snap.prs {
		issue, ok := snap.issueForPR(pr)
		if !ok {
			continue // not a PR the factory is driving
		}
		issueNum := issue.Number
		bk, err := s.store.Issue(issueNum)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		since := bk.HumanSeenAt
		if since.Before(pr.CreatedAt) {
			since = pr.CreatedAt
		}
		if !pr.UpdatedAt.After(since) {
			continue // nothing happened on the PR since we last looked
		}
		activity, err := s.gh.PRActivity(ctx, pr.Number, since)
		if err != nil {
			errs = append(errs, fmt.Sprintf("pr #%d activity: %v", pr.Number, err))
			continue
		}
		// Whether or not humans wrote anything, remember we looked.
		seen := pr.UpdatedAt
		if len(activity) == 0 {
			_ = s.store.SetHumanSeenAt(issueNum, seen)
			continue
		}
		if last := activity[len(activity)-1].CreatedAt; last.After(seen) {
			seen = last
		}
		m := mail.Message{
			From:    HumanSender,
			To:      config.RoleDeveloper,
			Subject: fmt.Sprintf("Feedback on PR #%d from %s", pr.Number, strings.Join(activityAuthors(activity), ", ")),
			Body:    formatActivity(s.cfg.Project.Repo, pr.Number, activity),
			Issue:   issueNum,
			PR:      pr.Number,
		}
		if _, err := s.mail.Send(m); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		// The developer's answer to this is local work: wake the loop.
		s.signal()
		if err := s.store.SetHumanSeenAt(issueNum, seen); err != nil {
			errs = append(errs, err.Error())
		}
		s.log.Info("human feedback delivered to developer", "pr", pr.Number, "issue", issueNum, "items", len(activity))

		if s.stateOf(issue.Labels) == "approved" {
			s.log.Info("approved PR received human feedback; issue back to ready", "issue", issueNum)
			if err := s.reopenApproved(ctx, snap, issue, pr); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("human feedback: %s", strings.Join(errs, "; "))
	}
	return nil
}

// inFlightStates are the issue states in which the factory is working on an
// issue, and so the states in which a person's comment on the issue itself
// has somebody to reach. An issue in ready or triage has no session to steer
// — the next one to run reads the comment out of the issue body's comment
// history — and a feature or feedback issue is the product manager's, not
// this delivery's.
var inFlightStates = []string{"in-progress", "review", "approved", "blocked"}

// deliverHumanIssueComments does for comments on an in-flight issue what
// deliverHumanFeedback does for a pull request: it collects the comments
// people wrote since the last check and mails them, as one message per issue
// from HumanSender, to whoever is in a position to act on them.
//
// The developer session already renders the issue's whole comment history in
// its prompt. What this adds is the four things that rendering cannot do:
// reach the reviewer during a review, count as the answer that unblocks a
// blocked issue (reconcile reads the mailbox for that, and runs after this in
// the same pass), wake the loop, and mark a direction as fresh and a person's
// rather than one line in a list.
//
// Who a comment reaches depends on the state:
//
//   - in-progress, approved: the developer.
//   - review: the developer, and a copy to the reviewer so the round in
//     flight takes the direction into account.
//   - blocked: whoever is waiting. A block that came out of a developer
//     session left a branch or a pull request in the bookkeeping; anything
//     else came out of triage and belongs to the project manager. Mailing the
//     developer either way would send an issue blocked on a triage question
//     to a developer as ready, unrefined.
//
// The first pass that sees an issue in a flight state with no recorded
// timestamp records the poll time and delivers nothing: a zero timestamp must
// not mean "deliver every comment this issue ever received", which on the
// first tick after an upgrade would replay the whole triage conversation.
// Nothing is lost — those comments are in the prompt's comment history.
func (s *Scheduler) deliverHumanIssueComments(ctx context.Context, snap *snapshot) error {
	var errs []string
	for _, st := range inFlightStates {
		for _, issue := range snap.byState[st] {
			n := issue.Number
			bk, err := s.store.Issue(n)
			if err != nil {
				errs = append(errs, err.Error())
				continue
			}
			if bk.IssueHumanSeenAt.IsZero() {
				// First observation: seed the clock, deliver nothing.
				if err := s.store.SetIssueHumanSeenAt(n, s.now()); err != nil {
					errs = append(errs, err.Error())
				}
				continue
			}
			if !issue.UpdatedAt.After(bk.IssueHumanSeenAt) {
				continue // nothing happened on the issue since we last looked
			}
			activity, err := s.gh.IssueActivity(ctx, n, bk.IssueHumanSeenAt)
			if err != nil {
				errs = append(errs, fmt.Sprintf("issue #%d activity: %v", n, err))
				continue
			}
			// Whether or not people wrote anything, remember we looked.
			seen := issue.UpdatedAt
			if len(activity) == 0 {
				if err := s.store.SetIssueHumanSeenAt(n, seen); err != nil {
					errs = append(errs, err.Error())
				}
				continue
			}
			if last := activity[len(activity)-1].CreatedAt; last.After(seen) {
				seen = last
			}
			m := mail.Message{
				From:    HumanSender,
				To:      s.issueCommentRecipient(st, bk),
				Subject: fmt.Sprintf("Comment on issue #%d from %s", n, strings.Join(activityAuthors(activity), ", ")),
				Body:    formatIssueComments(n, activity),
				Issue:   n,
			}
			if _, err := s.mail.Send(m); err != nil {
				errs = append(errs, err.Error())
				continue
			}
			if st == "review" {
				// The reviewer's inbox matches on either number, so the
				// issue alone would reach it; the PR is there because that
				// is what a reviewer session is about.
				copyToReviewer := m
				copyToReviewer.To, copyToReviewer.PR = config.RoleReviewer, bk.PR
				if _, err := s.mail.Send(copyToReviewer); err != nil {
					errs = append(errs, err.Error())
				}
			}
			// Whoever was mailed has local work now: wake the loop.
			s.signal()
			if err := s.store.SetIssueHumanSeenAt(n, seen); err != nil {
				errs = append(errs, err.Error())
			}
			s.log.Info("human issue comments delivered", "issue", n, "state", st, "to", m.To, "items", len(activity))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("human issue comments: %s", strings.Join(errs, "; "))
	}
	return nil
}

// issueCommentRecipient names the role a comment on an issue in state st goes
// to. Only blocked is ambiguous, and the developer worker's bookkeeping
// settles it: a branch or a pull request means a developer session asked the
// question, anything else means triage did.
func (s *Scheduler) issueCommentRecipient(st string, bk state.IssueState) string {
	if st == "blocked" && bk.Branch == "" && bk.PR == 0 {
		return config.RoleProjectManager
	}
	return config.RoleDeveloper
}

// activityAuthors lists the distinct authors of activity, in the order they
// first wrote.
func activityAuthors(activity []github.Activity) []string {
	seen := map[string]bool{}
	var names []string
	for _, a := range activity {
		if !seen[a.Author] {
			seen[a.Author] = true
			names = append(names, a.Author)
		}
	}
	return names
}

// formatIssueComments renders human comments on an issue as the body of a
// mail message, with the command the recipient needs to reply on the issue.
func formatIssueComments(issue int, activity []github.Activity) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "A person commented on issue #%d. Treat it as a direction: it outranks the issue body and the reviewer. Reply on the issue with the `comment` tool (`number: %d`), which adds the marker for you.\n", issue, issue)
	for _, a := range activity {
		sb.WriteString("\n---\n")
		fmt.Fprintf(&sb, "**Comment** by %s — %s\n\n", a.Author, a.CreatedAt.Format(time.RFC3339))
		sb.WriteString(strings.TrimSpace(a.Body))
		sb.WriteString("\n")
		if a.URL != "" {
			fmt.Fprintf(&sb, "\n%s\n", a.URL)
		}
	}
	return sb.String()
}

// reopenApproved sends an approved issue back to ready — its pull request
// needs more developer work (human feedback, a conflict with the default
// branch) — and drops bees:approved from the PR, then moves the issue
// between the snapshot's buckets so the rest of the pass sees it as ready.
// An issue a worker still owns (the auto-merge checks stage) is left alone:
// the worker's own transitions would only clobber the label.
func (s *Scheduler) reopenApproved(ctx context.Context, snap *snapshot, issue github.Issue, pr github.PR) error {
	s.mu.Lock()
	_, owned := s.owned[issue.Number]
	s.mu.Unlock()
	if owned {
		s.log.Info("approved issue is owned by a worker; leaving its labels to it", "issue", issue.Number)
		return nil
	}
	if err := s.setState(ctx, issue.Number, s.labels.Ready); err != nil {
		return err
	}
	if err := s.gh.EditLabels(ctx, pr.Number, nil, []string{s.labels.Approved}); err != nil {
		return err
	}
	issue.Labels = relabel(issue.Labels, s.labels.Approved, s.labels.Ready)
	snap.byState["approved"] = removeIssue(snap.byState["approved"], issue.Number)
	snap.byState["ready"] = append(snap.byState["ready"], issue)
	snap.byNumber[issue.Number] = issue
	// Keep the cached poll in step so a local pass can dispatch it too.
	s.cacheIssue(issue)
	return nil
}

func removeIssue(list []github.Issue, number int) []github.Issue {
	out := list[:0:0]
	for _, i := range list {
		if i.Number != number {
			out = append(out, i)
		}
	}
	return out
}

// formatActivity renders human PR activity as the body of a mail message,
// including the ids the developer needs to reply on GitHub.
func formatActivity(repo string, pr int, activity []github.Activity) string {
	var sb strings.Builder
	sb.WriteString("A person reviewed your pull request on GitHub. Address each point below and reply on GitHub so they see it (end replies with the `<!-- bees:developer -->` marker).\n")
	for _, a := range activity {
		sb.WriteString("\n---\n")
		switch a.Kind {
		case "review":
			fmt.Fprintf(&sb, "**Review** by %s — state `%s` — %s\n", a.Author, a.State, a.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(&sb, "Reply with: `gh pr comment %d -R %s --body '...'`\n\n", pr, repo)
		case "review-comment":
			loc := a.Path
			if a.Line > 0 {
				loc = fmt.Sprintf("%s:%d", a.Path, a.Line)
			}
			fmt.Fprintf(&sb, "**Inline comment** by %s on `%s` — %s\n", a.Author, loc, a.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(&sb, "Reply with: `gh api repos/%s/pulls/%d/comments/%d/replies -f body='...'`\n\n", repo, pr, a.ID)
		default:
			fmt.Fprintf(&sb, "**Comment** by %s — %s\n", a.Author, a.CreatedAt.Format(time.RFC3339))
			fmt.Fprintf(&sb, "Reply with: `gh pr comment %d -R %s --body '...'`\n\n", pr, repo)
		}
		sb.WriteString(strings.TrimSpace(a.Body))
		sb.WriteString("\n")
		if a.URL != "" {
			fmt.Fprintf(&sb, "\n%s\n", a.URL)
		}
	}
	return sb.String()
}

// adoptCreated is the visibility backstop: anything created since a session
// started that carries a factory label — the base label (bees) or
// any bees:-prefixed one (bees:bug, bees:feature, a state label) — but is
// missing part of the filter — the base label, the configured assignee, or
// (pull requests only) the configured milestone — is fixed up so it stays
// visible to the factory. It is what catches a PR that never reached
// ensureVisible because the worker crashed after `gh pr create`, or that a
// person opened by hand. A freshly created PR carries
// only the base label (that is all `gh pr create` is told to pass), and it
// earns its first bees:-prefixed label at approval, so the base label has to
// count or the backstop misses the whole pre-approval life of every PR.
//
// The gate stays: the repository is shared with people, whose unrelated
// issues and pull requests must not be assigned and milestoned by the
// factory.
// Repairing pre-existing items into a changed filter is `bees doctor --fix`.
//
// One failing item does not stop the others: each is logged and skipped.
//
// The listing is not scoped by author (ListCreatedSince): it asks for
// everything created since the session started, whoever opened it, and the
// label gate is what decides. That is what keeps a pull request a session
// opened with its own `gh pr create`, and an item a person opened by hand,
// in reach of the backstop whatever account each was opened from.
func (s *Scheduler) adoptCreated(ctx context.Context, since time.Time) {
	items, err := s.gh.ListCreatedSince(ctx, since)
	if s.op("list-created", err, "visibility backstop: list created items", "err", err) {
		return
	}
	for _, it := range items {
		if !s.isFactoryItem(it.Labels) {
			continue
		}
		s.log.Info("visibility backstop: making the item visible", "number", it.Number, "pr", it.IsPR)
		if err := s.ensureVisible(ctx, it.Number, it.IsPR, it.Labels, it.Assignees, it.MilestoneTitle()); err != nil {
			s.log.Warn("visibility backstop", "number", it.Number, "err", err)
		}
	}
}

// isFactoryItem reports whether labels mark an item as one of the factory's:
// it carries the base label, or any label in the factory's namespace (a kind
// or a state label). Both halves are needed — a PR `gh pr create` just opened
// has only the base label, and a PR the reviewer already touched may carry a
// state label a person removed the base label from.
func (s *Scheduler) isFactoryItem(labels []github.Label) bool {
	if github.HasLabel(labels, s.labels.Base) {
		return true
	}
	prefix := s.labels.Base + ":"
	for _, l := range labels {
		if strings.HasPrefix(l.Name, prefix) {
			return true
		}
	}
	return false
}
