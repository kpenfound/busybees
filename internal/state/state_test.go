package state

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

// TestTheRunningSessionSurvivesASaveIssue: the record of the session running
// for an issue has the same shape as the cost totals — a developer worker
// holds one IssueState for the whole life of an issue while another writer
// moves the record underneath it — but it fails in both directions, so it is
// owned by SetIssueSession alone. A worker's save must neither resurrect a
// record that has since been cleared (which would report a crash that never
// happened to the next session) nor clear one a session has just written
// (which would lose the report of a real one).
func TestTheRunningSessionSurvivesASaveIssue(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	run := &SessionRun{Role: "developer", Name: "developer-issue-7-r1", Dir: "/s/7", StartedAt: time.Now().UTC()}
	if err := s.SetIssueSession(7, run); err != nil {
		t.Fatal(err)
	}
	// The worker loads the issue with the record in it, as it would after a
	// crash, and the session it starts clears it.
	held, err := s.Issue(7)
	if err != nil {
		t.Fatal(err)
	}
	if held.Session == nil {
		t.Fatal("SetIssueSession recorded nothing")
	}
	if err := s.SetIssueSession(7, nil); err != nil {
		t.Fatal(err)
	}
	held.Round = 3
	if err := s.SaveIssue(held); err != nil {
		t.Fatal(err)
	}
	got, err := s.Issue(7)
	if err != nil {
		t.Fatal(err)
	}
	if got.Round != 3 {
		t.Errorf("round: got %d want 3", got.Round)
	}
	if got.Session != nil {
		t.Errorf("a cleared record was resurrected by a stale save: %+v", got.Session)
	}
	// The other direction: a record written while the worker holds a copy
	// that predates it survives the worker's next save.
	if err := s.SetIssueSession(7, run); err != nil {
		t.Fatal(err)
	}
	held.Round = 4
	if err := s.SaveIssue(held); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.Issue(7); got.Session == nil || got.Session.Name != run.Name {
		t.Errorf("a stale save dropped the running session: %+v", got.Session)
	}
}

// Two goroutines saving the same issue must not corrupt its state file. Every
// writer used to go through one temp name derived from the destination
// (issues/<n>.json.tmp), so concurrent saves truncated and wrote the same
// file and one could rename what the other was still writing. The result was
// invalid JSON, and permanent: every reader of a corrupt IssueState warns and
// carries on without rewriting it, so nothing ever repaired it.
//
// The assertion is on the errors and on the final read, not on the race
// detector: this is a race between filesystem syscalls, not between memory
// accesses, and -race does not see it.
func TestConcurrentSaveIssue(t *testing.T) {
	s := New(t.TempDir())
	var (
		mu    sync.Mutex
		fails int
		first error
	)
	fail := func(what string, err error) {
		mu.Lock()
		defer mu.Unlock()
		fails++
		if first == nil {
			first = fmt.Errorf("%s: %w", what, err)
		}
	}
	var wg sync.WaitGroup
	for g := range 2 {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			// 6000 writes in total is the margin that makes this fail on
			// every run against the shared temp name rather than flakily.
			for i := range 3000 {
				is, err := s.Issue(7)
				if err != nil {
					fail("read", err)
					continue
				}
				if g == 0 {
					is.OpenChildren = []int{1, 2, 3, i}
				} else {
					is.Proposal = i%2 == 0
				}
				if err := s.SaveIssue(is); err != nil {
					fail("save", err)
				}
			}
		}(g)
	}
	wg.Wait()
	if fails > 0 {
		t.Errorf("%d of 6000 read/write cycles failed, first: %v", fails, first)
	}
	if _, err := s.Issue(7); err != nil {
		t.Fatalf("the issue state file is corrupt: %v", err)
	}
}

// Every state file is written 0644 and leaves no temp file behind, on the
// success path and on an error path alike.
func TestWrittenStateFilesAreModeSixFourFourAndLeaveNoTempFile(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.SaveIssue(IssueState{Number: 7, Round: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRole("developer", RoleState{Sessions: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveStatus(Status{}); err != nil {
		t.Fatal(err)
	}
	// A value json.MarshalIndent cannot encode fails before anything is
	// created; one that fails mid-write is not reachable through the Store,
	// so writeJSON's own error path is exercised directly.
	if err := s.writeJSON(filepath.Join(dir, "bad.json"), func() {}); err == nil {
		t.Error("writeJSON accepted an unmarshalable value")
	}
	// A destination that is a directory fails at the rename, after the temp
	// file exists: that is the path the cleanup is there for.
	if err := os.MkdirAll(filepath.Join(dir, "taken.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := s.writeJSON(filepath.Join(dir, "taken.json"), IssueState{Number: 1}); err == nil {
		t.Error("writeJSON renamed over a directory")
	}

	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if strings.HasSuffix(path, ".tmp") {
			t.Errorf("temp file left behind: %s", path)
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if got := info.Mode().Perm(); got != 0o644 {
			t.Errorf("%s: mode %o, want 644", path, got)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
