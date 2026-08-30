package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// workHoursIntervals builds a mon-fri 09:00-18:00 UTC configuration with
// explicit polling intervals and every role disabled, so only the polling
// loop itself is exercised.
func workHoursIntervals(poll, offHours, backoff string) string {
	return fmt.Sprintf(`
version = 1
[project]
repo = "acme/widgets"
[scheduler]
poll_interval = %q
off_hours_poll_interval = %q
rate_limit_backoff = %q
max_developers = 2
max_review_rounds = 3
work_hours = "09:00-18:00"
work_days = ["mon", "tue", "wed", "thu", "fri"]
timezone = "UTC"
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`, poll, offHours, backoff)
}

// nextPoll reads the scheduler's next GitHub poll through the status file.
func nextPoll(t *testing.T, h *harness) time.Time {
	t.Helper()
	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	return st.NextPoll
}

// The last poll before the work day starts is scheduled for the moment the
// window opens, not a whole off-hours interval later, so the work day does not
// start late.
func TestPollIsScheduledForTheStartOfTheWorkDay(t *testing.T) {
	// 2026-08-31 08:55 UTC is a Monday, five minutes before the window opens.
	h := newHarnessAt(t, workHoursIntervals("5m", "8h", "15m"), time.Date(2026, 8, 31, 8, 55, 0, 0, time.UTC))
	ctx := context.Background()

	if full, err := h.sched.tick(ctx); err != nil || !full {
		t.Fatalf("first tick: full=%v err=%v", full, err)
	}
	if want := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC); !nextPoll(t, h).Equal(want) {
		t.Fatalf("next poll %s, want the moment the window opens (%s)", nextPoll(t, h), want)
	}

	// From there the loop polls every poll_interval, all day.
	for i, want := range []int{2, 3, 4, 5} {
		h.clock.advance(5 * time.Minute)
		if full, err := h.sched.tick(ctx); err != nil || !full {
			t.Fatalf("tick %d: full=%v err=%v", i, full, err)
		}
		if got := h.gh.callCount("issue list"); got != want {
			t.Fatalf("polls after tick %d: %d, want %d", i, got, want)
		}
	}
}

// rate_limit_backoff is a floor on the wait, never a speed-up: off hours the
// longer interval in force stands.
func TestRateLimitNeverShortensTheWait(t *testing.T) {
	for _, c := range []struct {
		name string
		now  time.Time
		want time.Time
	}{
		// 2026-08-29 is a Saturday, 2026-08-31 a Monday.
		{"off hours, the off-hours interval is longer", time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC), time.Date(2026, 8, 29, 20, 0, 0, 0, time.UTC)},
		{"work hours, the backoff is longer", time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC), time.Date(2026, 8, 31, 12, 15, 0, 0, time.UTC)},
		{"off hours, the backoff outlasts the start of the work day", time.Date(2026, 8, 31, 8, 55, 0, 0, time.UTC), time.Date(2026, 8, 31, 9, 10, 0, 0, time.UTC)},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newHarnessAt(t, workHoursIntervals("5m", "8h", "15m"), c.now)
			h.gh.errFor["issue list"] = errors.New("gh: API rate limit exceeded for user")
			full, err := h.sched.tick(context.Background())
			if !full || err == nil {
				t.Fatalf("tick: full=%v err=%v", full, err)
			}
			if !isRateLimited(err) {
				t.Fatalf("the fake failure is not recognised as a rate limit: %v", err)
			}
			if got := nextPoll(t, h); !got.Equal(c.want) {
				t.Fatalf("next poll %s, want %s", got, c.want)
			}
		})
	}
}
