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

func TestDegradedTextIsAbsentWhenNothingIsFailing(t *testing.T) {
	if got := degradedText(state.Status{}); got != "" {
		t.Errorf("clean status printed %q", got)
	}
	if got := degradedText(state.Status{Degraded: []state.OpFailure{}}); got != "" {
		t.Errorf("empty slice printed %q", got)
	}
}

func TestDegradedTextListsEveryFailingOperation(t *testing.T) {
	first := time.Date(2026, 8, 30, 9, 0, 0, 0, time.UTC)
	st := state.Status{Degraded: []state.OpFailure{
		{Op: "assign", Count: 12, First: first, Last: first.Add(3*time.Hour + 10*time.Minute),
			LastError: "GraphQL: Projects (classic) is being deprecated", Escalated: true},
		{Op: "feature-progress", Count: 4, First: first, Last: first.Add(2 * time.Minute),
			LastError: "gh: HTTP 502"},
		{Op: "label", Count: 1, First: first, Last: first, LastError: "gh: not found"},
	}}
	want := "\ndegraded:\n" +
		"  assign           12 consecutive failures over 3h10m   last: GraphQL: Projects (classic) is being deprecated\n" +
		"  feature-progress 4 consecutive failures over 2m   last: gh: HTTP 502\n" +
		"  label            1 failure   last: gh: not found\n"
	if got := degradedText(st); got != want {
		t.Errorf("degraded section:\ngot:\n%swant:\n%s", got, want)
	}
}

func TestShortDur(t *testing.T) {
	for _, c := range []struct {
		d    time.Duration
		want string
	}{
		{0, "0s"},
		{45 * time.Second, "45s"},
		// The sub-minute/minute boundary: rounding to whole seconds happens
		// before the branch, so a streak that rounds up to a minute prints
		// "1m" and never Duration.String()'s "1m0s".
		{59400 * time.Millisecond, "59s"},
		{59500 * time.Millisecond, "1m"},
		{59900 * time.Millisecond, "1m"},
		{60 * time.Second, "1m"},
		{90 * time.Second, "1m"},
		{2 * time.Hour, "2h"},
		{10 * time.Minute, "10m"},
		{time.Hour, "1h"},
		{3*time.Hour + 10*time.Minute, "3h10m"},
		{50 * time.Hour, "50h"},
	} {
		if got := shortDur(c.d); got != c.want {
			t.Errorf("shortDur(%s) = %q, want %q", c.d, got, c.want)
		}
	}
}

