package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/state"
)

func TestNeedsConsolidation(t *testing.T) {
	const every, maxBytes = 10, 32768
	cases := []struct {
		name     string
		rs       state.RoleState
		notesLen int
		want     bool
	}{
		{"first session of all", state.RoleState{}, 200, false},
		{"count not reached", state.RoleState{Sessions: 12, LastConsolidated: 10}, 200, false},
		{"count reached", state.RoleState{Sessions: 19, LastConsolidated: 10}, 200, true},
		{"count reached from zero", state.RoleState{Sessions: 9}, 200, true},
		{"size exceeded, count not reached", state.RoleState{Sessions: 3, LastConsolidated: 2}, maxBytes + 1, true},
		{"size at the limit is not exceeded", state.RoleState{Sessions: 3, LastConsolidated: 2}, maxBytes, false},
		{"first session with an oversized file", state.RoleState{}, maxBytes + 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needsConsolidation(c.rs, c.notesLen, every, maxBytes); got != c.want {
				t.Errorf("needsConsolidation(%+v, %d) = %v, want %v", c.rs, c.notesLen, got, c.want)
			}
		})
	}
}

func TestConsolidateReason(t *testing.T) {
	if got := consolidateReason(200, 10, 32768); got != "every 10 sessions" {
		t.Errorf("count reason: %q", got)
	}
	if got := consolidateReason(40960, 10, 32768); got != "file is 40 KB" {
		t.Errorf("size reason: %q", got)
	}
}

// With notes_consolidate_every = 2 a developer's second session is asked to
// consolidate its notes, and the ask is recorded so the next one is not.
func TestNotesConsolidationIsAskedForOnSchedule(t *testing.T) {
	h := newHarness(t, baseTOML+"notes_consolidate_every = 2\n")
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
		HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// developer -> reviewer -> developer: only the developer's second
	// session is two sessions past the last (never made) consolidation.
	dev := h.sessions(config.RoleDeveloper)
	if len(dev) != 2 {
		t.Fatalf("developer sessions: %d", len(dev))
	}
	const ask = "Also consolidate your notes this session"
	if first := readFile(t, dev[0]+"/prompt.md"); strings.Contains(first, ask) {
		t.Errorf("first developer session was asked to consolidate:\n%s", first)
	}
	second := readFile(t, dev[1]+"/prompt.md")
	if !strings.Contains(second, "Also consolidate your notes this session (every 2 sessions)") {
		t.Errorf("second developer session was not asked to consolidate:\n%s", second)
	}
	if !strings.Contains(second, h.store.NotesPath(config.RoleDeveloper)) {
		t.Errorf("the ask does not name the notes file:\n%s", second)
	}

	rs, err := h.store.Role(config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Sessions != 2 || rs.LastConsolidated != 2 {
		t.Errorf("developer bookkeeping: sessions = %d, last_consolidated = %d; want 2 and 2", rs.Sessions, rs.LastConsolidated)
	}
	// The reviewer counts its own sessions in its own file.
	rev, err := h.store.Role(config.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Sessions != len(h.sessions(config.RoleReviewer)) {
		t.Errorf("reviewer sessions: %d recorded, %d run", rev.Sessions, len(h.sessions(config.RoleReviewer)))
	}
}

// A singleton's run must not overwrite the session counters that live in the
// same file.
func TestSingletonRunKeepsSessionCounters(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.project_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n")
	h.sched.OnlyRoles = map[string]bool{config.RoleProductManager: true}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(h.sessions(config.RoleProductManager)); n != 1 {
		t.Fatalf("product manager sessions: %d", n)
	}
	rs, err := h.store.Role(config.RoleProductManager)
	if err != nil {
		t.Fatal(err)
	}
	if rs.LastRun.IsZero() {
		t.Error("the product manager's last run was not recorded")
	}
	if rs.Sessions != 1 {
		t.Errorf("product manager sessions recorded: %d, want 1", rs.Sessions)
	}
}
