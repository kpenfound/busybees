package scheduler

import (
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/text"
)

// trimLedger removes the ledger lines older than scheduler.retention_period.
// It runs once per full pass, before checkDayBudget, and never trims inside
// dayWindow: a retention_period under 24h would otherwise take away sessions
// max_cost_per_day still counts.
func (s *Scheduler) trimLedger() {
	keep := config.DefaultRetentionPeriod
	if d := s.cfg.Scheduler.RetentionPeriod; d != nil {
		keep = d.Duration
	}
	keep = max(keep, dayWindow)
	removed, err := s.store.TrimLedger(s.now().Add(-keep))
	s.op("ledger-trim", err, "could not trim the ledger", "err", err)
	if removed > 0 {
		s.log.Debug("trimmed the ledger", "removed", text.Count(removed, "line"), "older_than", keep.String())
	}
}
