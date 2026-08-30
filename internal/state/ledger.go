package state

import (
	"bufio"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"
)

// LedgerEntry records one finished session in the ledger. It is what the
// session cost the factory, not what it did: the outcome is kept as a label
// so `bees cost` can tell a wasted session from a productive one.
type LedgerEntry struct {
	Time         time.Time `json:"time"`
	Role         string    `json:"role"`
	Session      string    `json:"session"`
	Issue        int       `json:"issue"`
	PR           int       `json:"pr"`
	Turns        int       `json:"turns"`
	CostUSD      float64   `json:"cost_usd"`
	DurationMS   int64     `json:"duration_ms"`
	Outcome      string    `json:"outcome"`
	ErrorSubtype string    `json:"error_subtype"`
	TimedOut     bool      `json:"timed_out"`
}

// LedgerPath returns the ledger file.
func (s *Store) LedgerPath() string { return filepath.Join(s.Dir, "ledger.jsonl") }

// AppendLedger appends one entry to the ledger, creating it if needed. The
// line is written with a single Write to an O_APPEND file so concurrent
// workers never interleave.
func (s *Store) AppendLedger(e LedgerEntry) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
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
// tail must never break `bees cost`.
func (s *Store) ReadLedger(since time.Time) ([]LedgerEntry, error) {
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
	// A scan error (an overlong line, a truncated read) ends the ledger
	// early; what was read before it is still good.
	return out, nil
}

// maxLedgerLine caps how long a ledger line may be before it is skipped.
const maxLedgerLine = 1 << 20
