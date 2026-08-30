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
