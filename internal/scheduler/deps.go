package scheduler

import "github.com/kpenfound/busybees/internal/github"

// Work-item dependencies.
//
// An issue body may declare prerequisites with a "Blocked by #N" line (see
// github.Blockers). The scheduler holds a ready issue back while any of its
// blockers is still open — "open" meaning present in the last snapshot, so a
// closed issue, or one outside the factory's filter, blocks nothing. No label
// changes: the issue stays ready and becomes dispatchable on the first poll
// after its blocker closes.

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

// fillWaiting computes snap.waiting for every ready issue.
func (s *Scheduler) fillWaiting(snap *snapshot, byNumber map[int]github.Issue) {
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
		if w := waitingOn(i, snap.open); len(w) > 0 {
			snap.waiting[i.Number] = w
		}
	}
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
