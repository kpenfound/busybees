package ops

import (
	"sync"
	"testing"
	"time"
)

// TestDegradedCrossesThresholdOnce: the threshold-crossing signal fires on
// the record that reaches it and stays escalated, but never fires again for
// the same streak.
func TestDegradedCrossesThresholdOnce(t *testing.T) {
	var d Degraded
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		entry, crossed := d.Record("fetch", true, "unavailable", now.Add(time.Duration(i)*time.Second), 3)
		if entry.Count != i || crossed != (i == 3) || entry.Escalated != (i >= 3) {
			t.Fatalf("failure %d: got count=%d crossed=%v escalated=%v, want count=%d crossed=%v escalated=%v", i, entry.Count, crossed, entry.Escalated, i, i == 3, i >= 3)
		}
		if !entry.First.Equal(now.Add(time.Second)) || !entry.Last.Equal(now.Add(time.Duration(i)*time.Second)) {
			t.Fatalf("failure %d: got first=%s last=%s, want first=%s last=%s", i, entry.First, entry.Last, now.Add(time.Second), now.Add(time.Duration(i)*time.Second))
		}
	}
}

// TestDegradedSnapshotIsSortedAndIndependent: Snapshot orders by name and
// returns records a caller may mutate without touching tracked state.
func TestDegradedSnapshotIsSortedAndIndependent(t *testing.T) {
	var d Degraded
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	d.Record("fetch", true, "unavailable", now, 3)
	d.Record("fetch", true, "unavailable", now, 3)
	d.Record("apply", true, "denied", now, 3)
	snap := d.Snapshot()
	if len(snap) != 2 || snap[0].Op != "apply" || snap[1].Op != "fetch" {
		t.Fatalf("snapshot ops = %+v, want [apply fetch]", snap)
	}
	snap[1].Count = 99
	if got := d.Snapshot()[1].Count; got != 2 {
		t.Fatalf("fetch count after mutating a snapshot copy = %d, want 2 (unaffected)", got)
	}
}

// TestDegradedSuccessResetsStreak: a success clears the streak, so the next
// failure starts counting from one and can cross the threshold again.
func TestDegradedSuccessResetsStreak(t *testing.T) {
	var d Degraded
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	d.Record("fetch", true, "unavailable", now, 3)
	d.Record("fetch", true, "unavailable", now, 3)
	d.Record("fetch", false, "", now, 3)
	for i := 1; i <= 3; i++ {
		entry, crossed := d.Record("fetch", true, "again", now, 3)
		if entry.Count != i || crossed != (i == 3) {
			t.Fatalf("after reset, failure %d: got count=%d crossed=%v, want count=%d crossed=%v", i, entry.Count, crossed, i, i == 3)
		}
	}
}

// TestDegradedSuccessRemovesEntryFromSnapshot: once every tracked operation
// has succeeded, the snapshot reports nothing degraded.
func TestDegradedSuccessRemovesEntryFromSnapshot(t *testing.T) {
	var d Degraded
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	d.Record("fetch", true, "unavailable", now, 3)
	d.Record("apply", true, "denied", now, 3)
	d.Record("fetch", false, "", now, 3)
	d.Record("apply", false, "", now, 3)
	if got := d.Snapshot(); got != nil {
		t.Fatalf("snapshot after both operations recovered = %+v, want nil", got)
	}
}

func TestDegradedConcurrentRecordsAndSnapshots(t *testing.T) {
	var d Degraded
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				d.Record("operation", true, "err", time.Time{}, 100)
				d.Snapshot()
			}
		})
	}
	wg.Wait()
	if got := d.Snapshot(); len(got) != 1 || got[0].Count != 800 {
		t.Fatalf("snapshot after concurrent records = %+v, want one entry with count 800", got)
	}
}
