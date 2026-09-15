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
		t.Fatal("signals were not local and coalesced")
	}
	tick := make(chan time.Time, 1)
	passes := 0
	if !a.Wait(context.Background(), tick, func() { passes++; tick <- time.Time{} }) || passes != 1 {
		t.Fatalf("local passes=%d", passes)
	}
	if len(a) != 0 {
		t.Fatal("wake still pending")
	}
}

func TestTickSupersedesWakeAndCancellationStops(t *testing.T) {
	for range 100 {
		w := NewWake()
		w.Signal()
		tick := make(chan time.Time, 1)
		tick <- time.Time{}
		if !w.Wait(context.Background(), tick, func() { t.Error("redundant local pass") }) || len(w) != 0 {
			t.Fatal("tick did not consume wake")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := NewWake()
	w.Signal()
	if w.Wait(ctx, make(chan time.Time), func() { t.Error("reconciled after cancellation") }) {
		t.Fatal("cancelled wait continued")
	}
	if err := Sleep(ctx, time.Hour); err != context.Canceled {
		t.Fatalf("sleep=%v", err)
	}
}

func TestCompletionDuringLocalPassWakesAgain(t *testing.T) {
	w := NewWake()
	tick := make(chan time.Time, 1)
	w.Signal()
	passes := 0
	if !w.Wait(context.Background(), tick, func() {
		passes++
		if passes == 1 {
			w.Signal()
		} else {
			tick <- time.Time{}
		}
	}) || passes != 2 {
		t.Fatalf("lost completion signal, passes=%d", passes)
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
			t.Fatal("wrong loop woke")
		}
	case <-time.After(time.Second):
		t.Fatal("wake delayed until tick")
	}
	cancel()
	for range 2 {
		select {
		case continued := <-done:
			if continued {
				t.Fatal("loop continued")
			}
		case <-time.After(time.Second):
			t.Fatal("loop did not stop")
		}
	}
}
