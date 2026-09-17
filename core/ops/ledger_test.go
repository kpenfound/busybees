package ops

import (
	"errors"
	"os"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/work"
)

func TestAppendAndReadLedger(t *testing.T) {
	s := NewLedger(t.TempDir())
	base := time.Date(2026, 8, 29, 10, 0, 0, 0, time.UTC)
	entries := []LedgerEntry{
		{Time: base, Role: "builder", Session: "builder-job-a-r1", Turns: 18, CostUSD: 0.42, DurationMS: 214000, Outcome: "built", Work: work.Ref{Key: "job/a", Tags: map[string]string{"topic": "build"}}},
		{Time: base.Add(time.Hour), Role: "checker", Session: "checker-job-b-r1", Turns: 7, CostUSD: 0.11, Outcome: "approved", Work: work.Ref{Key: "job/a", Tags: map[string]string{"topic": "build"}}},
	}
	for _, e := range entries {
		if err := s.AppendLedger(e); err != nil {
			t.Fatal(err)
		}
	}
	// A truncated write from a killed session leaves a tail that does not
	// parse, and that must not break the read.
	appendRaw(t, s, "{\"time\":\"not a\n")

	got, err := s.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("read %d entries, want 2: %+v", len(got), got)
	}
	if !reflect.DeepEqual(got[0], entries[0]) {
		t.Errorf("first entry: got %+v want %+v", got[0], entries[0])
	}

	// since filters, inclusively.
	got, err = s.ReadLedger(base.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Role != "checker" {
		t.Fatalf("since: %+v", got)
	}

	// Once a session appends after the truncated tail, the tail is a line
	// in the middle, and the ledger no longer reads: a total that skipped
	// it would be quietly short.
	if err := s.AppendLedger(LedgerEntry{Time: base.Add(2 * time.Hour), Role: "audit", Session: "qa-r1", Turns: 3, CostUSD: 0.05, Outcome: "reported"}); err != nil {
		t.Fatal(err)
	}
	got, err = s.ReadLedger(time.Time{})
	var lineErr *LedgerLineError
	if !errors.As(err, &lineErr) || got != nil {
		t.Fatalf("corrupt line in the middle: got %+v, %v; want no entries and a LedgerLineError", got, err)
	}
	if lineErr.Path != s.LedgerPath() || lineErr.Line != 3 {
		t.Errorf("error names %s line %d, want %s line 3", lineErr.Path, lineErr.Line, s.LedgerPath())
	}
	if msg := err.Error(); !strings.Contains(msg, s.LedgerPath()) || !strings.Contains(msg, "line 3") {
		t.Errorf("error message names neither the file nor the line: %s", msg)
	}
}

// appendRaw writes bytes to the ledger as they are, the way a crash or a
// stray editor would.
func appendRaw(t *testing.T, s *Ledger, raw string) {
	t.Helper()
	f, err := os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(raw); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestReadLedgerFailsClosedOnACorruptMiddleLine: a line that does not parse
// anywhere but at the end is an error naming the file and the line, with no
// entries beside it, whatever shape the corruption takes.
func TestReadLedgerFailsClosedOnACorruptMiddleLine(t *testing.T) {
	for _, tc := range []struct {
		name, line string
	}{
		{"truncated", "{\"time\":\"not a"},
		{"not json", "garbage"},
		{"wrong type", "{\"cost_usd\":\"lots\"}"},
		// What an upgrade preserved of a record it could not read
		// (internal/statemigrate) is a JSON string, not an entry.
		{"preserved string", "\"{\\\"issue\\\":\\\"bad\\\"}\""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := NewLedger(t.TempDir())
			if err := s.AppendLedger(LedgerEntry{Session: "first", CostUSD: 1}); err != nil {
				t.Fatal(err)
			}
			appendRaw(t, s, tc.line+"\n")
			if err := s.AppendLedger(LedgerEntry{Session: "last", CostUSD: 2}); err != nil {
				t.Fatal(err)
			}
			got, err := s.ReadLedger(time.Time{})
			var lineErr *LedgerLineError
			if !errors.As(err, &lineErr) || got != nil {
				t.Fatalf("got %+v, %v; want no entries and a LedgerLineError", got, err)
			}
			if lineErr.Line != 2 || lineErr.Path != s.LedgerPath() {
				t.Errorf("error names %s line %d, want %s line 2", lineErr.Path, lineErr.Line, s.LedgerPath())
			}
		})
	}
}

// TestReadLedgerIgnoresTheTruncatedTail: only the final line may fail to
// parse, with or without its newline, and blank lines are not corruption.
func TestReadLedgerIgnoresTheTruncatedTail(t *testing.T) {
	for _, tail := range []string{"{\"time\":\"not a", "{\"time\":\"not a\n", "garbage\n\n", "\n\n"} {
		s := NewLedger(t.TempDir())
		if err := s.AppendLedger(LedgerEntry{Session: "first", CostUSD: 1}); err != nil {
			t.Fatal(err)
		}
		appendRaw(t, s, "\n")
		if err := s.AppendLedger(LedgerEntry{Session: "second", CostUSD: 2}); err != nil {
			t.Fatal(err)
		}
		appendRaw(t, s, tail)
		got, err := s.ReadLedger(time.Time{})
		if err != nil || len(got) != 2 || got[1].Session != "second" {
			t.Errorf("tail %q: got %+v, %v; want both entries", tail, got, err)
		}
	}
}

