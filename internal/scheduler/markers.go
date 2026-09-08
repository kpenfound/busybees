package scheduler

import (
	"context"
	"time"

	"github.com/kpenfound/busybees/internal/github"
)

// commentTarget is one issue or pull request a finished session may have
// commented on, and which of the two it is, so the warning names it the way
// a person reading the log would.
type commentTarget struct {
	number int
	pr     bool
}

// auditMarkers checks, after a session has ended, that every comment the
// factory posted while that session ran carries a role's marker.
//
// The marker is what tells a bee's comment from a person's on the account
// they share, and the comment tool appends it for the calling role, so a
// comment posted through the tool always has one. What the tool cannot reach
// is a comment a session posts from its own shell with `gh` — `gh issue
// close --comment`, say — where the marker is only there if the session
// wrote it. That is the one path where a marker can be missing, and it lies
// outside the scheduler and the MCP server both: there is nothing to
// intercept while it happens, so this looks afterwards instead.
//
// It detects and logs; it does not rewrite the comment. Editing a bee's
// comment behind its back is a bigger surprise than the missing marker is a
// problem: the login half of the rule (github.IsBee) still identifies the
// comment as the factory's, so nothing downstream mistakes it for a
// person's, and the warning is enough to find the prompt or the session that
// wrote it.
//
// A shared account is exactly the configuration where the login half is
// missing, and there a comment without a marker is indistinguishable from a
// person's — so with no [github] login there is nothing to check, and this
// costs nothing rather than guessing.
//
// The one warning it raises that is nobody's bug: a person stopping a
// session from the live view escalates the issue while the session is still
// running, and the orchestrator's escalation comment deliberately carries no
// marker.
func (s *Scheduler) auditMarkers(ctx context.Context, spec sessionSpec, touched []int, since time.Time) {
	if s.gh.ActsAs == "" {
		return
	}
	for _, t := range commentTargets(spec, touched) {
		comments, err := s.gh.CommentsSince(ctx, t.number, since)
		if s.op("marker-audit", err, "could not check the comments a session left", "item", t.number, "err", err) {
			continue
		}
		for _, c := range comments {
			if !github.IsBee(s.gh.ActsAs, c.Author, c.Body) {
				continue // a person's comment carries no marker, and needs none
			}
			if _, ok := github.BeeRole(c.Body); ok {
				continue
			}
			key := "issue"
			if t.pr {
				key = "pr"
			}
			s.log.Warn("a comment the factory posted is missing its bees marker",
				key, t.number, "role", spec.role, "session", spec.name,
				"comment", c.ID, "url", c.URL)
		}
	}
}

// commentTargets is where a session could have commented: the issue and the
// pull request it was given, and every issue it changed through the MCP
// server (the touched list refreshTouched has just read for the cache). The
// tools that record an issue as touched are the ones that create or relabel
// it, not the comment tool, so the session's own work item is the half of
// this that matters: it is what a session comments on with `gh`.
//
// One `gh` call each, once per session — the same budget refreshTouched
// spends, and nothing at all for a singleton session with no work item that
// touched no issue.
func commentTargets(spec sessionSpec, touched []int) []commentTarget {
	var out []commentTarget
	seen := map[int]bool{}
	add := func(n int, pr bool) {
		if n <= 0 || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, commentTarget{number: n, pr: pr})
	}
	if spec.data.Issue != nil {
		add(spec.data.Issue.Number, false)
	}
	if spec.data.PR != nil {
		add(spec.data.PR.Number, true)
	}
	for _, n := range touched {
		add(n, false)
	}
	return out
}
