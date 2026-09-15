package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/work"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/statemigrate"
)

func TestOpaqueWorkState(t *testing.T) {
	s := New(t.TempDir())
	ref := work.Ref{Key: work.Key("../" + strings.Repeat("opaque", 100)), Tags: map[string]string{"caller/key": "✓", "branch": "topic"}}
	held, err := s.Work(ref)
	if err != nil {
		t.Fatal(err)
	}
	held.Round = 2
	held.Branch = "topic"
	if err := s.SaveWork(held); err != nil {
		t.Fatal(err)
	}
	run := &SessionRun{Role: "custom", Name: "session", Dir: "/scratch/session", StartedAt: time.Now().UTC()}
	if err := s.SetWorkSession(ref, run); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddWorkCost(ref, 3.5); err != nil {
		t.Fatal(err)
	}
	held.Round = 3
	if err := s.SaveWork(held); err != nil {
		t.Fatal(err)
	}
	got, err := s.Work(ref)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Work, ref) || !reflect.DeepEqual(got.Session, run) || got.Round != 3 || got.Cost != 3.5 || got.Sessions != 1 {
		t.Fatalf("lost state: %+v", got)
	}
	keys, err := s.WorkKeys()
	if err != nil || !reflect.DeepEqual(keys, []work.Key{ref.Key}) {
		t.Fatalf("keys: %v %v", keys, err)
	}
	if err := s.AppendLedger(LedgerEntry{Work: ref, CostUSD: 3.5}); err != nil {
		t.Fatal(err)
	}
	ledger, err := s.ReadLedger(time.Time{})
	if err != nil || len(ledger) != 1 || !reflect.DeepEqual(ledger[0].Work, ref) {
		t.Fatalf("ledger: %v %v", ledger, err)
	}
	st := Status{Workers: []Worker{{Work: ref}}, NeedsHuman: []Escalated{{Work: ref}}, Approved: []ApprovedPR{{Work: ref}}, Priority: []work.Key{ref.Key}, WaitingOnDeps: map[work.Key][]work.Key{ref.Key: {"dependent"}}}
	if err := s.SaveStatus(st); err != nil {
		t.Fatal(err)
	}
	loaded, err := s.LoadStatus()
	if err != nil || !reflect.DeepEqual(loaded.Workers, st.Workers) || !reflect.DeepEqual(loaded.WaitingOnDeps, st.WaitingOnDeps) {
		t.Fatalf("status: %+v %v", loaded, err)
	}
	// A file at the right hash carrying a different key must never be overwritten.
	b, _ := json.Marshal(WorkState{Work: work.Ref{Key: "different"}, Cost: 99})
	if err := os.WriteFile(s.WorkPath(ref.Key), b, 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveWork(held); err == nil {
		t.Fatal("overwrote mismatched identity")
	}
	after, _ := os.ReadFile(s.WorkPath(ref.Key))
	if string(after) != string(b) {
		t.Fatal("mismatched record was discarded")
	}
}

func TestStateAccessMigratesBeforeReadingOrWriting(t *testing.T) {
	for _, operation := range []string{"init", "ledger-read", "ledger-write", "bookkeeping", "status", "mail-read", "mail-write"} {
		t.Run(operation, func(t *testing.T) {
			s := New(t.TempDir())
			legacy := `{"issue":12,"pr":34,"session":"old","cost_usd":4}` + "\n"
			if err := os.WriteFile(s.LedgerPath(), []byte(legacy), 0644); err != nil {
				t.Fatal(err)
			}
			box := mail.Open(s.MailDir(), s.Migrate)
			var err error
			switch operation {
			case "init":
				err = s.Init()
			case "ledger-read":
				_, err = s.ReadLedger(time.Time{})
			case "ledger-write":
				err = s.AppendLedger(LedgerEntry{Work: ghwork.New(12, 34), Session: "new"})
			case "bookkeeping":
				_, err = s.Issue(12)
			case "status":
				_, err = s.LoadStatus()
			case "mail-read":
				_, err = box.List(mail.Filter{})
			case "mail-write":
				_, err = box.Send(mail.Message{From: "human", To: "custom", Subject: "new"})
			}
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(s.LedgerPath())
			if err != nil || strings.Contains(string(b), `"issue":`) || !strings.Contains(string(b), `"github.issue":"12"`) {
				t.Fatalf("read/wrote unmigrated state: %s %v", b, err)
			}
			if _, err := os.Stat(filepath.Join(s.Dir, statemigrate.Marker)); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestMigrationFailureBlocksWriters(t *testing.T) {
	s := New(t.TempDir())
	path := filepath.Join(s.MailDir(), "custom", "legacy.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{bad"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := s.AppendLedger(LedgerEntry{CostUSD: 1}); err == nil {
		t.Fatal("ledger writer ignored migration failure")
	}
	if err := s.SaveWork(WorkState{Work: work.Ref{Key: "new"}}); err == nil {
		t.Fatal("bookkeeping writer ignored migration failure")
	}
	if _, err := mail.Open(s.MailDir(), s.Migrate).Send(mail.Message{From: "human", To: "custom", Subject: "new"}); err == nil {
		t.Fatal("mail writer ignored migration failure")
	}
	if _, err := os.Stat(s.LedgerPath()); !os.IsNotExist(err) {
		t.Fatal("failed migration allowed a ledger write")
	}
	if _, err := os.Stat(filepath.Join(s.Dir, statemigrate.Marker)); !os.IsNotExist(err) {
		t.Fatal("failed migration published marker")
	}
}

func TestMigrationSkipsMalformedLegacyLedgerRecords(t *testing.T) {
	s := New(t.TempDir())
	bad := []string{`{"issue":"bad","cost_usd":99}`, `{"pr":{},"cost_usd":98}`, `{"issue":12,"turns":"bad","cost_usd":97}`}
	lines := []string{`{"issue":12,"session":"first","cost_usd":2}`}
	lines = append(lines, bad...)
	lines = append(lines, `{"pr":34,"session":"last","cost_usd":3}`, `{truncated`)
	if err := os.WriteFile(s.LedgerPath(), []byte(strings.Join(lines, "\n")), 0644); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		entries, err := s.ReadLedger(time.Time{})
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 2 || entries[0].CostUSD != 2 || entries[1].CostUSD != 3 || entries[0].Work.Key != ghwork.IssueKey(12) || entries[1].Work.Key != ghwork.PRKey(34) {
			t.Fatalf("accounting changed: %+v", entries)
		}
	}
	b, err := os.ReadFile(s.LedgerPath())
	if err != nil {
		t.Fatal(err)
	}
	for _, original := range bad {
		found := false
		for _, line := range strings.Split(string(b), "\n") {
			var preserved string
			if json.Unmarshal([]byte(line), &preserved) == nil && strings.TrimSuffix(preserved, "\n") == original {
				found = true
			}
			if line == original {
				found = true
			}
		}
		if !found {
			t.Errorf("lost malformed record %s in %s", original, b)
		}
	}
	if !strings.HasSuffix(string(b), "{truncated") {
		t.Fatal("lost truncated tail")
	}
	if _, err := s.TrimLedger(time.Now()); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ReadLedger(time.Time{})
	if err != nil || len(entries) != 0 {
		t.Fatalf("malformed records became accounting entries: %+v %v", entries, err)
	}
}
