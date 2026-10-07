package testutil

import (
	"testing"
	"time"
)

// WaitFor polls cond every 5ms until it reports true or d elapses, when it
// fails the test naming what it was waiting for. Use it in place of a fixed
// sleep when a test must wait for background work, such as a goroutine or a
// started process reaching a state, rather than a wall-clock guess.
func WaitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", d, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
