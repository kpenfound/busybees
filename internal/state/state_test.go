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

// workerFields is the bookkeeping a developer worker owns and writes through
// SaveIssue. The two tests below are opposite halves of one rule — SaveIssue
// writes these fields and carries every other one over from the file — so
// they share the fixture rather than stating it twice.
func workerFields(round int) IssueState {
	return IssueState{
		Number:         7,
		Round:          round,
		PR:             101,
		Branch:         "bees/issue-7",
		CheckFixRounds: 2,
		WorkerStage:    "review",
		AfterDevelop:   "checks",
		PreReviewDone:  true,
	}
}

func wantWorkerFields(t *testing.T, got IssueState, round int, when string) {
	t.Helper()
	if want := workerFields(round); got.Round != want.Round || got.PR != want.PR ||
		got.Branch != want.Branch || got.CheckFixRounds != want.CheckFixRounds ||
		got.WorkerStage != want.WorkerStage || got.AfterDevelop != want.AfterDevelop ||
		got.PreReviewDone != want.PreReviewDone {
		t.Errorf("%s: the developer worker's bookkeeping was lost: got %+v want %+v", when, got, want)
	}
}

// TestThePollingPathsBookkeepingSurvivesASaveIssue: every field the scheduler
// writes on its polling path has the same shape as the cost totals — a
// developer worker holds one IssueState for the whole life of an issue, so
// anything it did not load is stale by the time it saves. Saving its copy
// wholesale would forget that a person's PR feedback had been delivered
// (delivering it twice), that a head had been mailed about (mailing it again,
// which is the one thing conflict_notified_sha exists to prevent), that a
// proposal had been approved (so the product manager is never told), which
// sub-issues a feature had open (so a finished feature is reported twice, or
// not at all) and why the factory gave the issue up (leaving the person it
// was escalated to nothing to read but the label). Each of those fields is
// owned by a method of its own and carried over by SaveIssue; what SaveIssue
// does write is the worker's own bookkeeping.
func TestThePollingPathsBookkeepingSurvivesASaveIssue(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveIssue(workerFields(1)); err != nil {
		t.Fatal(err)
	}
	// The feature carried the proposal label when the worker loaded it, so a
	// stale save writes that observation back too.
	if err := s.SetProposal(7, true, time.Time{}); err != nil {
		t.Fatal(err)
	}
	// The worker loads the issue once and holds it for the rest of its life.
	held, err := s.Issue(7)
	if err != nil {
		t.Fatal(err)
	}

	// Meanwhile the polling path records five things through their owners.
	seen := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	approved := time.Date(2026, 8, 31, 9, 30, 0, 0, time.UTC)
	reported := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	escalated := time.Date(2026, 8, 31, 10, 30, 0, 0, time.UTC)
	for _, step := range []struct {
		name string
		fn   func() error
	}{
		{"SetHumanSeenAt", func() error { return s.SetHumanSeenAt(7, seen) }},
		{"SetConflictNotifiedSHA", func() error { return s.SetConflictNotifiedSHA(7, "deadbee") }},
		{"SetProposal", func() error { return s.SetProposal(7, false, approved) }},
		{"SetOpenChildren", func() error { return s.SetOpenChildren(7, []int{11, 12}, reported) }},
		{"SetEscalation", func() error { return s.SetEscalation(7, "3 review rounds and no approval", escalated) }},
	} {
		if err := step.fn(); err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
	}

	// The worker's next save must write its own fields and nothing else.
	held.Round = 2
	if err := s.SaveIssue(held); err != nil {
		t.Fatal(err)
	}
	got, err := s.Issue(7)
	if err != nil {
		t.Fatal(err)
	}
	wantWorkerFields(t, got, 2, "after a save")
	if !got.HumanSeenAt.Equal(seen) {
		t.Errorf("human_seen_at: got %v want %v — feedback already delivered would be delivered again", got.HumanSeenAt, seen)
	}
	if got.ConflictNotifiedSHA != "deadbee" {
		t.Errorf("conflict_notified_sha: got %q want %q — the same head would be mailed about twice", got.ConflictNotifiedSHA, "deadbee")
	}
	if got.Proposal {
		t.Error("proposal: got true want false — the label observation was rolled back")
	}
	if !got.ProposalApprovedAt.Equal(approved) {
		t.Errorf("proposal_approved_at: got %v want %v — the product manager is never told", got.ProposalApprovedAt, approved)
	}
	if len(got.OpenChildren) != 2 || got.OpenChildren[0] != 11 || got.OpenChildren[1] != 12 {
		t.Errorf("open_children: got %v want [11 12]", got.OpenChildren)
	}
	if !got.CompleteReportedAt.Equal(reported) {
		t.Errorf("complete_reported_at: got %v want %v — the feature is reported complete twice", got.CompleteReportedAt, reported)
	}
	if got.Escalation != "3 review rounds and no approval" || !got.EscalatedAt.Equal(escalated) {
		t.Errorf("escalation: got %q at %v — the person it was handed to is told nothing but the label",
			got.Escalation, got.EscalatedAt)
	}
}

