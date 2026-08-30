package state

import (
	"os"
	"strings"
	"testing"
)

// A fresh notes file already has the shape roles are asked to consolidate
// their notes into.
func TestEnsureNotesWritesTheSkeleton(t *testing.T) {
	s := New(t.TempDir())
	if err := s.EnsureNotes("developer"); err != nil {
		t.Fatal(err)
	}
	notes, err := s.ReadNotes("developer")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(notes, "# developer notes\n") {
		t.Errorf("notes do not start with the title:\n%s", notes)
	}
	for _, h := range NotesSections {
		if !strings.Contains(notes, "\n## "+h+"\n") {
			t.Errorf("notes are missing the %q heading:\n%s", h, notes)
		}
	}
}

// EnsureNotes never touches a file a role (or a person) has written.
func TestEnsureNotesKeepsExistingNotes(t *testing.T) {
	s := New(t.TempDir())
	if err := s.EnsureNotes("reviewer"); err != nil {
		t.Fatal(err)
	}
	const mine = "# reviewer notes\n\nalways run the e2e suite\n"
	if err := os.WriteFile(s.NotesPath("reviewer"), []byte(mine), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureNotes("reviewer"); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.ReadNotes("reviewer"); got != mine {
		t.Errorf("notes were rewritten:\n%s", got)
	}
}

// Role bookkeeping round-trips, including the notes counters.
func TestRoleStateRoundTrip(t *testing.T) {
	s := New(t.TempDir())
	if err := s.SaveRole("developer", RoleState{Sessions: 12, LastConsolidated: 10}); err != nil {
		t.Fatal(err)
	}
	rs, err := s.Role("developer")
	if err != nil {
		t.Fatal(err)
	}
	if rs.Sessions != 12 || rs.LastConsolidated != 10 {
		t.Errorf("round trip: %+v", rs)
	}
	// An unknown role reads as the zero value, not an error.
	if rs, err := s.Role("qa"); err != nil || rs.Sessions != 0 {
		t.Errorf("missing role state: %+v, %v", rs, err)
	}
}

// TestIssueCostSurvivesASaveIssue: a developer worker holds one IssueState
// for the whole life of an issue, so SaveIssue must never write back the
// running cost as it was when the worker started.
func TestIssueCostSurvivesASaveIssue(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	held, err := s.Issue(7)
	if err != nil {
		t.Fatal(err)
	}
	held.Round = 1

	if is, err := s.AddIssueCost(7, 1.25); err != nil || is.Cost != 1.25 || is.Sessions != 1 {
		t.Fatalf("AddIssueCost: %+v %v", is, err)
	}
	if is, err := s.AddIssueCost(7, 0.75); err != nil || is.Cost != 2 || is.Sessions != 2 {
		t.Fatalf("AddIssueCost again: %+v %v", is, err)
	}

	held.Round = 2
	if err := s.SaveIssue(held); err != nil {
		t.Fatal(err)
	}
	got, err := s.Issue(7)
	if err != nil {
		t.Fatal(err)
	}
	if got.Round != 2 {
		t.Errorf("round: got %d want 2", got.Round)
	}
	if got.Cost != 2 || got.Sessions != 2 {
		t.Errorf("stale cost written back: $%v over %d sessions", got.Cost, got.Sessions)
	}

	// Seeding replaces the totals outright.
	if is, err := s.SetIssueCost(7, 9, 4); err != nil || is.Cost != 9 || is.Sessions != 4 {
		t.Fatalf("SetIssueCost: %+v %v", is, err)
	}
	if got, _ := s.Issue(7); got.Cost != 9 || got.Sessions != 4 || got.Round != 2 {
		t.Errorf("after seeding: %+v", got)
	}
}
