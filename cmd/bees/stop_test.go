package main

import (
	"os"
	"syscall"
	"testing"
	"time"
)

// The hard stop fires on the second signal, never the first: the first is
// the one signal.NotifyContext consumed to start the cool-down, and every
// registered channel sees it too.
func TestHardStopFiresOnTheSecondSignalOnly(t *testing.T) {
	ch := make(chan os.Signal, 2)
	stopped := make(chan struct{})
	go hardStopAfterSecond(ch, func() { close(stopped) })

	ch <- syscall.SIGINT
	select {
	case <-stopped:
		t.Fatal("the first signal hard-stopped the factory: it is the cool-down, not the hard stop")
	case <-time.After(50 * time.Millisecond):
	}
	ch <- syscall.SIGINT
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the second signal did not hard-stop the factory")
	}
}

// A removed handler (the channel closed after Run returned) stops nothing.
func TestARemovedHandlerNeverHardStops(t *testing.T) {
	ch := make(chan os.Signal, 2)
	stopped := make(chan struct{})
	done := make(chan struct{})
	go func() {
		hardStopAfterSecond(ch, func() { close(stopped) })
		close(done)
	}()
	ch <- syscall.SIGINT
	close(ch)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the watch did not end when its channel closed")
	}
	select {
	case <-stopped:
		t.Fatal("a closed channel hard-stopped the factory")
	default:
	}
}
