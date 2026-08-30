package scheduler

import (
	"fmt"
	"time"

	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/text"
)

// Cost budgets. All three are spent against the session ledger, are off by
// default (0 = unlimited) and are enforced at the only two moments the
// factory can act on them: between the stages of a developer worker, and at
// dispatch. A running session is never interrupted on cost — `claude -p`
// cannot be stopped that way — so the per-session budget is a post-hoc check.
const (
	// dayWindow is the rolling window scheduler.max_cost_per_day covers.
	dayWindow = 24 * time.Hour
	// overBudgetEscalateAfter is how many consecutive over-budget sessions
	// for the same work item are needed before it goes to a human instead of
	// being retried. Two in a row is not bad luck: it says the role's
	// max_turns or timeout are the wrong shape for this work.
	overBudgetEscalateAfter = 2
)

// recordIssueCost adds a finished session to the running total of the issue
// it was run for. It is called for every session, from record, so retries and
// reviewer sessions count like any other.
func (s *Scheduler) recordIssueCost(issue int, cost float64) {
	if issue == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.store.AddIssueCost(issue, cost); err != nil {
		s.log.Warn("could not record what the session cost the issue", "issue", issue, "err", err)
	}
}

// issueSpend returns what an issue has cost so far and over how many
// sessions. The stored total is authoritative; an issue that has none (its
// bookkeeping was written before budgets existed, or deleted) is seeded from
// the ledger once, which is also what makes the total survive a state file
// that was thrown away but not the ledger.
func (s *Scheduler) issueSpend(issue int) (float64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	is, err := s.store.Issue(issue)
	if err != nil {
		s.log.Warn("could not read what the issue has cost", "issue", issue, "err", err)
		return 0, 0
	}
	if is.Sessions > 0 || is.Cost > 0 {
		return is.Cost, is.Sessions
	}
	entries, err := s.store.ReadLedger(time.Time{})
	if err != nil {
		s.log.Warn("could not read the ledger", "issue", issue, "err", err)
		return 0, 0
	}
	var cost float64
	var sessions int
	for _, e := range entries {
		if e.Issue == issue {
			cost += e.CostUSD
			sessions++
		}
	}
	if sessions == 0 {
		return 0, 0
	}
	if _, err := s.store.SetIssueCost(issue, cost, sessions); err != nil {
		s.log.Warn("could not seed what the issue has cost", "issue", issue, "err", err)
	}
	return cost, sessions
}

// overIssueBudget reports whether an issue has passed
// scheduler.max_cost_per_issue, and the escalation text naming the spend.
// The developer worker calls it between stages, so the session that took the
// issue over its budget has finished and its work is on the branch.
func (s *Scheduler) overIssueBudget(issue int) (string, bool) {
	budget := s.cfg.Scheduler.MaxCostPerIssue
	if budget <= 0 {
		return "", false
	}
	cost, sessions := s.issueSpend(issue)
	if cost <= budget {
		return "", false
	}
	return fmt.Sprintf("Issue #%d has cost $%.2f across %s, over the `max_cost_per_issue` budget of $%.2f. Raise the budget or take it from here.",
		issue, cost, text.Count(sessions, "session"), budget), true
}

// checkDayBudget sums the ledger over the last 24 hours and decides whether
// dispatch is paused. It runs once per pass, before anything is dispatched,
// and logs only when the answer changes: a paused factory keeps polling, and
// a line per poll would drown everything else.
func (s *Scheduler) checkDayBudget() {
	budget := s.cfg.Scheduler.MaxCostPerDay
	if budget <= 0 {
		return
	}
	now := s.now()
	entries, err := s.store.ReadLedger(now.Add(-dayWindow))
	if err != nil {
		// Accounting must never stop the factory: an unreadable ledger
		// leaves the previous answer in force.
		s.log.Warn("could not read the ledger for the daily budget", "err", err)
		return
	}
	var spent float64
	for _, e := range entries {
		spent += e.CostUSD
	}
	paused := spent >= budget
	s.mu.Lock()
	was := s.dayPaused
	s.dayPaused, s.daySpend = paused, spent
	s.mu.Unlock()
	switch {
	case paused && !was:
		s.log.Warn(fmt.Sprintf("⏸ daily cost budget reached ($%.2f of $%.2f in the last 24h); starting no new sessions", spent, budget),
			logging.SummaryKey, true, "cost_usd", spent, "max_cost_per_day", budget)
	case was && !paused:
		s.log.Info(fmt.Sprintf("▶ daily cost budget no longer reached ($%.2f of $%.2f in the last 24h); dispatching again", spent, budget),
			logging.SummaryKey, true, "cost_usd", spent, "max_cost_per_day", budget)
	}
}

// dayBudgetReached reports whether dispatch is paused by the daily budget.
func (s *Scheduler) dayBudgetReached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dayPaused
}

// overSessionBudget reports whether one finished session cost more than
// scheduler.max_cost_per_session, and the note that says so.
func overSessionBudget(res *session.Result, budget float64) (string, bool) {
	if budget <= 0 || res.CostUSD <= budget {
		return "", false
	}
	return fmt.Sprintf("the session cost $%.2f, over the `max_cost_per_session` budget of $%.2f", res.CostUSD, budget), true
}

// overBudgetStreak counts consecutive over-budget sessions for one work item
// (or, for the singleton roles, for the role). A session within budget clears
// the streak.
func (s *Scheduler) overBudgetStreak(key string, over bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !over {
		delete(s.overBudget, key)
		return 0
	}
	s.overBudget[key]++
	return s.overBudget[key]
}

// budgetKey is what an over-budget streak is counted against.
func budgetKey(spec sessionSpec) string {
	if spec.data.Issue != nil {
		return fmt.Sprintf("issue-%d", spec.data.Issue.Number)
	}
	return "role-" + spec.role
}

// failedResult copies a session result with its outcome replaced by a
// reported failure, so every caller of outcomeOf sees the failure and the
// retry machinery treats it as behavioural (running it again would only
// spend the same money). The original result was already recorded in the
// ledger and summarised: the ledger says what the session did, this says
// what the factory made of it.
func failedResult(res *session.Result, note string) *session.Result {
	out := *res
	out.HasOutcome = true
	out.Outcome = session.Outcome{Status: OutcomeFailed, Note: note}
	return &out
}

// budgetStatus fills the cost-budget fields of the status file.
func (s *Scheduler) budgetStatus(st *state.Status) {
	st.BudgetPaused = s.dayPaused
	st.DaySpendUSD = s.daySpend
	st.DayBudgetUSD = s.cfg.Scheduler.MaxCostPerDay
}