// TestAppendLedgerAfterATornTail: an append starts a fresh line after a
// tail with no newline, leaving the tail as a line that reads fail closed;
// a file already ending in a newline gets no blank line.
func TestAppendLedgerAfterATornTail(t *testing.T) {
	s := NewLedger(t.TempDir())
	if err := s.AppendLedger(LedgerEntry{Session: "first", CostUSD: 1}); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, s, "{\"time\":\"2026-09-1")
	if err := s.AppendLedger(LedgerEntry{Session: "next", CostUSD: 2}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	if len(lines) != 3 || lines[1] != "{\"time\":\"2026-09-1" || !strings.Contains(lines[2], "\"session\":\"next\"") {
		t.Fatalf("ledger lines = %q; want first, the torn tail, next", lines)
	}
	got, err := s.ReadLedger(time.Time{})
	var lineErr *LedgerLineError
	if !errors.As(err, &lineErr) || lineErr.Line != 2 || got != nil {
		t.Fatalf("got %+v, %v; want a LedgerLineError on line 2", got, err)
	}

	clean := NewLedger(t.TempDir())
	for _, name := range []string{"a", "b"} {
		if err := clean.AppendLedger(LedgerEntry{Session: name}); err != nil {
			t.Fatal(err)
		}
	}
	raw, err = os.ReadFile(clean.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "\n\n") || strings.Count(string(raw), "\n") != 2 || strings.HasPrefix(string(raw), "\n") {
		t.Errorf("ledger = %q; want two lines and no blank one", raw)
	}
}

// TestLedgerUnknownCostRoundTrip: an entry whose session reported no cost
// says so on the way back, and a line written before the field existed
// reads as a known cost.
func TestLedgerUnknownCostRoundTrip(t *testing.T) {
	s := NewLedger(t.TempDir())
	if err := s.AppendLedger(LedgerEntry{Session: "known", CostUSD: 1.5}); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLedger(LedgerEntry{Session: "unknown", CostUnknown: true}); err != nil {
		t.Fatal(err)
	}
	appendRaw(t, s, "{\"time\":\"2026-09-15T12:00:00Z\",\"session\":\"old\",\"cost_usd\":0.25}\n")
	got, err := s.ReadLedger(time.Time{})
	if err != nil || len(got) != 3 {
		t.Fatalf("got %+v, %v", got, err)
	}
	if got[0].CostUnknown || got[0].CostUSD != 1.5 {
		t.Errorf("known entry: %+v", got[0])
	}
	if !got[1].CostUnknown || got[1].CostUSD != 0 {
		t.Errorf("unknown entry: %+v", got[1])
	}
	if got[2].CostUnknown || got[2].CostUSD != 0.25 {
		t.Errorf("entry without the field: %+v", got[2])
	}
	b, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(b), "cost_unknown") != 1 {
		t.Errorf("the field is written for the unknown entry alone:\n%s", b)
	}
}

func TestReadLedgerMissingFile(t *testing.T) {
	s := NewLedger(t.TempDir())
	got, err := s.ReadLedger(time.Time{})
	if err != nil || got != nil {
		t.Fatalf("got %+v, %v; want nil, nil", got, err)
	}
}

func TestAppendLedgerConcurrent(t *testing.T) {
	s := NewLedger(t.TempDir())
	const workers, each = 8, 20
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < each; i++ {
				e := LedgerEntry{Role: "builder", Session: strings.Repeat("x", 200), Turns: i, Work: work.Ref{Key: work.Key(strconv.Itoa(w))}}
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
	s := NewLedger(t.TempDir())
	base := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	for _, e := range []LedgerEntry{
		{Time: base.Add(-72 * time.Hour), Role: "builder", Session: "ancient"},
		{Time: base.Add(-25 * time.Hour), Role: "checker", Session: "old"},
		{Time: base.Add(-24 * time.Hour), Role: "audit", Session: "edge"},
		{Time: base.Add(-time.Hour), Role: "builder", Session: "recent"},
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
	if len(entries) != 1 {
		t.Errorf("state dir holds %d files after the trim, want only ledger: %v", len(entries), entries)
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
	s := NewLedger(t.TempDir())
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
	s := NewLedger(t.TempDir())
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
	s := NewLedger(t.TempDir())
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

func TestTrimPreservesMalformedTruncatedBytes(t *testing.T) {
	s := NewLedger(t.TempDir())
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	if err := s.AppendLedger(LedgerEntry{Session: "clock"}); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ReadLedger(time.Time{}); err != nil || len(got) != 1 || !got[0].Time.Equal(now) {
		t.Fatalf("clock=%+v err=%v", got, err)
	}
	// Every retained byte, including unknown fields and an unfinished final
	// line without a newline, survives a rewrite triggered by an old record.
	kept := "not-json\n{\"time\":\"2026-09-15T12:00:00Z\",\"extra\":42}\n{\"unfinished\":"
	old := "{\"time\":\"2026-09-01T00:00:00Z\"}\n"
	if err := os.WriteFile(s.LedgerPath(), []byte(old+kept), 0644); err != nil {
		t.Fatal(err)
	}
	if removed, err := s.TrimLedger(now.Add(-24 * time.Hour)); err != nil || removed != 1 {
		t.Fatalf("trim=%d err=%v", removed, err)
	}
	if got, err := os.ReadFile(s.LedgerPath()); err != nil || string(got) != kept {
		t.Fatalf("retained=%q err=%v", got, err)
	}
}
