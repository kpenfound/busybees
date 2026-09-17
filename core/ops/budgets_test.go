package ops

import (
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/work"
)

// TestOverBudgetStreak: the streak is per work item, and any session within
// budget clears it — two expensive sessions a week apart are not a pattern.
func TestOverBudgetStreak(t *testing.T) {
	var streak Streaks[work.Key]
	for i, want := range []int{1, 2, 3} {
		if got := streak.Record("job/1", true); got != want {
			t.Fatalf("over-budget session %d: streak %d want %d", i+1, got, want)
		}
	}
	if got := streak.Record("job/1", false); got != 0 {
		t.Errorf("a session within budget should clear the streak, got %d", got)
	}
	if got := streak.Record("job/1", true); got != 1 {
		t.Errorf("streak after the reset: got %d want 1", got)
	}
	if got := streak.Record("job/2", true); got != 1 {
		t.Errorf("another work item shares the streak: got %d want 1", got)
	}
}

func TestBudgetEdgesAndRollingWindow(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	entries := []LedgerEntry{
		{Time: now.Add(-24*time.Hour - time.Nanosecond), Work: work.Ref{Key: "build/A"}, CostUSD: 99},
		{Time: now.Add(-24 * time.Hour), Work: work.Ref{Key: "build/A"}, CostUSD: 3},
		{Time: now, Work: work.Ref{Key: "build/B"}, CostUSD: 2},
	}
	if cost, n, unknown := Spend(entries, "build/A", time.Time{}); cost != 102 || n != 2 || unknown != 0 {
		t.Fatalf("work total = %v across %v, %v unknown", cost, n, unknown)
	}
	if cost, n, unknown := Spend(entries, "build/A", now.Add(-24*time.Hour)); cost != 3 || n != 1 || unknown != 0 {
		t.Fatalf("window total = %v across %v, %v unknown", cost, n, unknown)
	}
	for _, tc := range []struct {
		name                            string
		limit, resume                   float64
		was, reached, crossed, released bool
	}{
		{"unlimited", 0, 100, false, false, false, false},
		{"negative unlimited", -1, 100, true, false, false, true},
		{"under", 6, 100, false, false, false, false},
		{"at limit", 5, 100, false, true, true, false},
		{"already reached", 5, 100, true, true, false, false},
		{"at resume", 10, 50, true, true, false, false},
		{"below resume", 10, 60, true, false, false, true},
		{"restart clears hysteresis", 10, 50, false, false, false, false},
		{"default resume", 6, 100, true, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := EvaluateWindow(entries, now, 24*time.Hour, tc.limit, tc.resume, tc.was)
			if got.Spent != 5 || got.Resume != tc.limit*tc.resume/100 || got.Reached != tc.reached || got.Crossed != tc.crossed || got.Released != tc.released {
				t.Fatalf("signal = %+v", got)
			}
		})
	}
	// A session that reported no cost is a session and no dollars, and the
	// window says how many it left out.
	unknown := append(entries, LedgerEntry{Time: now, Work: work.Ref{Key: "build/A"}, CostUSD: 50, CostUnknown: true})
	if cost, n, u := Spend(unknown, "build/A", time.Time{}); cost != 102 || n != 3 || u != 1 {
		t.Fatalf("with an unknown cost: total = %v across %v, %v unknown", cost, n, u)
	}
	if got := EvaluateWindow(unknown, now, 24*time.Hour, 5, 100, false); got.Spent != 5 || got.Unknown != 1 || !got.Reached {
		t.Fatalf("window with an unknown cost: %+v", got)
	}
	if got := EvaluateWindow(entries, now, 24*time.Hour, 5, 100, false); got.Unknown != 0 {
		t.Fatalf("window without one: %+v", got)
	}
	for _, tc := range []struct {
		cost, limit float64
		over        bool
	}{
		{99, 0, false}, {99, -1, false}, {1, 2, false}, {2, 2, false}, {2.01, 2, true},
	} {
		if got := OverBudget(tc.cost, tc.limit); got != tc.over {
			t.Errorf("OverBudget(%v,%v)=%v", tc.cost, tc.limit, got)
		}
	}
}
