package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
)

// HumanSender is the mailbox sender name used for feedback that came from
// people on GitHub.
const HumanSender = "human"

// deliverHumanFeedback looks at every open factory PR for reviews and
// comments written by people since the last check, mails them to the
// developer for that PR, and — when the issue was parked in approved —
// moves it back to ready so a developer worker picks it up.
//
// Bee comments are recognised by github.BeeRole, because humans and bees
// share one GitHub account: a comment is a bee's only when its last line is
// the marker, so a person quoting the bee they answer still reaches the
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
		bk.HumanSeenAt = pr.UpdatedAt
		if len(activity) == 0 {
			_ = s.store.SaveIssue(bk)
			continue
		}
		last := activity[len(activity)-1].CreatedAt
		if last.After(bk.HumanSeenAt) {
			bk.HumanSeenAt = last
		}
		authors := map[string]bool{}
		for _, a := range activity {
			authors[a.Author] = true
		}
		var names []string
		for a := range authors {
			names = append(names, a)
		}
		m := mail.Message{
			From:    HumanSender,
			To:      config.RoleDeveloper,
			Subject: fmt.Sprintf("Feedback on PR #%d from %s", pr.Number, strings.Join(names, ", ")),
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
		if err := s.store.SaveIssue(bk); err != nil {
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

// adoptCreated is the visibility backstop: anything the account created since
// a session started that carries a factory label — the base label (bees) or
// any bees:-prefixed one (bees:bug, bees:feature, a state label) — but is
// missing part of the filter — the base label, the configured assignee, or
// (pull requests only) the configured milestone — is fixed up so it stays
// visible to the factory. It is what catches a PR that never reached
// ensureVisible because the worker crashed after `gh pr create`, or that a
// person opened by hand on the shared account. A freshly created PR carries
// only the base label (that is all `gh pr create` is told to pass), and it
// earns its first bees:-prefixed label at approval, so the base label has to
// count or the backstop misses the whole pre-approval life of every PR.
//
// The gate stays: the account is shared with people, whose unrelated issues
// and pull requests must not be assigned and milestoned by the factory.
// Repairing pre-existing items into a changed filter is `bees doctor --fix`.
//
// One failing item does not stop the others: each is logged and skipped.
//
// Known narrowing with [github] set (#263): the listing asks GitHub for the
// items the account bees acts as authored, so it sees what bees' own code
// created (already born matching the filter, since issues.Create applies it)
// and not the pull requests a session opened with its own gh. The main path -
// ensureVisible in the developer worker - is unaffected; it is this backstop
// that narrows.
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
