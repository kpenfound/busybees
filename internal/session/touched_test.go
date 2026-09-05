package session

import (
	"os"
	"path/filepath"
	"testing"
)

func wantNumbers(t *testing.T, got []int, want ...int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("touched issues %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("touched issues %v, want %v", got, want)
		}
	}
}

func read(t *testing.T, dir string) []int {
	t.Helper()
	got, err := TouchedIssues(dir)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// The list is in the order the session recorded it and carries each issue
// once: a session that relabels an issue and then rewrites its body has
// changed one issue, and the scheduler spends one call on it.
func TestTouchedIssuesAreRecordedOnceInOrder(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []int{41, 7, 41} {
		if err := RecordTouched(dir, n); err != nil {
			t.Fatal(err)
		}
	}
	wantNumbers(t, read(t, dir), 41, 7)
}

// The common case: a session with no tools that change an issue, and a
// session run outside a session directory (`bees mcp serve` by hand), record
// nothing and read back nothing. Neither is an error.
func TestASessionThatRecordedNothingReadsBackNothing(t *testing.T) {
	wantNumbers(t, read(t, t.TempDir()))
	if err := RecordTouched("", 41); err != nil {
		t.Fatalf("recording without a session directory: %v", err)
	}
	wantNumbers(t, read(t, ""))
}

// A line that is not an issue number — a torn concurrent append — costs its
// own entry and nothing else: the rest of the list is still worth reading,
// and whatever is lost is picked up by the next poll.
func TestAnUnreadableLineDoesNotCostTheWholeList(t *testing.T) {
	dir := t.TempDir()
	if err := RecordTouched(dir, 41); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(filepath.Join(dir, TouchedFile), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("4\x009\nnot a number\n-3\n0\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if err := RecordTouched(dir, 7); err != nil {
		t.Fatal(err)
	}
	wantNumbers(t, read(t, dir), 41, 7)
}
