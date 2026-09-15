package ops

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kpenfound/busybees/core/work"
)

// LedgerEntry records the accounting and caller-defined outcome of one session.
type LedgerEntry struct {
	Work    work.Ref  `json:"work"`
	Time    time.Time `json:"time"`
	Role    string    `json:"role"`
	Session string    `json:"session"`

	Turns        int     `json:"turns"`
	CostUSD      float64 `json:"cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
	Outcome      string  `json:"outcome"`
	ErrorSubtype string  `json:"error_subtype"`
	TimedOut     bool    `json:"timed_out"`
}

// Ledger serializes append and trim operations. Share one instance per file.
// Dir and Now must not change while the ledger is in use. A nil Now uses time.Now.
type Ledger struct {
	Dir string
	Now func() time.Time
	mu  sync.Mutex
}

func (s *Ledger) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// NewLedger opens accounting rooted at dir without creating files.
func NewLedger(dir string) *Ledger { return &Ledger{Dir: dir} }

// LedgerPath returns the ledger file.
func (s *Ledger) LedgerPath() string { return filepath.Join(s.Dir, "ledger.jsonl") }

// AppendLedger appends one entry to the ledger, creating it if needed. The
// line is written with a single Write to an O_APPEND file so concurrent
// workers never interleave.
func (s *Ledger) AppendLedger(e LedgerEntry) error {
	if e.Time.IsZero() {
		e.Time = s.now()
	}
	e.Time = e.Time.UTC()
	line, err := json.Marshal(e)
	if err != nil {
		return err
	}
	line = append(line, '\n')
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.LedgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(line); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// ReadLedger returns the entries recorded at or after since (a zero since
// returns everything). Lines that do not parse are skipped: a half-written
// tail must never break accounting. Read and scan failures return an error
// without entries, so callers cannot report a partial total.
func (s *Ledger) ReadLedger(since time.Time) ([]LedgerEntry, error) {
	f, err := os.Open(s.LedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var out []LedgerEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLedgerLine)
	for sc.Scan() {
		var e LedgerEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		if e.Time.Before(since) {
			continue
		}
		out = append(out, e)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// TrimLedger removes the entries recorded before before and returns how many
// it removed. The ledger is rewritten through a temporary file renamed over
// it, so a reader sees either the old ledger or the trimmed one, and only when
// there is something to remove. A line that does not parse is kept: its age is
// unknown, and ReadLedger skips it anyway.
func (s *Ledger) TrimLedger(before time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, err := os.ReadFile(s.LedgerPath())
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	var kept []byte
	removed := 0
	for len(b) > 0 {
		line := b
		if i := bytes.IndexByte(b, '\n'); i >= 0 {
			line, b = b[:i+1], b[i+1:]
		} else {
			b = nil
		}
		var e LedgerEntry
		if err := json.Unmarshal(line, &e); err == nil && e.Time.Before(before) {
			removed++
			continue
		}
		kept = append(kept, line...)
	}
	if removed == 0 {
		return 0, nil
	}
	tmp, err := os.CreateTemp(s.Dir, "ledger.jsonl.*")
	if err != nil {
		return 0, err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(kept); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return 0, err
	}
	if err := tmp.Close(); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp.Name(), s.LedgerPath()); err != nil {
		return 0, err
	}
	return removed, nil
}

// maxLedgerLine caps how long a ledger line may be before scanning fails.
const maxLedgerLine = 1 << 20
