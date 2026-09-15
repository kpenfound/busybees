package ops

import (
	"sync"
	"testing"
	"time"
)

func TestDegradedCrossesOnceAndSnapshotsAreIndependent(t *testing.T) {
	var d Degraded
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	for i := 1; i <= 5; i++ {
		entry, crossed := d.Record("fetch", true, "unavailable", now.Add(time.Duration(i)*time.Second), 3)
		if entry.Count != i || crossed != (i == 3) || entry.Escalated != (i >= 3) {
			t.Fatalf("failure %d: %+v crossed=%v", i, entry, crossed)
		}
		if !entry.First.Equal(now.Add(time.Second)) || !entry.Last.Equal(now.Add(time.Duration(i)*time.Second)) {
			t.Fatal("streak clocks changed")
		}
	}
	d.Record("apply", true, "denied", now, 3)
	snap := d.Snapshot()
	if len(snap) != 2 || snap[0].Op != "apply" || snap[1].Op != "fetch" {
		t.Fatalf("snapshot=%+v", snap)
	}
	snap[1].Count = 99
	if d.Snapshot()[1].Count != 5 {
		t.Fatal("snapshot aliases state")
	}
	d.Record("fetch", false, "", now, 3)
	for i := 1; i <= 3; i++ {
		entry, crossed := d.Record("fetch", true, "again", now, 3)
		if entry.Count != i || crossed != (i == 3) {
			t.Fatal("success did not reset streak")
		}
	}
	d.Record("fetch", false, "", now, 3)
	d.Record("apply", false, "", now, 3)
	if d.Snapshot() != nil {
		t.Fatal("success retained degraded entries")
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
		t.Fatalf("lost records: %+v", got)
	}
}
