package state

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/ghwork"
)

func TestAppendAndReadLedger(t *testing.T) {
	s := New(t.TempDir())
	base := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	entries := []LedgerEntry{
		{Time: base, Role: "developer", Session: "developer-issue-12-r1", Turns: 18, CostUSD: 0.42, DurationMS: 214000, Outcome: "pr-opened", Work: ghwork.New(12, 34)},
		{Time: base.Add(time.Hour), Role: "reviewer", Session: "reviewer-pr-34-r1", Turns: 7, CostUSD: 0.11, Outcome: "approved", Work: ghwork.New(12, 34)},
	}
	for _, e := range entries {
		if err := s.AppendLedger(e); err != nil {
			t.Fatal(err)
		}
	}
	// A truncated write from a killed session must not break the read.
	f, err := os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"time\":\"not a\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLedger(LedgerEntry{Time: base.Add(2 * time.Hour), Role: "qa", Session: "qa-r1", Turns: 3, CostUSD: 0.05, Outcome: "reported"}); err != nil {
		t.Fatal(err)
	}

	got, err := s.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 {
		t.Fatalf("read %d entries, want 3: %+v", len(got), got)
	}
	if !reflect.DeepEqual(got[0], entries[0]) {
		t.Errorf("first entry: got %+v want %+v", got[0], entries[0])
	}
	if got[2].Role != "qa" {
		t.Errorf("garbage line was not skipped: %+v", got)
	}

	// since filters, inclusively.
	got, err = s.ReadLedger(base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Role != "reviewer" {
		t.Fatalf("since: %+v", got)
	}
}

func TestReadLedgerMissingFile(t *testing.T) {
	s := New(t.TempDir())
	got, err := s.ReadLedger(time.Time{})
	if err != nil || got != nil {
		t.Fatalf("got %+v, %v; want nil, nil", got, err)
	}
}

func TestAppendLedgerConcurrent(t *testing.T) {
	s := New(t.TempDir())
	const workers, each = 8, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				e := LedgerEntry{Role: "developer", Session: strings.Repeat("x", 200), Turns: i, Work: ghwork.New(w, 0)}
				if err := s.AppendLedger(e); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	b, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	if len(lines) != workers*each {
		t.Fatalf("got %d lines, want %d", len(lines), workers*each)
	}
	got, err := s.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != workers*each {
		t.Fatalf("parsed %d entries, want %d (interleaved writes)", len(got), workers*each)
	}
	for _, e := range got {
		if len(e.Session) != 200 {
			t.Fatalf("torn line: %+v", e)
		}
	}
}

func TestTrimLedger(t *testing.T) {
	s := New(t.TempDir())
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, e := range []LedgerEntry{
		{Time: base.Add(-72 * time.Hour), Role: "developer", Session: "ancient"},
		{Time: base.Add(-25 * time.Hour), Role: "reviewer", Session: "old"},
		{Time: base.Add(-24 * time.Hour), Role: "qa", Session: "edge"},
		{Time: base.Add(-time.Hour), Role: "developer", Session: "recent"},
	} {
		if err := s.AppendLedger(e); err != nil {
			t.Fatal(err)
		}
	}
	// A line with no parseable time is kept: its age is unknown.
	f, err := os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("{\"time\":\"not a\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}

	removed, err := s.TrimLedger(base.Add(-24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Errorf("removed %d entries, want 2", removed)
	}
	got, err := s.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	var sessions []string
	for _, e := range got {
		sessions = append(sessions, e.Session)
	}
	if strings.Join(sessions, ",") != "edge,recent" {
		t.Errorf("kept %v, want [edge recent] (the cutoff is inclusive)", sessions)
	}
	b, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(b), "{\"time\":\"not a\n") {
		t.Errorf("unparseable line dropped:\n%s", b)
	}
	if info, err := os.Stat(s.LedgerPath()); err != nil || info.Mode().Perm() != 0o644 {
		t.Errorf("ledger mode after trim: %v, %v", info, err)
	}
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("state dir holds %d files after the trim, want ledger, schema marker and lock: %v", len(entries), entries)
	}

	// Nothing older than the cutoff: the file is left alone.
	before, err := os.Stat(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if removed, err := s.TrimLedger(base.Add(-24 * time.Hour)); err != nil || removed != 0 {
		t.Errorf("second trim: removed %d, %v", removed, err)
	}
	after, err := os.Stat(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Error("a trim with nothing to remove rewrote the ledger")
	}
}

func TestTrimLedgerMissingFile(t *testing.T) {
	s := New(t.TempDir())
	if removed, err := s.TrimLedger(time.Now()); err != nil || removed != 0 {
		t.Fatalf("got %d, %v; want 0, nil", removed, err)
	}
	if _, err := os.Stat(s.LedgerPath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("trim created the ledger: %v", err)
	}
}

// TestTrimLedgerKeepsConcurrentAppends trims while workers append: every line
// appended must survive, since each is newer than the cutoff.
func TestTrimLedgerKeepsConcurrentAppends(t *testing.T) {
	s := New(t.TempDir())
	now := time.Now()
	for i := 0; i < 50; i++ {
		if err := s.AppendLedger(LedgerEntry{Time: now.Add(-48 * time.Hour), Session: "old"}); err != nil {
			t.Fatal(err)
		}
	}
	const workers, each = 8, 50
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				if err := s.AppendLedger(LedgerEntry{Time: now, Session: "new"}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			if _, err := s.TrimLedger(now.Add(-24 * time.Hour)); err != nil {
				t.Error(err)
				return
			}
		}
	}()
	wg.Wait()
	if _, err := s.TrimLedger(now.Add(-24 * time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != workers*each {
		t.Fatalf("%d entries after trimming, want %d", len(got), workers*each)
	}
}

func TestReadLedgerScanFailure(t *testing.T) {
	s := New(t.TempDir())
	if err := s.AppendLedger(LedgerEntry{CostUSD: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLedger(LedgerEntry{Session: strings.Repeat("x", maxLedgerLine), CostUSD: 2}); err != nil {
		t.Fatal(err)
	}
	got, err := s.ReadLedger(time.Time{})
	if err == nil || len(got) != 0 {
		t.Fatalf("partial ledger returned: %v, %v", got, err)
	}
}
