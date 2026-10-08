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
	if got := PauseUntil(now, now, backoff, maxLimitPause); now.Before(got) {
		t.Errorf("pauseUntil(reset=now) = %s, want no later than %s", got, now)
	}
}

func TestCapacityEpisodeExtendsAndReleasesOnce(t *testing.T) {
	var pause CapacityPause
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	if active, released := pause.Check(now); active || released {
		t.Fatalf("zero-value Check = (active=%v, released=%v), want (false, false)", active, released)
	}
	until, started := pause.Extend(now, now.Add(time.Hour))
	if !started || !until.Equal(now.Add(time.Hour)) {
		t.Fatalf("first Extend = (until=%s, started=%v), want (%s, true)", until, started, now.Add(time.Hour))
	}
	until, started = pause.Extend(now, now.Add(time.Minute))
	if started || !until.Equal(now.Add(time.Hour)) {
		t.Fatalf("Extend with a shorter until = (until=%s, started=%v), want (%s, false)", until, started, now.Add(time.Hour))
	}
	until, started = pause.Extend(now, now.Add(2*time.Hour))
	if started || !until.Equal(now.Add(2*time.Hour)) {
		t.Fatalf("Extend with a longer until = (until=%s, started=%v), want (%s, false)", until, started, now.Add(2*time.Hour))
	}
	if active, released := pause.Check(until.Add(-time.Nanosecond)); !active || released {
		t.Fatalf("Check one nanosecond before until = (active=%v, released=%v), want (true, false)", active, released)
	}
	if active, released := pause.Check(until); active || !released {
		t.Fatalf("Check at until = (active=%v, released=%v), want (false, true)", active, released)
	}
	if active, released := pause.Check(until); active || released || !pause.Until().IsZero() {
		t.Fatalf("second Check at until = (active=%v, released=%v, Until=%s), want (false, false, zero)", active, released, pause.Until())
	}
	if _, started := pause.Extend(until, until.Add(time.Hour)); !started {
		t.Fatalf("Extend after release: started=%v, want true", started)
	}
}