func TestSchedulerLine(t *testing.T) {
	now := time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)
	running := state.Status{UpdatedAt: now, PID: 42, LastPoll: now.Add(-90 * time.Second)}
	// The session-limit pause prints a wall-clock time, so its rows carry
	// their own clock in the local zone the line is rendered in.
	local := time.Date(2026, 3, 1, 12, 0, 0, 0, time.Local)
	for _, tc := range []struct {
		name string
		st   state.Status
		// now overrides the shared clock; zero uses it.
		now  time.Time
		want string
	}{
		{name: "never run", st: state.Status{}, want: "scheduler: never run"},
		{name: "running, no budget", st: running, want: "scheduler: pid 42, last poll 1m30s ago"},
		{
			// A status.json that records the build the running scheduler
			// was started from names it, because the role prompts are
			// compiled in and a merged prompt change reaches no session
			// until bees is rebuilt and `bees run` restarted (#296).
			name: "running a recorded build",
			st: state.Status{UpdatedAt: now, PID: 42, LastPoll: now.Add(-90 * time.Second),
				Version: "dev (abc123def456 modified)", Revision: "abc123def456789"},
			want: "scheduler: pid 42, last poll 1m30s ago   build dev (abc123def456 modified)",
		},
		{
			// The build comes after anything that has stopped the factory:
			// it is attribution, not something to act on.
			name: "a recorded build under a pause",
			st: state.Status{UpdatedAt: now, PID: 42, LastPoll: now, BudgetPaused: true,
				DaySpendUSD: 101.2, DayBudgetUSD: 100, Version: "v0.2.0"},
			want: "scheduler: pid 42, last poll 0s ago   paused: daily budget ($101.20 / $100.00)   build v0.2.0",
		},
		{
			// A revision with no version is not a build anyone resolved:
			// only Version turns the segment on.
			name: "a revision without a version",
			st:   state.Status{UpdatedAt: now, PID: 42, LastPoll: now.Add(-90 * time.Second), Revision: "abc123def456789"},
			want: "scheduler: pid 42, last poll 1m30s ago",
		},
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
		{name: "never run but paused", st: state.Status{BudgetPaused: true, DaySpendUSD: 1, DayBudgetUSD: 0.5},
			want: "scheduler: never run   paused: daily budget ($1.00 / $0.50)"},
		{
			name: "paused on the claude session limit",
			st:   state.Status{UpdatedAt: local, PID: 42, LastPoll: local, LimitPausedUntil: local.Add(37 * time.Minute)},
			now:  local,
			want: "scheduler: pid 42, last poll 0s ago   paused: claude session limit until 12:37 (in 37m)",
		},
		{
			// The harder stop wins while it is in force...
			name: "paused on both",
			st: state.Status{UpdatedAt: local, PID: 42, LastPoll: local, LimitPausedUntil: local.Add(2 * time.Hour),
				BudgetPaused: true, DaySpendUSD: 101.2, DayBudgetUSD: 100},
			now:  local,
			want: "scheduler: pid 42, last poll 0s ago   paused: claude session limit until 14:00 (in 2h)",
		},
		{
			// ...and a reset time that has passed is no pause at all, even
			// when the scheduler has not looked at it since.
			name: "the session limit has reset",
			st: state.Status{UpdatedAt: local, PID: 42, LastPoll: local, LimitPausedUntil: local.Add(-time.Minute),
				BudgetPaused: true, DaySpendUSD: 101.2, DayBudgetUSD: 100},
			now:  local,
			want: "scheduler: pid 42, last poll 0s ago   paused: daily budget ($101.20 / $100.00)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			at := now
			if !tc.now.IsZero() {
				at = tc.now
			}
			if got := schedulerLine(tc.st, at); got != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// A worker that took over from a session a killed scheduler left unfinished
// is marked resumed, because its branch may already carry work nobody
// reported; one that started fresh reads exactly as it always did (#250).
func TestWorkersTextMarksAResumedWorker(t *testing.T) {
	since := time.Date(2026, 8, 31, 8, 22, 0, 0, time.Local)
	fresh := state.Worker{Name: "dev-1", Issue: 7, Size: "m", Stage: "developer", Round: 1, Since: since}
	resumed := state.Worker{Name: "dev-2", Issue: 9, Size: "s", Stage: "reviewer", Round: 2, Since: since, Resumed: true}

	if got := workersText(state.Status{}); got != "  none\n" {
		t.Errorf("no workers renders %q", got)
	}
	got := workersText(state.Status{Workers: []state.Worker{fresh, resumed}})
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("two workers render %d lines:\n%s", len(lines), got)
	}
	if strings.Contains(lines[0], "resumed") {
		t.Errorf("a worker that started fresh is marked resumed: %q", lines[0])
	}
	if !strings.HasSuffix(lines[1], "   resumed") {
		t.Errorf("a resumed worker is not marked: %q", lines[1])
	}
	// Everything the line said before is still on it, in the same columns.
	if !strings.HasPrefix(lines[1], "  dev-2        issue #9     s   reviewer          round 2              since ") {
		t.Errorf("the columns moved: %q", lines[1])
	}
}

// `bees status --json` needed no new key for the running build: the two
// fields ride along inside the marshalled `status` object, which is the whole
// state.Status. The test marshals the map the command builds, so a second,
// top-level copy of the version added later fails here rather than quietly
// giving a consumer two places to read it from.
func TestStatusJSONCarriesTheBuildInsideStatus(t *testing.T) {
	st := state.Status{Version: "dev (abc123def456 modified)", Revision: "abc123def456789"}
	raw, err := json.Marshal(map[string]any{
		"status": st, "unread_mail": map[string]int{}, "today": nil, "notes_bytes": map[string]int{},
		"work_hours": nil, "acting_as": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Status map[string]any `json:"status"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status["version"] != st.Version {
		t.Errorf("status.version: got %v want %q", got.Status["version"], st.Version)
	}
	if got.Status["revision"] != st.Revision {
		t.Errorf("status.revision: got %v want %q", got.Status["revision"], st.Revision)
	}
	var top map[string]any
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "revision", "build"} {
		if _, ok := top[key]; ok {
			t.Errorf("--json grew a top-level %q key; the build belongs inside status", key)
		}
	}
}
