package scheduler

import (
	"context"

	"github.com/kpenfound/busybees/internal/github"
)

// Work-item dependencies.
//
// An issue body may declare prerequisites with a "Blocked by #N" line (see
// github.Blockers). The scheduler holds a ready issue back while any of its
// blockers is still open — "open" meaning present in the last snapshot, so a
// closed issue, or one outside the factory's filter, blocks nothing. No label
// changes: the issue stays ready and becomes dispatchable on the first poll
// after its blocker closes.
//
// Under scheduler.stacked_prs a blocker that is a sub-issue of the same
// feature and already has an open pull request holds nothing back: the issue
// is built on that branch instead of the default branch (stackPredecessor),
// so it is dispatched as soon as the predecessor's pull request exists.

// waitingOn returns the blockers issue declares that are still open. It is
// pure so it can be tested without a scheduler. Self-references are ignored;
// so are blockers that are not in open.
func waitingOn(issue github.Issue, open map[int]bool) []int {
	var out []int
	for _, n := range github.Blockers(issue.Body) {
		if n == issue.Number || !open[n] {
			continue
		}
		out = append(out, n)
	}
	return out
}

// blockerCycle reports whether the blocker graph reachable from start leads
// back to start. Issues in a cycle would hold each other back forever, so the
// scheduler ignores their declared dependencies and dispatches them.
func blockerCycle(start int, byNumber map[int]github.Issue) bool {
	seen := map[int]bool{}
	var stack []int
	for _, n := range github.Blockers(byNumber[start].Body) {
		if n != start { // a pure self-reference is not a cycle, just noise
			stack = append(stack, n)
		}
	}
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n == start {
			return true
		}
		if seen[n] {
			continue
		}
		seen[n] = true
		if i, ok := byNumber[n]; ok {
			stack = append(stack, github.Blockers(i.Body)...)
		}
	}
	return false
}

// fillWaiting computes snap.waiting for every ready issue. snap.prByBranch
// must be filled first: with stacking on, a blocker with an open pull request
// under the same feature is not waited for.
func (s *Scheduler) fillWaiting(ctx context.Context, snap *snapshot, byNumber map[int]github.Issue) {
	for _, i := range snap.byState["ready"] {
		if blockerCycle(i.Number, byNumber) {
			s.warnCycle(i.Number)
			continue
		}
		for _, n := range github.Blockers(i.Body) {
			// Closed blockers are the normal case, not a problem.
			if n != i.Number && !snap.open[n] {
				s.log.Debug("blocker is not open; not holding the issue", "issue", i.Number, "blocker", n)
			}
		}
		w := waitingOn(i, snap.open)
		if len(w) > 0 && s.cfg.Scheduler.StackedPRs {
			w = s.notStacked(ctx, snap, i, w)
		}
		if len(w) > 0 {
			snap.waiting[i.Number] = w
		}
	}
}

// notStacked returns the open blockers of issue that still hold it back
// under scheduler.stacked_prs: the ones the issue cannot be stacked on
// because they have no open pull request yet, or belong to another feature
// (or to none). The parent lookups are made only once a blocker has a pull
// request, and only for issues held back at all, so a factory with nothing
// to stack pays for none.
func (s *Scheduler) notStacked(ctx context.Context, snap *snapshot, issue github.Issue, blockers []int) []int {
	var parent *github.Parent
	looked := false
	var out []int
	for _, n := range blockers {
		if _, ok := snap.prByBranch[s.BranchFor(n)]; !ok {
			out = append(out, n)
			continue
		}
		if !looked {
			parent = s.parentOf(ctx, issue.Number)
			looked = true
		}
		if !s.sameFeature(ctx, parent, n) {
			out = append(out, n)
			continue
		}
		s.log.Debug("blocker has an open pull request under the same feature; stacking on it", "issue", issue.Number, "blocker", n)
	}
	return out
}

// stackPredecessor returns the blocker whose branch issue builds on under
// scheduler.stacked_prs: the first one declared (github.Blockers order)
// that hasOpenPR reports a pull request for and that is a sub-issue of the
// same feature as issue. 0 when stacking is off or no blocker qualifies,
// which is the default branch. A blocker whose pull request is closed is
// never a predecessor, whatever its issue's state: a merged branch is in
// the default branch already and may have been deleted since.
func (s *Scheduler) stackPredecessor(ctx context.Context, issue github.Issue, hasOpenPR func(n int) bool) int {
	if !s.cfg.Scheduler.StackedPRs {
		return 0
	}
	var parent *github.Parent
	looked := false
	for _, n := range github.Blockers(issue.Body) {
		if n == issue.Number || !hasOpenPR(n) {
			continue
		}
		if !looked {
			parent = s.parentOf(ctx, issue.Number)
			looked = true
		}
		if s.sameFeature(ctx, parent, n) {
			return n
		}
	}
	return 0
}

// parentOf is the feature issue number is a sub-issue of, or nil: for none,
// and for a lookup that failed, which is logged and treated as no parent —
// an issue nothing can be stacked on is built the way it is without
// stacking, from the default branch, after its blockers close.
func (s *Scheduler) parentOf(ctx context.Context, number int) *github.Parent {
	p, err := s.gh.ParentIssue(ctx, number)
	if err != nil {
		s.log.Warn("parent lookup failed; not stacking", "issue", number, "err", err)
		return nil
	}
	return p
}

// sameFeature reports whether n is a sub-issue of parent. A nil parent
// matches nothing: two issues with no feature are not under the same one.
func (s *Scheduler) sameFeature(ctx context.Context, parent *github.Parent, n int) bool {
	if parent == nil {
		return false
	}
	p := s.parentOf(ctx, n)
	return p != nil && p.Number == parent.Number
}

// warnCycle logs a dependency cycle once per issue per process.
func (s *Scheduler) warnCycle(issue int) {
	s.mu.Lock()
	warned := s.warnedCycles[issue]
	s.warnedCycles[issue] = true
	s.mu.Unlock()
	if !warned {
		s.log.Warn("dependency cycle declared; ignoring the blockers of this issue", "issue", issue)
	}
}
