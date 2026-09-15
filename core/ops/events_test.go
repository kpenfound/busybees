package ops

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/work"
)

func TestEventBusDropsAndUsesInjectedClock(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	bus := NewBus(2, func() time.Time { return now })
	slow, live := bus.Subscribe(), bus.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 100 {
			bus.Publish(Event{Kind: "custom", Work: work.Ref{Key: "task/arbitrary", Tags: map[string]string{"lane": "blue"}}})
		}
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("producer blocked")
	}
	if len(slow) != 2 || len(live) != 2 {
		t.Fatal("subscriber buffer is not bounded")
	}
	a, b := <-slow, <-live
	if a.Time != now || a.Kind != "custom" || a.Work.Key != "task/arbitrary" {
		t.Fatalf("event=%+v", a)
	}
	a.Work.Tags["lane"] = "red"
	if b.Work.Tags["lane"] != "blue" {
		t.Fatal("subscriber metadata aliased")
	}
	<-live
	bus.Publish(Event{Kind: "next"})
	if got := <-live; got.Kind != "next" {
		t.Fatal("slow subscriber blocked live one")
	}
}

func TestEventBusConcurrentSubscribeAndPublish(t *testing.T) {
	bus := NewBus(64, func() time.Time { return time.Time{} })
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				bus.Publish(Event{Work: work.Ref{Key: "task"}})
			}
		})
		wg.Go(func() {
			for range 10 {
				bus.Subscribe()
			}
		})
	}
	wg.Wait()
	// All producers have completed with every subscriber left unread.
}

// publish drops rather than blocks, keeps the events a subscriber has not
// read yet, and gives every subscriber its own copy.
func TestPublishDropsRatherThanBlocks(t *testing.T) {
	at := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	const eventBuffer = 64
	s := NewBus(eventBuffer, func() time.Time { return at })
	// Publishing with nobody subscribed is a no-op, not a panic.
	s.Publish(Event{Kind: "poll"})

	first, second := s.Subscribe(), s.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range eventBuffer + 10 {
			s.Publish(Event{Kind: "started", Work: work.Ref{Key: work.Key(fmt.Sprint(i))}})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publish blocked on a subscriber that is not reading")
	}

	for name, sub := range map[string]<-chan Event{"first": first, "second": second} {
		var got []Event
		for len(sub) > 0 {
			got = append(got, <-sub)
		}
		if len(got) != eventBuffer {
			t.Fatalf("%s subscriber got %d events, want %d", name, len(got), eventBuffer)
		}
		// The buffer keeps what arrived first and drops the overflow.
		if got[0].Work.Key != "0" || got[len(got)-1].Work.Key != work.Key(fmt.Sprint(eventBuffer-1)) {
			t.Errorf("%s subscriber kept work %s..%s, want 0..%d", name, got[0].Work.Key, got[len(got)-1].Work.Key, eventBuffer-1)
		}
		if !got[0].Time.Equal(at) {
			t.Errorf("%s subscriber: event stamped %s, want the scheduler's clock %s", name, got[0].Time, at)
		}
	}
}
