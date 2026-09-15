package ops

import (
	"sync"
	"time"
)

// PauseUntil uses backoff for a missing or past reset and caps a future reset.
// Backoff is caller policy and is preserved even if larger than maxPause.
func PauseUntil(now, resets time.Time, backoff, maxPause time.Duration) time.Time {
	switch {
	case resets.IsZero(), resets.Before(now):
		return now.Add(backoff)
	case resets.After(now.Add(maxPause)):
		return now.Add(maxPause)
	default:
		return resets
	}
}

// CapacityPause tracks an account-wide pause episode. Its zero value is ready.
type CapacityPause struct {
	mu    sync.Mutex
	until time.Time
}

// Extend never shortens an episode and reports whether this starts a new one.
func (p *CapacityPause) Extend(now, until time.Time) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	started := !p.until.After(now)
	if until.After(p.until) {
		p.until = until
	}
	return p.until, started
}

// Check reports whether paused and whether an expired episode was released.
// Only the first check of an expired episode signals release.
func (p *CapacityPause) Check(now time.Time) (paused, released bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.until.IsZero() {
		return false, false
	}
	if now.Before(p.until) {
		return true, false
	}
	p.until = time.Time{}
	return false, true
}

func (p *CapacityPause) Until() time.Time { p.mu.Lock(); defer p.mu.Unlock(); return p.until }
