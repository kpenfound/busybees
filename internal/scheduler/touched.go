package scheduler

import (
	"context"
	"strings"

	"github.com/kpenfound/busybees/internal/session"
)

// refreshTouched brings the cached issue list back in step with what a
// session that has just ended changed on GitHub.
//
// The rule everywhere else in the scheduler is that a write keeps the cache
// in step (reconcile's cacheIssue calls). A session's writes go through the
// MCP server, which runs in its own process and cannot reach that cache, so
// the session records each issue it creates or relabels in its own directory
// (session.RecordTouched) and this reads the list back. Without it the wake
// a finished session signals runs a local pass that classifies the issue from
// its stale labels and finds nothing to do: a triaged issue waits a whole
// poll interval — an hour outside work_hours — before a developer starts on
// it, and a feature breakdown costs two polls.
//
// The cost is one `gh issue view` per issue the session actually touched,
// and nothing at all when it touched none. That is the budget liveCandidate
// already spends: a single-issue read outside a poll, never a list call, so
// the polling cadence stays exactly what poll_interval says.
//
// An issue that has been closed, or that does not match the factory's
// filter, is dropped rather than cached: the cached list is what the poll
// would have returned, and the poll returns neither.
func (s *Scheduler) refreshTouched(ctx context.Context, sessionDir string) {
	touched, err := session.TouchedIssues(sessionDir)
	if s.op("touched-issues", err, "reading the issues a session changed", "session", sessionDir, "err", err) {
		return
	}
	for _, n := range touched {
		live, err := s.gh.GetIssue(ctx, n)
		if s.op("issue-get", err, "refreshing an issue a session changed", "issue", n, "err", err) {
			continue
		}
		if live.State != "" && !strings.EqualFold(live.State, "open") {
			continue
		}
		if !s.query.Matches(live.Labels, live.Assignees, live.MilestoneTitle()) {
			continue
		}
		s.log.Debug("refreshed an issue the session changed", "issue", n, "state", s.stateOf(live.Labels))
		s.cacheIssue(live)
	}
}
