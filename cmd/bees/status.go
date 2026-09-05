package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/state"
)

// workHoursLine renders the "work hours:" line of `bees status`. It is always
// printed: an operator who never set scheduler.work_hours still needs to know
// that, and which cadence applies instead. The yes/no is computed from now, so
// it is right even when the scheduler is stopped; st only contributes the
// recorded time of the next poll.
func workHoursLine(s config.Scheduler, st state.Status, now time.Time) string {
	line := fmt.Sprintf("work hours: not configured — GitHub polled every %s", s.PollInterval)
	if s.WorkHoursEnabled() {
		yes := "no"
		if s.InWorkHours(now) {
			yes = "yes"
		}
		line = fmt.Sprintf("work hours: %s (%s)", yes, s.WorkHoursDescription(now))
	}
	// The scheduler records NextPoll whether or not work hours are
	// configured, and a cadence without it says nothing about when the next
	// poll actually happens.
	switch d := st.NextPoll.Sub(now).Round(time.Second); {
	case st.NextPoll.IsZero():
	case d > 0:
		line += fmt.Sprintf("   next GitHub poll in %s", d)
	default:
		line += "   next GitHub poll due"
	}
	return line
}

// workHoursView is the `work_hours` object of `bees status --json`: the live
// answer, computed when the command runs. status.in_work_hours next to it is
// the scheduler's own record from its last pass and goes stale as soon as the
// scheduler stops, so the two are reported side by side.
type workHoursView struct {
	Configured   bool      `json:"configured"`
	InWorkHours  *bool     `json:"in_work_hours,omitempty"`
	Window       string    `json:"window,omitempty"`
	PollInterval string    `json:"poll_interval"`
	CheckedAt    time.Time `json:"checked_at"`
}

// workHoursJSON computes the work_hours object for `bees status --json`.
func workHoursJSON(s config.Scheduler, now time.Time) workHoursView {
	v := workHoursView{
		Configured:   s.WorkHoursEnabled(),
		PollInterval: s.PollIntervalAt(now).String(),
		CheckedAt:    now,
	}
	if v.Configured {
		in := s.InWorkHours(now)
		v.InWorkHours, v.Window = &in, s.WorkHoursDescription(now)
	}
	return v
}

// degradedText renders the "degraded:" section of `bees status`: the factory
// operations that are failing right now, one line each. The name column is a
// minimum width, not a bound: most operation names fit it, but the longer ones
// ("pre-review-checks", and the per-role "project-prompts/<role>") push the
// rest of their own line right, and only that line loses its alignment. It
// returns "" when nothing is failing, and the caller prints no section at all —
// a clean run should not carry an empty heading.
func degradedText(st state.Status) string {
	if len(st.Degraded) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\ndegraded:\n")
	for _, f := range st.Degraded {
		fmt.Fprintf(&b, "  %-16s %s", f.Op, failureCount(f))
		if f.LastError != "" {
			fmt.Fprintf(&b, "   last: %s", f.LastError)
		}
		b.WriteString("\n")
	}
	return b.String()
}

// failureCount describes the streak: how many times in a row, and over how
// long. A single failure has no span worth printing.
func failureCount(f state.OpFailure) string {
	if f.Count < 2 {
		return "1 failure"
	}
	return fmt.Sprintf("%d consecutive failures over %s", f.Count, state.ShortDur(f.Last.Sub(f.First)))
}

// workersText renders the "developer workers:" section of `bees status`: one
// line per running worker, with the issue it owns, its size, the stage it is
// in, the round it is on and, when it took over from a session that never
// finished — a scheduler dying while it ran, or a hard stop — that it
// resumed rather than started fresh: the branch of a resumed worker may
// already carry work nobody reported.
func workersText(st state.Status) string {
	if len(st.Workers) == 0 {
		return "  none\n"
	}
	var b strings.Builder
	for _, w := range st.Workers {
		round := fmt.Sprintf("round %d", w.Round)
		if w.Attempt > 1 {
			round += fmt.Sprintf(" attempt %d", w.Attempt)
		}
		size := w.Size
		if size == "" {
			size = "-"
		}
		fmt.Fprintf(&b, "  %-12s issue #%-5d %-3s %-17s %-20s since %s", w.Name, w.Issue, size, w.Stage, round, w.Since.Format(time.Kitchen))
		if w.Resumed {
			b.WriteString("   resumed")
		}
		b.WriteString("\n")
	}
	return b.String()
}

// schedulerLine renders the "scheduler:" line of `bees status`. When
// scheduler.max_cost_per_day is configured it also carries the rolling 24h
// spend against it, and says so plainly while dispatch is paused. Both
// numbers come from status.json — they are what the scheduler last computed,
// not a fresh sum, so they go stale with the rest of the file when it stops.
// The claude session limit is reported before the daily budget: it is the
// harder stop, and it names the time it lifts because that is the only
// thing a person can do anything about.
//
// The build the running scheduler was started from comes last, after
// anything that has stopped the factory: it is attribution rather than a
// thing to act on. It is omitted entirely when status.json carries no
// version — one written by a bees older than the field — so the line reads
// exactly as it always did.
//
// LastPoll stays zero until the first poll succeeds, so a scheduler whose
// polls have all failed (an expired token, a network outage at startup) has
// UpdatedAt set but no LastPoll: that is reported plainly instead of the
// saturated duration now.Sub(time.Time{}) would otherwise print.
func schedulerLine(st state.Status, now time.Time) string {
	line := "scheduler: never run"
	switch {
	case st.UpdatedAt.IsZero():
		// line stays "scheduler: never run"
	case st.LastPoll.IsZero():
		line = fmt.Sprintf("scheduler: pid %d, no successful poll yet", st.PID)
	default:
		line = fmt.Sprintf("scheduler: pid %d, last poll %s ago", st.PID, now.Sub(st.LastPoll).Round(time.Second))
	}
	switch notice := st.PauseNotice(now); {
	case notice != "":
		line += "   paused: " + notice
	case st.DayBudgetUSD > 0:
		line += fmt.Sprintf("   daily budget: $%.2f / $%.2f", st.DaySpendUSD, st.DayBudgetUSD)
	}
	if st.Version != "" {
		line += "   build " + st.Version
	}
	return line
}
