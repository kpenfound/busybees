package scheduler

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/kpenfound/busybees/core/work"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/session"
)

// Retention. An issue that has closed is never worked on again, so the
// state it left — its bookkeeping in issues/work-<hash>.json and the directory of
// every session that worked on it — is deleted once
// scheduler.retention_period has passed since it went stale: its pull
// request merging, or the issue closing when no pull request merged.
//
// The sweep runs at the end of a full pass, at most once per
// retentionSweepInterval. It starts from the bookkeeping files rather than
// from the poll, which lists open issues only: a file whose issue the poll
// does not show is one that may have closed, and GitHub is asked when. A
// session the issue's bookkeeping still records — one running, or one a
// killed scheduler or a hard stop left unfinished — keeps the whole issue
// as it is, and so does a developer worker or a live session holding it.
// Among the issue's session directories, the ones CheckInterrupted reports
// running or interrupted are kept too.

// retentionSweepInterval is how often the sweep runs. A closed issue's state
// goes up to this much later than retention_period says, never earlier.
const retentionSweepInterval = time.Hour

// sweepRetention deletes the state of the issues that closed more than
// retention_period ago.
func (s *Scheduler) sweepRetention(ctx context.Context, snap *snapshot) {
	now := s.now()
	s.mu.Lock()
	if !s.lastSweep.IsZero() && now.Before(s.lastSweep.Add(retentionSweepInterval)) {
		s.mu.Unlock()
		return
	}
	s.lastSweep = now
	s.mu.Unlock()
	retention := config.DefaultRetentionPeriod
	if d := s.config().Scheduler.RetentionPeriod; d != nil {
		retention = d.Duration
	}
	numbers, err := s.store.IssueNumbers()
	if s.op("retention", err, "could not list the issues' bookkeeping", "err", err) {
		return
	}
	var index *sessionIndex
	var errs []error
	for _, n := range numbers {
		if snap.open[n] || s.holds(n) {
			continue
		}
		if _, ok := snap.prByNumber[n]; ok {
			continue // an open pull request a person asked the reviewer about
		}
		bk, err := s.store.Issue(n)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if bk.Session != nil {
			continue
		}
		stale, err := s.staleSince(ctx, n, ghwork.PR(bk.Work))
		if err != nil {
			// A pull request's number (a requested review keeps bookkeeping
			// under it) is not an issue gh can view; nor is anything while gh
			// is failing. Either way there is nothing to delete yet.
			s.log.Debug("retention: could not tell when the issue closed", "issue", n, "err", err)
			continue
		}
		if stale.IsZero() || now.Before(stale.Add(retention)) {
			continue
		}
		if index == nil {
			if index, err = indexSessions(s.store.SessionsDir()); err != nil {
				errs = append(errs, err)
				break
			}
		}
		removed, kept := 0, 0
		for _, dir := range index.of(n, ghwork.PR(bk.Work)) {
			if in, running := session.CheckInterrupted("", dir, s.alive); running || in != nil {
				kept++
				continue
			}
			if err := os.RemoveAll(dir); err != nil {
				errs = append(errs, err)
				continue
			}
			removed++
		}
		s.mu.Lock()
		err = s.store.RemoveIssue(n)
		delete(s.triggers, n)
		s.mu.Unlock()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.log.Info("removed the state of a closed issue", "issue", n, "closed", stale, "sessions_removed", removed, "sessions_kept", kept)
	}
	err = errors.Join(errs...)
	s.op("retention", err, "retention sweep", "err", capErrors(err))
}

// holds reports whether a developer worker or a running session is on the
// issue.
func (s *Scheduler) holds(n int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.owned[n]; ok {
		return true
	}
	for _, ls := range s.live {
		if ghwork.Issue(ls.Work) == n {
			return true
		}
	}
	return false
}

// staleSince is when an issue's state went stale: its pull request merging
// or, when none merged, the issue closing. The zero time is an issue still
// open. A closed issue's answer is remembered, so a sweep asks GitHub about
// it once.
func (s *Scheduler) staleSince(ctx context.Context, n, pr int) (time.Time, error) {
	s.mu.Lock()
	at, ok := s.triggers[n]
	s.mu.Unlock()
	if ok {
		return at, nil
	}
	at, err := s.gh.IssueClosedAt(ctx, n)
	if err != nil || at.IsZero() {
		return time.Time{}, err
	}
	if pr > 0 {
		p, err := s.gh.GetPR(ctx, pr)
		if err != nil {
			return time.Time{}, err
		}
		if p.MergedAt != nil {
			at = *p.MergedAt
		}
	}
	s.mu.Lock()
	s.triggers[n] = at
	s.mu.Unlock()
	return at, nil
}

// sessionIndex is the session directories under the sessions directory, by
// the issue they worked on and, for a directory that names a pull request
// instead, by that pull request.
type sessionIndex struct {
	byWork map[work.Key][]string
}

func indexSessions(sessionsDir string) (*sessionIndex, error) {
	idx := &sessionIndex{byWork: map[work.Key][]string{}}
	entries, err := os.ReadDir(sessionsDir)
	if errors.Is(err, os.ErrNotExist) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(sessionsDir, e.Name())
		ref, err := session.ReadWork(dir)
		if err != nil {
			return nil, err
		}
		if ref.Key != "" {
			idx.byWork[ref.Key] = append(idx.byWork[ref.Key], dir)
		}
	}
	return idx, nil
}

// of returns the session directories of an issue whose pull request is pr (0
// for none).
func (idx *sessionIndex) of(issue, pr int) []string {
	dirs := idx.byWork[ghwork.IssueKey(issue)]
	if pr > 0 {
		dirs = append(dirs[:len(dirs):len(dirs)], idx.byWork[ghwork.PRKey(pr)]...)
	}
	return dirs
}
