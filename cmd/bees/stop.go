package main

import (
	"os"
	"os/signal"
	"syscall"
)

// hardStopOnSecondInterrupt makes a second Ctrl-C (or SIGTERM) stop the
// factory's running sessions. The first interrupt is consumed by main's
// signal.NotifyContext, which cancels the command context — the cool-down:
// polling stops and the work in flight finishes — and a context can only be
// cancelled once, so without this a second interrupt would do nothing at all
// and a person who wants out would be stuck waiting on a long session.
//
// It watches the same signals on a channel of its own: the runtime delivers
// every signal to every registered channel, so the first receive is the
// interrupt that started the cool-down and the second is the one that means
// "stop them now". The returned function removes the handler.
func hardStopOnSecondInterrupt(stop func()) func() {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go hardStopAfterSecond(ch, stop)
	return func() {
		signal.Stop(ch)
		close(ch)
	}
}

// hardStopAfterSecond calls stop once ch has delivered two signals. A closed
// channel ends the watch without stopping anything.
func hardStopAfterSecond(ch <-chan os.Signal, stop func()) {
	for i := 0; i < 2; i++ {
		if _, ok := <-ch; !ok {
			return
		}
	}
	stop()
}
