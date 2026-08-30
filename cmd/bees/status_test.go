package main

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/state"
)

// schedulerFor builds a resolved config.Scheduler from bees.toml text, with
// no repository and nothing on disk beyond the file Parse is handed.
func schedulerFor(t *testing.T, body string) config.Scheduler {
	t.Helper()
	text := "version = 1\n[project]\nrepo = \"a/b\"\n[scheduler]\npoll_interval = \"5m\"\n" + body
	cfg, err := config.Parse(text, filepath.Join(t.TempDir(), "bees.toml"))
	if err != nil {
		t.Fatal(err)
	}
	return cfg.Scheduler
}

// 2026-08-31 is a Monday.
func utcAt(t *testing.T, day, hour, min int) time.Time {
	t.Helper()
	return time.Date(2026, 8, day, hour, min, 0, 0, time.UTC)
}

func TestWorkHoursLine(t *testing.T) {
	// An explicit timezone and a fixed now keep the expected strings
	// independent of the machine the test runs on. 2026-08-31 is a Monday,
	// 2026-08-29 a Saturday.
	const window = "work_hours = \"09:00-18:00\"\ntimezone = \"UTC\"\n"
	for _, c := range []struct {
		name string
		toml string
		now  time.Time
		next time.Time
		want string
	}{
		{
			"configured, inside the window",
			window, utcAt(t, 31, 12, 0), utcAt(t, 31, 12, 2).Add(55 * time.Second),
			"work hours: yes (09:00-18:00 mon-fri, UTC)   next GitHub poll in 2m55s",
		},
		{
			"configured, outside the window",
			window, utcAt(t, 29, 12, 0), utcAt(t, 29, 11, 59),
			"work hours: no (09:00-18:00 mon-fri, UTC)   next GitHub poll due",
		},
		{
			"not configured, with a next poll",
			"", utcAt(t, 31, 12, 0), utcAt(t, 31, 12, 0).Add(30 * time.Second),
			"work hours: not configured — GitHub polled every 5m0s   next GitHub poll in 30s",
		},
		{
			"not configured, without a next poll",
			"", utcAt(t, 31, 12, 0), time.Time{},
			"work hours: not configured — GitHub polled every 5m0s",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := workHoursLine(schedulerFor(t, c.toml), state.Status{NextPoll: c.next}, c.now)
			if got != c.want {
				t.Fatalf("workHoursLine = %q, want %q", got, c.want)
			}
		})
	}
}

func TestWorkHoursJSON(t *testing.T) {
	now := utcAt(t, 31, 12, 0)
	t.Run("not configured", func(t *testing.T) {
		v := workHoursJSON(schedulerFor(t, ""), now)
		if v.Configured {
			t.Fatal("configured should be false without work_hours")
		}
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "in_work_hours") {
			t.Fatalf("in_work_hours must be omitted when work hours are off: %s", b)
		}
		if !strings.Contains(string(b), `"poll_interval":"5m0s"`) {
			t.Fatalf("cadence missing from %s", b)
		}
	})
	for _, c := range []struct {
		name string
		now  time.Time
		want bool
	}{
		{"inside the window", now, true},
		{"outside the window", utcAt(t, 31, 20, 0), false},
	} {
		t.Run("configured, "+c.name, func(t *testing.T) {
			s := schedulerFor(t, "work_hours = \"09:00-18:00\"\ntimezone = \"UTC\"\noff_hours_poll_interval = \"1h\"\n")
			v := workHoursJSON(s, c.now)
			if !v.Configured {
				t.Fatal("configured should be true")
			}
			if v.InWorkHours == nil || *v.InWorkHours != s.InWorkHours(c.now) {
				t.Fatalf("in_work_hours = %v, want %v", v.InWorkHours, s.InWorkHours(c.now))
			}
			if *v.InWorkHours != c.want {
				t.Fatalf("in_work_hours = %v, want %v", *v.InWorkHours, c.want)
			}
			if v.Window != "09:00-18:00 mon-fri, UTC" {
				t.Fatalf("window = %q", v.Window)
			}
			// The cadence reported is the one in force at checked_at.
			want := "5m0s"
			if !c.want {
				want = "1h0m0s"
			}
			if v.PollInterval != want {
				t.Fatalf("poll_interval = %q, want %q", v.PollInterval, want)
			}
			if !v.CheckedAt.Equal(c.now) {
				t.Fatalf("checked_at = %s, want %s", v.CheckedAt, c.now)
			}
		})
	}
}

func TestSchedulerLine(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	running := state.Status{UpdatedAt: now, PID: 42, LastPoll: now.Add(-90 * time.Second)}
	for _, tc := range []struct {
		name string
		st   state.Status
		want string
	}{
		{"never run", state.Status{}, "scheduler: never run"},
		{"running, no budget", running, "scheduler: pid 42, last poll 1m30s ago"},
		{
			name: "running under a budget",
			st:   state.Status{UpdatedAt: now, PID: 42, LastPoll: now, DaySpendUSD: 42.1, DayBudgetUSD: 100},
			want: "scheduler: pid 42, last poll 0s ago   daily budget: $42.10 / $100.00",
		},
		{
			name: "paused",
			st:   state.Status{UpdatedAt: now, PID: 42, LastPoll: now, BudgetPaused: true, DaySpendUSD: 101.2, DayBudgetUSD: 100},
			want: "scheduler: pid 42, last poll 0s ago   paused: daily budget ($101.20 / $100.00)",
		},
		{"never run but paused", state.Status{BudgetPaused: true, DaySpendUSD: 1, DayBudgetUSD: 0.5},
			"scheduler: never run   paused: daily budget ($1.00 / $0.50)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := schedulerLine(tc.st, now); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}
