package scheduler

import (
	"errors"
	"fmt"
	"time"

	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// The account-wide claude session limit. It is not a per-session failure:
// every role shares one account, so a session that dies on it says nothing
// about the work it was given and everything about what the factory may do
// next. Running it again, or dispatching the next issue, walks into the
// same wall — so the whole factory stops dispatching until the limit
// resets. A session already running is never interrupted, for the same
// reason the cost budgets do not interrupt one (see budgets.go): `claude
// -p` cannot be stopped that way.
const (
	// maxLimitPause caps how long one report may stop the factory. A bad
	// clock, or a reset time belonging to the seven-day window rather than
	// the five-hour one, must not park everything for days.
	maxLimitPause = 8 * time.Hour
)

// errSessionLimited is what runSessionWithRetry returns instead of a
// failure the caller would act on. The session did not fail on its merits:
// escalating its issue to a human, or counting the round against it, would
// punish the work for the account's capacity. The worker unwinds, the issue
// keeps its state label, and it is picked up again after the pause lifts.
var errSessionLimited = errors.New("claude session limit reached")

// pauseUntil turns a reported reset time into the moment dispatch may
// resume. A reset time that is missing, or already in the past because some
// clock is wrong, is not usable and the pause falls back to
// scheduler.rate_limit_backoff — which is exactly what that key means. One
// further ahead than maxLimitPause is clamped to it.
func pauseUntil(now, resets time.Time, backoff time.Duration) time.Time {
	switch {
	case resets.IsZero(), resets.Before(now):
		return now.Add(backoff)
	case resets.After(now.Add(maxLimitPause)):
		return now.Add(maxLimitPause)
	default:
		return resets
	}
}

// recordSessionLimit pauses dispatch when a finished session died on the
// account-wide session limit, and reports whether it did. It is called for
// every session, from runSessionWithRetry, before any retry is considered.
func (s *Scheduler) recordSessionLimit(res *session.Result) bool {
	resets, hit := res.SessionLimited()
	if !hit {
		return false
	}
	now := s.now()
	until := pauseUntil(now, resets, s.cfg.Scheduler.RateLimitBackoff.Duration)
	s.mu.Lock()
	// An episode already under way is extended, never shortened: two
	// sessions hitting the same limit report the same window.
	started := !s.limitPausedUntil.After(now)
	if until.After(s.limitPausedUntil) {
		s.limitPausedUntil = until
	}
	until = s.limitPausedUntil
	s.mu.Unlock()
	if started {
		s.log.Warn(fmt.Sprintf("⏸ claude session limit reached; starting no new sessions until %s", until.Local().Format("15:04 MST")),
			logging.SummaryKey, true, "until", until, "reset_reported", !resets.IsZero())
	}
	return true
}

// limitPaused reports whether dispatch is paused by the claude session
// limit. It is the predicate the dispatch gates read, and it is also what
// ends the episode: the first call after the reset time clears the pause
// and logs that it lifted, so a pause is announced exactly twice however
// many times the gates ask.
func (s *Scheduler) limitPaused() bool {
	s.mu.Lock()
	if s.limitPausedUntil.IsZero() {
		s.mu.Unlock()
		return false
	}
	if s.now().Before(s.limitPausedUntil) {
		s.mu.Unlock()
		return true
	}
	s.limitPausedUntil = time.Time{}
	s.mu.Unlock()
	s.log.Info("▶ claude session limit reset; dispatching again", logging.SummaryKey, true)
	return false
}

// limitStatus fills the session-limit field of the status file. A pause
// whose reset time has passed but that no dispatch gate has looked at yet
// is still on the scheduler; `bees status` compares it with the clock.
func (s *Scheduler) limitStatus(st *state.Status) {
	st.LimitPausedUntil = s.limitPausedUntil
}
