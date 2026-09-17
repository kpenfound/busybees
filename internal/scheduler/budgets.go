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
// back: trimLedger keeps only max(scheduler.retention_period, 24h) of it. A
// session that reported no cost counts as a session and adds nothing to the
// total. The error is the work's bookkeeping, or the ledger when the seed
// needed it, that could not be read, and it is the caller's to fail closed
// on.
func (s *Scheduler) issueSpend(issue int) (float64, int, error) {
	return s.workSpend(ghwork.New(issue, 0))
}

func (s *Scheduler) workSpend(ref work.Ref) (float64, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	is, err := s.store.Work(ref)
	if err != nil {
		return 0, 0, fmt.Errorf("read what the work has cost: %w", err)
	}
	if is.Sessions > 0 || is.Cost > 0 {
		return is.Cost, is.Sessions, nil
	}
	entries, err := s.store.ReadLedger(time.Time{})
	if err != nil {
		return 0, 0, fmt.Errorf("read the ledger: %w", err)
	}
	cost, sessions, _ := ops.Spend(entries, ref.Key, time.Time{})
	if sessions == 0 {
		return 0, 0, nil
	}
	if _, err := s.store.SetWorkCost(ref, cost, sessions); err != nil {
		s.log.Warn("could not seed what the issue has cost", "work", ref.Key, "err", err)
	}
	return cost, sessions, nil
}

// overIssueBudget reports whether an issue has passed
// scheduler.max_cost_per_issue, and the escalation text naming the spend.
// The developer worker calls it between stages, so the session that took the
// issue over its budget has finished and its work is on the branch. A spend
// that cannot be read, from the bookkeeping or the ledger it is seeded
// from, is over budget too: the budget cannot be enforced against a total
// nobody can vouch for, so the worker stops and the text says what could
// not be read.
func (s *Scheduler) overIssueBudget(issue int) (string, bool) {
	budget := s.cfg.Scheduler.MaxCostPerIssue
	if budget <= 0 {
		return "", false
	}
	cost, sessions, err := s.issueSpend(issue)
	if err != nil {
		return fmt.Sprintf("Issue #%d cannot be checked against the `max_cost_per_issue` budget of $%.2f: %v. Repair what could not be read or take it from here.",
			issue, budget, err), true
	}
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
//
// The ledger is read fail-closed. When it cannot be read the pass dispatches
// nothing the budget gates, as if the budget were reached, and says so once;
// the previous sum stays in the status file, stale, until a read succeeds.
// A session that reported no cost counts as a session and adds nothing to
// the sum, and the sum is reported with how many of those it leaves out.
func (s *Scheduler) checkDayBudget() {
	budget := s.cfg.Scheduler.MaxCostPerDay
	if budget <= 0 {
		return
	}
	now := s.now()
	entries, err := s.store.ReadLedger(now.Add(-dayWindow))
	s.track("ledger-read", err)
	s.mu.Lock()
	wasUnreadable := s.ledgerErr != nil
	s.ledgerErr = err
	s.mu.Unlock()
	if err != nil {
		if !wasUnreadable {
			s.log.Warn(fmt.Sprintf("⏸ the ledger cannot be read for the daily cost budget (%v); starting no new sessions", err),
				logging.SummaryKey, true, "err", err)
		}
		return
	}
	if wasUnreadable {
		s.log.Info("▶ the ledger reads again; the daily cost budget decides dispatch", logging.SummaryKey, true)
	}
	s.mu.Lock()
	signal := ops.EvaluateWindow(entries, now, dayWindow, budget, s.cfg.Scheduler.MaxCostPerDayResumePercent, s.dayPaused)
	spent, resume, paused := signal.Spent, signal.Resume, signal.Reached
	hadUnknown := s.dayUnknown > 0
	s.dayPaused, s.daySpend, s.dayUnknown = paused, spent, signal.Unknown
	s.mu.Unlock()
	unknown := ""
	if signal.Unknown > 0 {
		unknown = fmt.Sprintf("; %s of unknown cost not counted", text.Count(signal.Unknown, "session"))
	}
	switch {
	case signal.Crossed:
		s.log.Warn(fmt.Sprintf("⏸ daily cost budget reached ($%.2f of $%.2f in the last 24h)%s; starting no new sessions", spent, budget, unknown),
			logging.SummaryKey, true, "cost_usd", spent, "max_cost_per_day", budget, "cost_unknown_sessions", signal.Unknown)
	case signal.Released:
		// The threshold is named because it is what the pause was waiting
		// for: without it a pause that lasted while the window sat between
		// the two numbers reads as arbitrarily long in bees.log.
		s.log.Info(fmt.Sprintf("▶ daily cost budget released ($%.2f, back under the $%.2f resume threshold of $%.2f in the last 24h)%s; dispatching again", spent, resume, budget, unknown),
			logging.SummaryKey, true, "cost_usd", spent, "max_cost_per_day", budget, "resume_threshold_usd", resume, "cost_unknown_sessions", signal.Unknown)
	case signal.Unknown > 0 && !hadUnknown:
		// Said once when the window first holds one, like the pause: the
		// count is in status.json for as long as it lasts.
		s.log.Warn(fmt.Sprintf("%s in the last 24h reported no cost; the daily cost budget counts them as $0.00 ($%.2f of $%.2f)", text.Count(signal.Unknown, "session"), spent, budget),
			logging.SummaryKey, true, "cost_usd", spent, "max_cost_per_day", budget, "cost_unknown_sessions", signal.Unknown)
	}
}

// dayBudgetReached reports whether dispatch is paused by the daily budget,
// or by a ledger that could not be read for it.
func (s *Scheduler) dayBudgetReached() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dayPaused || s.ledgerErr != nil
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
	st.DayUnknownSessions = s.dayUnknown
	if s.ledgerErr != nil {
		st.LedgerError = oneLine(s.ledgerErr.Error(), escalationNoteLimit)
	}
}
