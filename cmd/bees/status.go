package main

import (
	"fmt"
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
