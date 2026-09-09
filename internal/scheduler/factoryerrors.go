package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/kpenfound/busybees/internal/duplicates"
	"github.com/kpenfound/busybees/internal/feedback"
	"github.com/kpenfound/busybees/internal/github"
)

// upstreamRepo is the busybees project itself, where a factory error belongs
// however configured a factory is: the drafts describe a bug in bees, not in
// the product this factory is building, so this is deliberately not
// project.repo.
const upstreamRepo = "kpenfound/busybees"

// filedByABee ends every body this loop writes, so a person reading the issue
// or the comment knows no human typed it. It names no factory: the draft above
// it was scrubbed of anything that identifies one (see package feedback).
const filedByABee = "Filed automatically by a bee factory's feedback loop."

// drainFeedbackQueue files the factory-error drafts a session recorded with
// report_factory_error (package feedback) against the busybees repository,
// and removes each one it filed.
//
// A draft is checked against the issues already there (internal/duplicates)
// before anything is written: the best match gets a comment saying the
// problem was seen again, and only a draft that matches nothing opens an
// issue. The issue is filed with no label and no assignee, so it stays
// outside the factory's own visibility filter even when the factory is
// building busybees itself: an automatic report is for a person to read and
// label, not something to triage into a work item.
//
// It runs on a full pass only, and only with scheduler.report_factory_errors
// on — the same key that lets a session record a draft, so switching the
// feature off stops the filing too and leaves the queue where it is. A draft
// whose GitHub call fails stays queued for the next pass, and the drafts
// after it are still filed: one broken report must not hold up the rest.
func (s *Scheduler) drainFeedbackQueue(ctx context.Context) {
	if !s.cfg.Scheduler.ReportFactoryErrors {
		return
	}
	q := feedback.Open(s.store.FeedbackDir())
	drafts, err := q.List()
	if s.op("feedback-queue", err, "could not read the factory-error queue", "err", err) {
		return
	}
	var errs []error
	for _, d := range drafts {
		if err := s.fileDraft(ctx, d); err != nil {
			errs = append(errs, fmt.Errorf("draft %s: %w", d.ID, err))
			continue
		}
		if err := q.Remove(d.ID); err != nil {
			errs = append(errs, fmt.Errorf("draft %s: %w", d.ID, err))
		}
	}
	joined := errors.Join(errs...)
	s.op("feedback-file", joined, "could not file a factory-error report", "err", capErrors(joined))
}

// fileDraft files one draft: a comment on the issue it duplicates, or a new
// issue when it duplicates none.
func (s *Scheduler) fileDraft(ctx context.Context, d feedback.Draft) error {
	matches, err := duplicates.Find(ctx, s.upstream, d.Title, d.Detail)
	if err != nil {
		return err
	}
	if len(matches) > 0 {
		// The best match whatever its state: a closed issue is where the
		// problem was already discussed, and a person reading the comment
		// can reopen it.
		m := matches[0]
		if err := s.upstream.Comment(ctx, m.Number, seenAgainBody(d)); err != nil {
			return err
		}
		s.log.Info("factory error reported on an existing issue", "repo", upstreamRepo,
			"issue", m.Number, "role", d.Role, "title", d.Title)
		return nil
	}
	number, err := s.upstream.CreateIssue(ctx, github.NewIssue{Title: d.Title, Body: newIssueBody(d)})
	if err != nil {
		return err
	}
	s.log.Info("factory error filed", "repo", upstreamRepo, "issue", number, "role", d.Role, "title", d.Title)
	return nil
}

// newIssueBody is the draft as an issue: what the role wrote, and the line
// saying who wrote it. The draft's session directory stays behind — it is a
// path on this machine, and it means nothing upstream.
func newIssueBody(d feedback.Draft) string {
	return d.Detail + "\n\n" + filedByABee
}

// seenAgainBody is the draft as a comment on the issue it duplicates: one
// line saying that it happened again, and the detail as the data point.
func seenAgainBody(d feedback.Draft) string {
	return "Also seen:\n\n" + d.Detail + "\n\n" + filedByABee
}
