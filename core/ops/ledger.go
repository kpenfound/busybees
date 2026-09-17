package ops

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
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

	Turns int `json:"turns"`
	// CostUSD is what the session cost, when the agent reported a cost.
	// CostUnknown says it reported none: the session ran and is counted,
	// but its cost is not zero, it is not known, and no total it is part
	// of is complete. An entry written before the field was recorded reads
	// as known.
	CostUSD      float64 `json:"cost_usd"`
	CostUnknown  bool    `json:"cost_unknown,omitempty"`
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

// LedgerLineError is a ledger line that does not parse, other than the last
// one: which file, which line, and what was wrong with it.
type LedgerLineError struct {
	Path string
	Line int
	Err  error
}

func (e *LedgerLineError) Error() string {
	return fmt.Sprintf("%s line %d does not parse: %v", e.Path, e.Line, e.Err)
}

func (e *LedgerLineError) Unwrap() error { return e.Err }

// ReadLedger returns the entries recorded at or after since (a zero since
// returns everything). The ledger is read fail-closed: a line that does not
// parse is a LedgerLineError naming the file and the line, and no entries
// are returned, so no caller can report or budget against a total that is
// quietly short of a session. The one exception is the final line, which is
// ignored when it does not parse: a session killed mid-write leaves a
// truncated tail, and that must never break accounting. Blank lines are
// ignored. Read and scan failures return an error without entries too.
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
	// pending is the last line that did not parse. It becomes the error
	// once a further line follows it, which is what makes it not the tail.
	var pending *LedgerLineError
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxLedgerLine)
	for n := 1; sc.Scan(); n++ {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		if pending != nil {
			return nil, pending
		}
		var e LedgerEntry
		if err := json.Unmarshal(line, &e); err != nil {
			pending = &LedgerLineError{Path: s.LedgerPath(), Line: n, Err: err}
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
// there is something to remove. A line that does not parse is kept where it
// is: its age is unknown, and ReadLedger is what reports it.
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