// The other direction of the same rule: an owner method reads the file,
// changes its own fields and writes it back, so it must keep whatever the
// developer worker saved since — including a save that landed between the
// owner's own read and its write on the previous call.
func TestTheOwnerMethodsPreserveTheWorkerFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		set  func(s *Store) error
		want func(t *testing.T, is IssueState)
	}{
		{
			name: "SetHumanSeenAt",
			set:  func(s *Store) error { return s.SetHumanSeenAt(7, time.Unix(1, 0).UTC()) },
			want: func(t *testing.T, is IssueState) {
				if is.HumanSeenAt.IsZero() {
					t.Error("human_seen_at was not recorded")
				}
			},
		},
		{
			name: "SetConflictNotifiedSHA",
			set:  func(s *Store) error { return s.SetConflictNotifiedSHA(7, "cafe") },
			want: func(t *testing.T, is IssueState) {
				if is.ConflictNotifiedSHA != "cafe" {
					t.Errorf("conflict_notified_sha: got %q", is.ConflictNotifiedSHA)
				}
			},
		},
		{
			name: "SetProposal",
			set:  func(s *Store) error { return s.SetProposal(7, true, time.Time{}) },
			want: func(t *testing.T, is IssueState) {
				if !is.Proposal {
					t.Error("proposal was not recorded")
				}
			},
		},
		{
			name: "SetEscalation",
			set:  func(s *Store) error { return s.SetEscalation(7, "gave up", time.Unix(2, 0).UTC()) },
			want: func(t *testing.T, is IssueState) {
				if is.Escalation != "gave up" || is.EscalatedAt.IsZero() {
					t.Errorf("escalation: got %q at %v", is.Escalation, is.EscalatedAt)
				}
			},
		},
		{
			name: "SetOpenChildren",
			set:  func(s *Store) error { return s.SetOpenChildren(7, []int{3}, time.Time{}) },
			want: func(t *testing.T, is IssueState) {
				if len(is.OpenChildren) != 1 || is.OpenChildren[0] != 3 {
					t.Errorf("open_children: got %v want [3]", is.OpenChildren)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(t.TempDir())
			if err := s.Init(); err != nil {
				t.Fatal(err)
			}
			// The worker saved its bookkeeping; the owner method is called
			// afterwards and must not write a copy that predates it.
			if err := s.SaveIssue(workerFields(3)); err != nil {
				t.Fatal(err)
			}
			if err := tc.set(s); err != nil {
				t.Fatal(err)
			}
			got, err := s.Issue(7)
			if err != nil {
				t.Fatal(err)
			}
			wantWorkerFields(t, got, 3, "after "+tc.name)
			tc.want(t, got)
		})
	}
}

// SetOpenChildren carries two rules the completeness check depends on: a nil
// set leaves the remembered one alone (an empty or incomplete lookup is
// indistinguishable from children that closed), and a set that differs clears
// the report marker so a feature that gains a sub-issue can be reported
// complete again.
func TestSetOpenChildrenKeepsARememberedSet(t *testing.T) {
	s := New(t.TempDir())
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	reported := time.Date(2026, 8, 31, 10, 0, 0, 0, time.UTC)
	if err := s.SetOpenChildren(7, []int{11, 12}, reported); err != nil {
		t.Fatal(err)
	}
	// An empty lookup records nothing and forgets nothing.
	if err := s.SetOpenChildren(7, nil, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Issue(7)
	if len(got.OpenChildren) != 2 || !got.CompleteReportedAt.Equal(reported) {
		t.Errorf("an empty lookup overwrote the remembered set: %v %v", got.OpenChildren, got.CompleteReportedAt)
	}
	// A set that changed re-arms the trigger.
	if err := s.SetOpenChildren(7, []int{11, 12, 13}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Issue(7)
	if len(got.OpenChildren) != 3 || !got.CompleteReportedAt.IsZero() {
		t.Errorf("a changed set did not re-arm the trigger: %v %v", got.OpenChildren, got.CompleteReportedAt)
	}
}

// The build the scheduler is running as survives a status.json round trip:
// #297 compares the revision against the repository, so it is recorded raw
// rather than as the 12-character form Version shows.
func TestStatusCarriesTheRunningBuild(t *testing.T) {
	s := New(t.TempDir())
	want := Status{
		Version:  "dev (b24a0605c2a1 modified)",
		Revision: "b24a0605c2a1e9f0d3c4b5a6978869d3d1e2f3a4",
	}
	if err := s.SaveStatus(want); err != nil {
		t.Fatal(err)
	}
	got, err := s.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != want.Version {
		t.Errorf("version: got %q want %q", got.Version, want.Version)
	}
	if got.Revision != want.Revision {
		t.Errorf("revision: got %q want %q", got.Revision, want.Revision)
	}
}

// A scheduler given no build records none: the two keys are absent from the
// file, so a status.json written by a bees that does not resolve them reads
// exactly like one written before the fields existed.
func TestStatusOmitsAnAbsentBuild(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	if err := s.SaveStatus(Status{}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "status.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"version"`, `"revision"`} {
		if strings.Contains(string(raw), key) {
			t.Errorf("status.json carries %s with no build recorded:\n%s", key, raw)
		}
	}
	got, err := s.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if got.Version != "" || got.Revision != "" {
		t.Errorf("got %q / %q, want both empty", got.Version, got.Revision)
	}
}
