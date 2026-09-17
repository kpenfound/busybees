package ops

import (
	"sync"
	"time"

	"github.com/kpenfound/busybees/core/work"
)

// OverBudget reports the strict crossing used for completed sessions and work.
// Nonpositive limits are unlimited; a cost exactly at the limit is allowed.
func OverBudget(cost, limit float64) bool { return limit > 0 && cost > limit }

// Spend totals sessions at or after since. An empty key selects all work.
// Every session counts as one, and unknown is how many of them reported no
// cost (LedgerEntry.CostUnknown): those add nothing to cost, which is only
// what the sessions that reported one cost, so a caller with unknown > 0
// has a total that is short by an amount nobody knows.
func Spend(entries []LedgerEntry, key work.Key, since time.Time) (cost float64, sessions, unknown int) {
	for _, e := range entries {
		if (key == "" || e.Work.Key == key) && !e.Time.Before(since) {
			sessions++
			if e.CostUnknown {
				unknown++
				continue
			}
			cost += e.CostUSD
		}
	}
	return
}

// BudgetSignal is a rolling-budget evaluation, independent of dispatch policy.
// Unknown is how many sessions in the window reported no cost, which Spent
// leaves out.
type BudgetSignal struct {
	Reached, Crossed, Released bool
	Spent, Resume              float64
	Unknown                    int
}

// EvaluateWindow preserves hysteresis: reach at limit, release strictly below
// the resume threshold. previouslyReached is the caller's prior evaluation.
func EvaluateWindow(entries []LedgerEntry, now time.Time, window time.Duration, limit, resumePercent float64, previouslyReached bool) BudgetSignal {
	spent, _, unknown := Spend(entries, "", now.Add(-window))
	resume := limit * resumePercent / 100
	reached := limit > 0 && spent >= limit
	if previouslyReached {
		reached = limit > 0 && spent >= resume
	}
	return BudgetSignal{Reached: reached, Crossed: reached && !previouslyReached, Released: !reached && previouslyReached, Spent: spent, Resume: resume, Unknown: unknown}
}

// Streaks counts consecutive crossings per caller-owned subject. Its zero value
// is ready. A false crossing clears only that subject's streak.
type Streaks[K comparable] struct {
	mu     sync.Mutex
	counts map[K]int
}

func (s *Streaks[K]) Record(key K, crossed bool) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !crossed {
		delete(s.counts, key)
		return 0
	}
	if s.counts == nil {
		s.counts = make(map[K]int)
	}
	s.counts[key]++
	return s.counts[key]
}
