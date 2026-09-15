package state

import (
	"path/filepath"
	"time"

	"github.com/kpenfound/busybees/core/ops"
)

// LedgerEntry is the core accounting record; migration belongs to this adapter.
type LedgerEntry = ops.LedgerEntry

func (s *Store) LedgerPath() string { return filepath.Join(s.Dir, "ledger.jsonl") }

func (s *Store) ledgerStore() *ops.Ledger {
	s.ledgerOnce.Do(func() { s.ledger = ops.NewLedger(s.Dir) })
	return s.ledger
}

func (s *Store) AppendLedger(e LedgerEntry) error {
	if err := s.Migrate(); err != nil {
		return err
	}
	return s.ledgerStore().AppendLedger(e)
}

func (s *Store) ReadLedger(since time.Time) ([]LedgerEntry, error) {
	if err := s.MigrateExisting(); err != nil {
		return nil, err
	}
	return s.ledgerStore().ReadLedger(since)
}

func (s *Store) TrimLedger(before time.Time) (int, error) {
	if err := s.Migrate(); err != nil {
		return 0, err
	}
	return s.ledgerStore().TrimLedger(before)
}
