package scheduler

import (
	"context"
	"fmt"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
)

// OrchestratorSender is the mailbox sender name for messages the scheduler
// itself writes to a role, such as a request to bring a pull request up to
// date with the default branch.
const OrchestratorSender = "orchestrator"

// checkPRs looks at the merge state of every open factory PR in review or
// approved and, when the base branch has moved on in a way the
// configuration cares about (scheduler.pr_fix_conflicts for conflicts,
// scheduler.pr_keep_updated for merely being behind), hands the PR back to
// its developer: a mail from the orchestrator says what to do, and an
// approved issue returns to ready so a developer worker takes it ahead of
// new work. Each head commit is mailed about once; a push changes the head
// and, if it still conflicts, is notified again.
//
// GitHub computes mergeability asynchronously: a PR whose state is UNKNOWN
// (or missing, as with older gh versions) is left alone this poll.
func (s *Scheduler) checkPRs(ctx context.Context, snap *snapshot) error {
	fix, keep := s.cfg.Scheduler.FixConflicts(), s.cfg.Scheduler.PRKeepUpdated
	if !fix && !keep {
		return nil
	}
	var errs []string
	for _, pr := range snap.prs {
		issue, ok := snap.issueForPR(pr)
		if !ok {
			continue // not a PR the factory is driving
		}
		st := s.stateOf(issue.Labels)
		if st != "review" && st != "approved" {
			continue // in progress: the developer is on it already
		}
		reason := ""
		switch {
		case pr.Conflicting() && fix:
			reason = "conflicts with"
		case pr.Behind() && keep:
			reason = "is behind"
		default:
			continue
		}
		if pr.HeadSHA == "" {
			continue // cannot tell one head from the next; nothing to remember
		}
		bk, err := s.store.Issue(issue.Number)
		if err != nil {
			errs = append(errs, err.Error())
			continue
		}
		if bk.ConflictNotifiedSHA == pr.HeadSHA {
			continue // already told the developer about this head
		}
		base := s.cfg.Project.DefaultBranch
		m := mail.Message{
			From:    OrchestratorSender,
			To:      config.RoleDeveloper,
			Subject: fmt.Sprintf("PR #%d %s %s", pr.Number, reason, base),
			Body:    updateBranchBody(pr, s.cfg.Project.Remote, base, reason),
			Issue:   issue.Number,
			PR:      pr.Number,
		}
		if _, err := s.mail.Send(m); err != nil {
			errs = append(errs, err.Error())
			continue
		}
		bk.ConflictNotifiedSHA = pr.HeadSHA
		if err := s.store.SaveIssue(bk); err != nil {
			errs = append(errs, err.Error())
		}
		s.log.Info("pull request needs updating; developer notified", "pr", pr.Number, "issue", issue.Number, "reason", reason, "head", short(pr.HeadSHA))
		if st == "approved" {
			if err := s.reopenApproved(ctx, snap, issue, pr); err != nil {
				errs = append(errs, err.Error())
			}
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("check PRs: %s", strings.Join(errs, "; "))
	}
	return nil
}

// updateBranchBody is the mail the developer receives when its pull
// request has fallen behind the default branch. remote is
// project.remote, the remote the worktree was cut from, and reason is
// "conflicts with" or "is behind".
func updateBranchBody(pr github.PR, remote, base, reason string) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Pull request #%d (branch `%s`) %s `%s`", pr.Number, pr.HeadRefName, reason, base)
	if reason == "conflicts with" {
		sb.WriteString(" and cannot be merged as it stands.\n\n")
	} else {
		sb.WriteString("; bring it up to date so it is tested against what will actually be merged.\n\n")
	}
	fmt.Fprintf(&sb, "Merge `%s` into your branch (`git fetch %s && git merge %s/%s`; rebase only if the repository asks for it)", base, remote, remote, base)
	if reason == "conflicts with" {
		sb.WriteString(", resolve the conflicts, ")
	} else {
		sb.WriteString(", ")
	}
	sb.WriteString("run the tests, push, and report `pr-updated`. Keep the change limited to the update: this is not a review round.\n")
	if pr.URL != "" {
		fmt.Fprintf(&sb, "\n%s\n", pr.URL)
	}
	return sb.String()
}

// short abbreviates a commit SHA for log lines.
func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
