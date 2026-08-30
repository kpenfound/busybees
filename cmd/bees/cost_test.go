package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/state"
)

// fixtureLedger writes a small ledger and reads it back, the way
// `bees cost` does.
func fixtureLedger(t *testing.T) []state.LedgerEntry {
	t.Helper()
	store := state.New(t.TempDir())
	day1 := time.Date(2026, 8, 28, 12, 0, 0, 0, time.Local)
	day2 := day1.Add(24 * time.Hour)
	for _, e := range []state.LedgerEntry{
		{Time: day1, Role: "developer", Session: "developer-issue-12-r1", Issue: 12, PR: 34, Turns: 18, CostUSD: 0.42, Outcome: "pr-opened"},
		{Time: day1.Add(time.Hour), Role: "reviewer", Session: "reviewer-pr-34-r1", Issue: 12, PR: 34, Turns: 6, CostUSD: 0.10, Outcome: "changes-requested"},
		{Time: day2, Role: "developer", Session: "developer-issue-13-r1", Issue: 13, PR: 35, Turns: 10, CostUSD: 0.25, Outcome: "pr-opened"},
		{Time: day2.Add(time.Hour), Role: "qa", Session: "qa-r1", Turns: 4, CostUSD: 0.03, Outcome: "reported"},
	} {
		if err := store.AppendLedger(e); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := store.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	return entries
}

func TestGroupCost(t *testing.T) {
	entries := fixtureLedger(t)
	tests := []struct {
		by   string
		want []costGroup
	}{
		{byRole, []costGroup{
			{Group: "developer", Sessions: 2, Turns: 28, CostUSD: 0.67},
			{Group: "qa", Sessions: 1, Turns: 4, CostUSD: 0.03},
			{Group: "reviewer", Sessions: 1, Turns: 6, CostUSD: 0.10},
		}},
		{byIssue, []costGroup{
			{Group: "12", Sessions: 2, Turns: 24, CostUSD: 0.52},
			{Group: "13", Sessions: 1, Turns: 10, CostUSD: 0.25},
			{Group: noGroup, Sessions: 1, Turns: 4, CostUSD: 0.03},
		}},
		{byDay, []costGroup{
			{Group: "2026-08-28", Sessions: 2, Turns: 24, CostUSD: 0.52},
			{Group: "2026-08-29", Sessions: 2, Turns: 14, CostUSD: 0.28},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.by, func(t *testing.T) {
			groups, total := groupCost(entries, tt.by)
			if len(groups) != len(tt.want) {
				t.Fatalf("got %d groups, want %d: %+v", len(groups), len(tt.want), groups)
			}
			for i, g := range groups {
				w := tt.want[i]
				if g.Group != w.Group || g.Sessions != w.Sessions || g.Turns != w.Turns || !closeTo(g.CostUSD, w.CostUSD) {
					t.Errorf("group %d: got %+v want %+v", i, g, w)
				}
			}
			if total.Sessions != 4 || total.Turns != 38 || !closeTo(total.CostUSD, 0.80) {
				t.Errorf("total: %+v", total)
			}
		})
	}
}

func closeTo(a, b float64) bool { return a-b < 1e-9 && b-a < 1e-9 }

func TestGroupCostEmpty(t *testing.T) {
	groups, total := groupCost(nil, byRole)
	if len(groups) != 0 || total.Sessions != 0 {
		t.Fatalf("got %+v %+v", groups, total)
	}
}

func TestCostText(t *testing.T) {
	groups, total := groupCost(fixtureLedger(t), byRole)
	got := costText(byRole, groups, total)
	want := []string{
		"role             sessions    turns       cost",
		"developer               2       28      $0.67",
		"qa                      1        4      $0.03",
		"reviewer                1        6      $0.10",
		"total                   4       38      $0.80",
	}
	if got != strings.Join(want, "\n")+"\n" {
		t.Fatalf("cost table:\n%s", got)
	}
}

func TestCostJSONRoundTrip(t *testing.T) {
	groups, total := groupCost(fixtureLedger(t), byIssue)
	b, err := json.Marshal(map[string]any{"since": "24h0m0s", "by": byIssue, "groups": groups, "total": total})
	if err != nil {
		t.Fatal(err)
	}
	var back struct {
		Since  string      `json:"since"`
		By     string      `json:"by"`
		Groups []costGroup `json:"groups"`
		Total  costGroup   `json:"total"`
	}
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if back.By != byIssue || back.Since != "24h0m0s" {
		t.Fatalf("header: %+v", back)
	}
	if len(back.Groups) != len(groups) {
		t.Fatalf("groups: %+v", back.Groups)
	}
	for i := range groups {
		if back.Groups[i] != groups[i] {
			t.Errorf("group %d: got %+v want %+v", i, back.Groups[i], groups[i])
		}
	}
	if back.Total != total {
		t.Errorf("total: got %+v want %+v", back.Total, total)
	}
}

func TestTodayTotal(t *testing.T) {
	store := state.New(t.TempDir())
	now := time.Now()
	for _, e := range []state.LedgerEntry{
		{Time: startOfDay(now).Add(-time.Hour), Role: "developer", Turns: 100, CostUSD: 9},
		{Time: startOfDay(now).Add(time.Minute), Role: "developer", Turns: 12, CostUSD: 0.40},
		{Time: now, Role: "reviewer", Turns: 11, CostUSD: 0.10},
	} {
		if err := store.AppendLedger(e); err != nil {
			t.Fatal(err)
		}
	}
	total := todayTotal(store, now)
	if total.Sessions != 2 || total.Turns != 23 || !closeTo(total.CostUSD, 0.50) {
		t.Fatalf("today: %+v", total)
	}
	if got := todayText(total); got != "today: 2 sessions, 23 turns, $0.50" {
		t.Fatalf("today line: %q", got)
	}
	one := costGroup{Group: "today", Sessions: 1, Turns: 1, CostUSD: 0.05}
	if got := todayText(one); got != "today: 1 session, 1 turn, $0.05" {
		t.Fatalf("singular today line: %q", got)
	}
}

func TestTodayTotalNoLedger(t *testing.T) {
	total := todayTotal(state.New(t.TempDir()), time.Now())
	if got := todayText(total); got != "today: 0 sessions, 0 turns, $0.00" {
		t.Fatalf("today line: %q", got)
	}
}
