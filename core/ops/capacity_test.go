package ops

import (
	"testing"
	"time"
)

const maxLimitPause = 8 * time.Hour

// TestPauseUntil covers what the factory does with the reset time a session
// reported: an unusable one falls back to the caller backoff, an
// implausible one is clamped, and a reset that has just arrived is no pause
// at all.
func TestPauseUntil(t *testing.T) {
	now := time.Date(2026, 8, 30, 23, 13, 0, 0, time.UTC)
	const backoff = 15 * time.Minute
	cases := []struct {
		name   string
		resets time.Time
		want   time.Time
	}{
		{"no reset time at all", time.Time{}, now.Add(backoff)},
		{"reset in the past", now.Add(-time.Hour), now.Add(backoff)},
		{"reset exactly at now", now, now},
		{"reset ahead", now.Add(37 * time.Minute), now.Add(37 * time.Minute)},
		{"reset at the cap", now.Add(maxLimitPause), now.Add(maxLimitPause)},
		{"reset beyond the cap", now.Add(7 * 24 * time.Hour), now.Add(maxLimitPause)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PauseUntil(now, c.resets, backoff, maxLimitPause); !got.Equal(c.want) {
				t.Errorf("pauseUntil = %s, want %s", got, c.want)
			}
		})
	}
	// A pause until exactly now is one the dispatch gate never sees: the
	// predicate asks whether the clock is still before it.
	if now.Before(PauseUntil(now, now, backoff, maxLimitPause)) {
		t.Error("a reset at now must not pause dispatch")
	}
}

func TestCapacityEpisodeExtendsAndReleasesOnce(t *testing.T) {
	var pause CapacityPause
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if active, released := pause.Check(now); active || released {
		t.Fatal("zero pause is active")
	}
	until, started := pause.Extend(now, now.Add(time.Hour))
	if !started || !until.Equal(now.Add(time.Hour)) {
		t.Fatal("episode did not start")
	}
	until, started = pause.Extend(now, now.Add(time.Minute))
	if started || !until.Equal(now.Add(time.Hour)) {
		t.Fatal("episode shortened or restarted")
	}
	until, started = pause.Extend(now, now.Add(2*time.Hour))
	if started || !until.Equal(now.Add(2*time.Hour)) {
		t.Fatal("episode did not extend")
	}
	if active, released := pause.Check(until.Add(-time.Nanosecond)); !active || released {
		t.Fatal("early release")
	}
	if active, released := pause.Check(until); active || !released {
		t.Fatal("missing release at edge")
	}
	if active, released := pause.Check(until); active || released || !pause.Until().IsZero() {
		t.Fatal("duplicate release")
	}
	if _, started := pause.Extend(until, until.Add(time.Hour)); !started {
		t.Fatal("new episode did not start")
	}
}
