package state

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAppendAndReadLedger(t *testing.T) {
	s := New(t.TempDir())
	base := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	entries := []LedgerEntry{
		{Time: base, Role: "developer", Session: "developer-issue-12-r1", Issue: 12, PR: 34, Turns: 18, CostUSD: 0.42, DurationMS: 214000, Outcome: "pr-opened"},
		{Time: base.Add(time.Hour), Role: "reviewer", Session: "reviewer-pr-34-r1", Issue: 12, PR: 34, Turns: 7, CostUSD: 0.11, Outcome: "approved"},
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
	if got[0] != entries[0] {
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
				e := LedgerEntry{Role: "developer", Session: strings.Repeat("x", 200), Issue: w, Turns: i}
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
