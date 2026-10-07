package testutil

import (
	"sync/atomic"
	"testing"
	"time"
)

// WaitFor returns as soon as a condition set by background work turns true,
// rather than waiting out its full deadline.
func TestWaitForReturnsOnceTheConditionIsTrue(t *testing.T) {
	var ready atomic.Bool
	go func() {
		time.Sleep(10 * time.Millisecond)
		ready.Store(true)
	}()
	start := time.Now()
	WaitFor(t, time.Second, "the flag to be set", ready.Load)
	if elapsed := time.Since(start); elapsed >= time.Second {
		t.Fatalf("WaitFor took %s, as long as its deadline; it should have returned once the flag was set", elapsed)
	}
}

// WaitFor returns at once, with no extra polling, when the condition is
// already true.
func TestWaitForReturnsAtOnceWhenAlreadyTrue(t *testing.T) {
	var calls int
	WaitFor(t, time.Second, "an always-true condition", func() bool {
		calls++
		return true
	})
	if calls != 1 {
		t.Fatalf("cond was polled %d times, want exactly 1", calls)
	}
}
