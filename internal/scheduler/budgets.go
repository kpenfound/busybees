package scheduler

import (
	"fmt"
	"time"

	"github.com/kpenfound/busybees/core/ops"
	"github.com/kpenfound/busybees/core/work"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/text"
)

// Cost budgets. All three are spent against the session ledger, are off by
// default (0 = unlimited) and are enforced at the only two moments the
// factory can act on them: between the stages of a developer worker, and at
// dispatch. A running session is never interrupted on cost — a headless
// agent cannot be stopped that way — so the per-session budget is a post-hoc
// check.
const (
	// dayWindow is the rolling window scheduler.max_cost_per_day covers.
	dayWindow = 24 * time.Hour
	// overBudgetEscalateAfter is how many consecutive over-budget sessions
	// for the same work item are needed before it goes to a human instead of
	// being retried. Two in a row is not bad luck: it says the role's
	// max_turns or timeout are the wrong shape for this work.
	overBudgetEscalateAfter = 2
)

// recordWorkCost adds a finished session to its work item's running total,
// including PR-only requested reviews. It is called for every session, from
// record, so retries and reviewer sessions count like any other.
func (s *Scheduler) recordWorkCost(ref work.Ref, cost float64) {
	if ref.Key == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.store.AddWorkCost(ref, cost); err != nil {
		s.log.Warn("could not record what the session cost the work", "work", ref.Key, "err", err)
	}
}

// issueSpend returns what an issue has cost so far and over how many
// sessions. The stored total is authoritative; an issue that has none (its
// bookkeeping was written before budgets existed, or deleted) is seeded from
// the ledger once, which is also what makes the total survive a state file
// that was thrown away but not the ledger, as far as the ledger still reaches
// back: trimLedger keeps only max(scheduler.retention_period, 24h) of it.
func (s *Scheduler) issueSpend(issue int) (float64, int) { return s.workSpend(ghwork.New(issue, 0)) }

func (s *Scheduler) workSpend(ref work.Ref) (float64, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	is, err := s.store.Work(ref)
	if err != nil {
		s.log.Warn("could not read what the issue has cost", "work", ref.Key, "err", err)
		return 0, 0
	}
	if is.Sessions > 0 || is.Cost > 0 {
		return is.Cost, is.Sessions
	}
	entries, err := s.store.ReadLedger(time.Time{})
	if err != nil {
		s.log.Warn("could not read the ledger", "work", ref.Key, "err", err)
		return 0, 0
	}
	cost, sessions := ops.Spend(entries, ref.Key, time.Time{})
	if sessions == 0 {
		return 0, 0
	}
	if _, err := s.store.SetWorkCost(ref, cost, sessions); err != nil {
		s.log.Warn("could not seed what the issue has cost", "work", ref.Key, "err", err)
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
	if !ops.OverBudget(cost, budget) {
		return "", false
	}
	return fmt.Sprintf("Issue #%d has cost $%.2f across %s, over the `max_cost_per_issue` budget of $%.2f. Raise the budget or take it from here.",
		issue, cost, text.Count(sessions, "session"), budget), true
}

// checkDayBudget sums the ledger over the last 24 hours and decides whether
// dispatch is paused. It runs once per pass, before anything is dispatched,
// and logs only when the answer changes: a paused factory keeps polling, and
// a line per poll would drown everything else. The two thresholds differ —
// it pauses at scheduler.max_cost_per_day and resumes under
// scheduler.max_cost_per_day_resume_percent of it — so the factory backs off
// instead of oscillating on the edge of the budget.
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
	s.mu.Lock()
	signal := ops.EvaluateWindow(entries, now, dayWindow, budget, s.cfg.Scheduler.MaxCostPerDayResumePercent, s.dayPaused)
	spent, resume, paused := signal.Spent, signal.Resume, signal.Reached
	s.dayPaused, s.daySpend = paused, spent
	s.mu.Unlock()
	switch {
	case signal.Crossed:
		s.log.Warn(fmt.Sprintf("⏸ daily cost budget reached ($%.2f of $%.2f in the last 24h); starting no new sessions", spent, budget),
			logging.SummaryKey, true, "cost_usd", spent, "max_cost_per_day", budget)
	case signal.Released:
		// The threshold is named because it is what the pause was waiting
		// for: without it a pause that lasted while the window sat between
		// the two numbers reads as arbitrarily long in bees.log.
		s.log.Info(fmt.Sprintf("▶ daily cost budget released ($%.2f, back under the $%.2f resume threshold of $%.2f in the last 24h); dispatching again", spent, resume, budget),
			logging.SummaryKey, true, "cost_usd", spent, "max_cost_per_day", budget, "resume_threshold_usd", resume)
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
	if !ops.OverBudget(res.CostUSD, budget) {
		return "", false
	}
	return fmt.Sprintf("the session cost $%.2f, over the `max_cost_per_session` budget of $%.2f", res.CostUSD, budget), true
}

// overBudgetStreak counts consecutive over-budget sessions for one work item
// (or, for the singleton roles, for the role). A session within budget clears
// the streak.
type budgetSubject struct {
	Work work.Key
	Role string
}

func (s *Scheduler) overBudgetStreak(key budgetSubject, over bool) int {
	return s.overBudget.Record(key, over)
}

// budgetKey is what an over-budget streak is counted against.
func budgetKey(spec sessionSpec) budgetSubject {
	if ref := sessionWork(spec); ref.Key != "" {
		return budgetSubject{Work: ref.Key}
	}
	return budgetSubject{Role: spec.role}
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
