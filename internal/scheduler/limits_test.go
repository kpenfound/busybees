package scheduler

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// TestPauseUntil covers what the factory does with the reset time a session
// reported: an unusable one falls back to scheduler.rate_limit_backoff, an
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
			if got := pauseUntil(now, c.resets, backoff); !got.Equal(c.want) {
				t.Errorf("pauseUntil = %s, want %s", got, c.want)
			}
		})
	}
	// A pause until exactly now is one the dispatch gate never sees: the
	// predicate asks whether the clock is still before it.
	if !now.Before(pauseUntil(now, now, backoff)) == false {
		t.Error("a reset at now must not pause dispatch")
	}
}

// TestSessionLimitPausesDeveloperDispatch: a developer session that dies on
// the account-wide limit is not retried, its issue is not escalated, and
// nothing else is dispatched until the reset time — which `bees status`
// carries. The pause then lifts by itself.
func TestSessionLimitPausesDeveloperDispatch(t *testing.T) {
	now := time.Date(2026, 8, 30, 23, 13, 0, 0, time.UTC)
	resets := now.Add(37 * time.Minute)
	t.Setenv("FAKE_LIMIT", strconv.FormatInt(resets.Unix(), 10))
	h := newHarnessAt(t, strings.Replace(devOnlyTOML, "[scheduler]\n", "[scheduler]\nretries = 2\nretry_delay = \"1ms\"\n", 1), now)
	seedReady(h, 1, "s", now)

	runPass(t, h)

	if n := sessionCount(h); n != 1 {
		t.Errorf("%d sessions ran, want exactly 1: the limit must not be retried", n)
	}
	if got := h.gh.history[1]; strings.Contains(strings.Join(got, ","), "bees:needs-human") {
		t.Errorf("issue 1 was escalated for the account's limit: %v", got)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !st.LimitPausedUntil.Equal(resets) {
		t.Errorf("status limit_paused_until = %s, want %s", st.LimitPausedUntil, resets)
	}
	if !strings.Contains(h.logs.String(), "claude session limit reached; starting no new sessions until") {
		t.Errorf("the pause was not reported:\n%s", h.logs.String())
	}

	// The next pass, still inside the window: a second ready issue waits.
	// The clock moves past the per-issue backoff a failed worker leaves so
	// that the session limit is the only thing holding anything back.
	seedReady(h, 2, "s", now)
	h.clock.advance(time.Minute)
	forcePoll(h)
	runPass(t, h)
	if n := sessionCount(h); n != 1 {
		t.Errorf("%d sessions after the pause started, want the same 1", n)
	}
	if got := h.gh.history[2]; len(got) != 0 {
		t.Errorf("issue 2 was picked up while paused: %v", got)
	}

	// Past the reset time: dispatch resumes and the lift is reported. The
	// limit is over, so the sessions do their work again.
	t.Setenv("FAKE_LIMIT", "")
	h.clock.advance(40 * time.Minute)
	forcePoll(h)
	runPass(t, h)
	if n := sessionCount(h); n < 2 {
		t.Errorf("%d sessions after the reset, want dispatch to have resumed", n)
	}
	if !strings.Contains(h.logs.String(), "claude session limit reset; dispatching again") {
		t.Errorf("the lift was not reported:\n%s", h.logs.String())
	}
	st, _ = h.store.LoadStatus()
	if !st.LimitPausedUntil.IsZero() {
		t.Errorf("status still paused until %s after the reset", st.LimitPausedUntil)
	}
}

// TestSessionLimitPausesSingletonDispatch: the same pause holds the
// singleton roles, which is the whole point — the limit is per account, so
// every role would otherwise walk into it in turn. This one reports no
// reset time, so the pause is scheduler.rate_limit_backoff long.
func TestSessionLimitPausesSingletonDispatch(t *testing.T) {
	now := time.Date(2026, 8, 30, 23, 13, 0, 0, time.UTC)
	t.Setenv("FAKE_LIMIT", "none")
	toml := strings.Replace(baseTOML, "[scheduler]\n", "[scheduler]\nrate_limit_backoff = \"20m\"\n", 1)
	h := newHarnessAt(t, toml, now)
	h.sched.OnlyRoles = map[string]bool{config.RoleProjectManager: true}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Needs triage", Body: "hi", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: now}

	runPass(t, h)
	if n := len(h.sessions(config.RoleProjectManager)); n != 1 {
		t.Fatalf("%d project manager sessions, want 1", n)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if want := now.Add(20 * time.Minute); !st.LimitPausedUntil.Equal(want) {
		t.Errorf("status limit_paused_until = %s, want the rate_limit_backoff fallback %s", st.LimitPausedUntil, want)
	}

	// Past the singleton's own backoff after a failed run, inside the pause.
	h.clock.advance(time.Minute)
	forcePoll(h)
	runPass(t, h)
	if n := len(h.sessions(config.RoleProjectManager)); n != 1 {
		t.Errorf("%d project manager sessions while paused, want the same 1", n)
	}

	h.clock.advance(25 * time.Minute)
	forcePoll(h)
	runPass(t, h)
	if n := len(h.sessions(config.RoleProjectManager)); n < 2 {
		t.Errorf("%d project manager sessions after the reset, want dispatch to have resumed", n)
	}
}

// TestRateLimitedTextNamesTheSessionLimit: the sentence a person sees when
// the account runs out of capacity matched none of the phrases that mark a
// message "come back later", so it read as an ordinary failure wherever
// that list is consulted.
func TestRateLimitedTextNamesTheSessionLimit(t *testing.T) {
	for _, c := range []struct {
		msg  string
		want bool
	}{
		{"You've hit your session limit · resets 11:50pm (America/Detroit)", true},
		{"Claude AI usage limit reached", true},
		{"gh: API rate limit exceeded for user", true},
		{"the tests do not pass", false},
	} {
		if got := rateLimitedText(c.msg); got != c.want {
			t.Errorf("rateLimitedText(%q) = %v, want %v", c.msg, got, c.want)
		}
	}
}
