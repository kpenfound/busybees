package ops

import (
	"context"
	"sync"
	"testing"
	"time"
)

func TestWakeBurstsCoalesceAndLoopsAreIndependent(t *testing.T) {
	a, b := NewWake(), NewWake()
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				a.Signal()
			}
		})
	}
	wg.Wait()
	if len(a) != 1 || len(b) != 0 {
		t.Fatalf("pending signals = (a=%d, b=%d), want (1, 0): signals coalesce and stay local to their own loop", len(a), len(b))
	}
	tick := make(chan time.Time, 1)
	passes := 0
	if got := a.Wait(context.Background(), tick, func() { passes++; tick <- time.Time{} }); !got || passes != 1 {
		t.Fatalf("Wait returned %v with %d local passes, want true with 1", got, passes)
	}
	if len(a) != 0 {
		t.Fatalf("pending signals on a = %d, want 0 (consumed by the pass)", len(a))
	}
}

func TestTickSupersedesWakeAndCancellationStops(t *testing.T) {
	for range 100 {
		w := NewWake()
		w.Signal()
		tick := make(chan time.Time, 1)
		tick <- time.Time{}
		if got, pending := w.Wait(context.Background(), tick, func() { t.Error("redundant local pass") }), len(w); !got || pending != 0 {
			t.Fatalf("Wait returned %v with %d wake still pending, want true with 0: a ready tick must consume a pending wake", got, pending)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := NewWake()
	w.Signal()
	if got := w.Wait(ctx, make(chan time.Time), func() { t.Error("reconciled after cancellation") }); got {
		t.Fatalf("Wait on a cancelled context = %v, want false", got)
	}
	if err := Sleep(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("Sleep on a cancelled context = %v, want %v", err, context.Canceled)
	}
}

func TestCompletionDuringLocalPassWakesAgain(t *testing.T) {
	w := NewWake()
	tick := make(chan time.Time, 1)
	w.Signal()
	passes := 0
	if got := w.Wait(context.Background(), tick, func() {
		passes++
		if passes == 1 {
			w.Signal()
		} else {
			tick <- time.Time{}
		}
	}); !got || passes != 2 {
		t.Fatalf("Wait returned %v with %d local passes, want true with 2: a signal raised during a pass must not be lost", got, passes)
	}
}

func TestWaitingLoopsWakePromptlyAndCancel(t *testing.T) {
	a, b := NewWake(), NewWake()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	seen := make(chan string, 2)
	done := make(chan bool, 2)
	go func() { done <- a.Wait(ctx, make(chan time.Time), func() { seen <- "a" }) }()
	go func() { done <- b.Wait(ctx, make(chan time.Time), func() { seen <- "b" }) }()
	b.Signal()
	select {
	case name := <-seen:
		if name != "b" {
			t.Fatalf("loop woken by b's signal = %q, want %q", name, "b")
		}
	case <-time.After(time.Second):
		t.Fatal("neither loop ran its local pass within 1s of b.Signal(); want b woken promptly, without waiting for a tick")
	}
	cancel()
	for range 2 {
		select {
		case continued := <-done:
			if continued {
				t.Fatalf("Wait returned %v after cancellation, want false", continued)
			}
		case <-time.After(time.Second):
			t.Fatal("a loop did not return within 1s of cancellation")
		}
	}
}
