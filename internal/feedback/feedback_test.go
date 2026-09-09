package feedback

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAddFillsIDAndTimeAndListReturnsOldestFirst(t *testing.T) {
	q := Open(filepath.Join(t.TempDir(), "feedback"))
	if got, err := q.List(); err != nil || len(got) != 0 {
		t.Fatalf("empty queue: %v, %v", got, err)
	}

	later := time.Date(2026, 9, 8, 12, 0, 0, 0, time.UTC)
	second, err := q.Add(Draft{Role: "developer", SessionDir: "/s/sessions/2", Title: "later", Detail: "b", CreatedAt: later})
	if err != nil {
		t.Fatal(err)
	}
	first, err := q.Add(Draft{Role: "qa", Title: "earlier", Detail: "a", CreatedAt: later.Add(-time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []Draft{first, second} {
		if d.ID == "" || d.CreatedAt.IsZero() {
			t.Errorf("Add left the draft unfilled: %+v", d)
		}
	}
	if _, err := os.Stat(filepath.Join(q.Root(), first.ID+".json")); err != nil {
		t.Errorf("draft not written under the queue: %v", err)
	}

	got, err := q.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Title != "earlier" || got[1].Title != "later" {
		t.Fatalf("List = %+v, want earlier then later", got)
	}
	if got[1].Role != "developer" || got[1].SessionDir != "/s/sessions/2" || got[1].Detail != "b" || !got[1].CreatedAt.Equal(later) {
		t.Errorf("round trip lost a field: %+v", got[1])
	}
}

func TestAddSetsTheTimeWhenNoneIsGiven(t *testing.T) {
	q := Open(t.TempDir())
	before := time.Now().UTC().Add(-time.Second)
	d, err := q.Add(Draft{Role: "reviewer", Title: "t", Detail: "d"})
	if err != nil {
		t.Fatal(err)
	}
	if d.CreatedAt.Before(before) {
		t.Errorf("CreatedAt %v is before the call", d.CreatedAt)
	}
	if !strings.HasPrefix(d.ID, d.CreatedAt.Format("20060102T150405")) {
		t.Errorf("ID %q does not start with the timestamp", d.ID)
	}
}

func TestAddRefusesAnIncompleteDraft(t *testing.T) {
	q := Open(filepath.Join(t.TempDir(), "feedback"))
	for name, d := range map[string]Draft{
		"no role":   {Title: "t", Detail: "d"},
		"no title":  {Role: "qa", Title: "  ", Detail: "d"},
		"no detail": {Role: "qa", Title: "t"},
	} {
		if _, err := q.Add(d); err == nil {
			t.Errorf("%s: Add accepted %+v", name, d)
		}
	}
	if got, err := q.List(); err != nil || len(got) != 0 {
		t.Fatalf("a refused draft was written: %v, %v", got, err)
	}
	if _, err := os.Stat(q.Root()); !os.IsNotExist(err) {
		t.Errorf("a refused draft created the queue directory: %v", err)
	}
}

func TestListSkipsNonDraftsAndReportsCorruption(t *testing.T) {
	q := Open(t.TempDir())
	if _, err := q.Add(Draft{Role: "qa", Title: "t", Detail: "d"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(q.Root(), "notes.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(q.Root(), "sub.json"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := q.List()
	if err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v; want the one draft", got, err)
	}
	if err := os.WriteFile(filepath.Join(q.Root(), "bad.json"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := q.List(); err == nil || !strings.Contains(err.Error(), "corrupt draft") {
		t.Errorf("List over a corrupt file: %v, want a corrupt draft error", err)
	}
}
