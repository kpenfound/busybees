package scheduler

import (
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/state"
)

// TestPassTrimsTheLedger checks that a full pass removes the ledger lines
// older than scheduler.retention_period, and that a retention_period under
// 24h still leaves every line the daily budget counts.
func TestPassTrimsTheLedger(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, retention, want string
	}{
		{"default", "", "day,recent"},
		{"longer", `retention_period = "48h"` + "\n", "twoday,day,recent"},
		{"shorter than a day", `retention_period = "1h"` + "\n", "day,recent"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessAt(t, baseTOML+tc.retention, now)
			for _, e := range []state.LedgerEntry{
				{Time: now.Add(-72 * time.Hour), Role: config.RoleQA, Session: "ancient"},
				{Time: now.Add(-47 * time.Hour), Role: config.RoleDeveloper, Session: "twoday"},
				{Time: now.Add(-23 * time.Hour), Role: config.RoleReviewer, Session: "day"},
				{Time: now.Add(-time.Minute), Role: config.RoleDeveloper, Session: "recent"},
			} {
				if err := h.store.AppendLedger(e); err != nil {
					t.Fatal(err)
				}
			}

			runPass(t, h)

			got, err := h.store.ReadLedger(time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			var sessions []string
			for _, e := range got {
				if e.Time.Before(now.Add(-time.Minute)) || e.Session == "recent" {
					sessions = append(sessions, e.Session)
				}
			}
			if s := strings.Join(sessions, ","); s != tc.want {
				t.Errorf("ledger kept %q, want %q", s, tc.want)
			}
		})
	}
}
