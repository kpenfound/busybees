package scheduler

import (
	"context"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/state"
)

// drain empties an event channel without blocking. It is called after Run
// has returned, so everything the pass published is already in the buffer.
func drain(sub <-chan Event) []Event {
	var out []Event
	for {
		select {
		case ev := <-sub:
			out = append(out, ev)
		default:
			return out
		}
	}
}

// find returns the first event of kind that is about role, or false.
func find(events []Event, kind, role string) (Event, bool) {
	for _, ev := range events {
		if ev.Kind == kind && ev.Role == role {
			return ev, true
		}
	}
	return Event{}, false
}

// count returns how many events of kind the slice holds.
func count(events []Event, kind string) int {
	n := 0
	for _, ev := range events {
		if ev.Kind == kind {
			n++
		}
	}
	return n
}

// runEventFixture runs one developer -> reviewer loop over a single ready
// issue and returns the events published and the sessions that ran. With
// subscribe false nobody is listening, so there are no events to return and
// the run is there to be compared against the subscribed one.
func runEventFixture(t *testing.T, h *harness, subscribe bool) ([]Event, []string) {
	t.Helper()
	// The fake reviewer requests changes on its first review and approves
	// afterwards; seeding the counter makes this run the approving one, so
	// the fixture is one round rather than three.
	seedCounter(t, h, "review", 1)
	seedReady(h, 1, "s", h.clock.now().Add(-time.Hour))
	var sub <-chan Event
	if subscribe {
		sub = h.sched.Subscribe()
	}
	if err := h.sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if sub == nil {
		return nil, h.sessionNames()
	}
	return drain(sub), h.sessionNames()
}

// The stream carries a session's start and its end, the stage a developer
// worker moved to, and the end of a full pass. It is a view mechanism, so
// the same fixture must run identically whether or not anyone subscribed:
// the test runs it both ways and compares the sessions (#244).
func TestSchedulerPublishesSessionStageAndPollEvents(t *testing.T) {
	h := newHarnessAt(t, devOnlyTOML, time.Now())
	events, sessions := runEventFixture(t, h, true)

	quiet := newHarnessAt(t, devOnlyTOML, time.Now())
	// The with-no-subscriber half is carried by this comparison, not by
	// counting the quiet run's events: with nobody subscribed there is no
	// channel to read, so a count of zero could never fail. What a publish
	// that changed the pass would break is the pass itself.
	_, quietSessions := runEventFixture(t, quiet, false)
	if len(sessions) == 0 {
		t.Fatalf("no session ran: %v", sessions)
	}
	if got, want := len(quietSessions), len(sessions); got != want {
		t.Fatalf("with a subscriber %v ran, without one %v: publishing changed the pass", sessions, quietSessions)
	}
	for i := range sessions {
		if sessions[i] != quietSessions[i] {
			t.Fatalf("session %d is %q with a subscriber and %q without", i, sessions[i], quietSessions[i])
		}
	}

	start, ok := find(events, EventSessionStarted, config.RoleDeveloper)
	if !ok {
		t.Fatalf("no developer session-started event: %v", events)
	}
	if start.Issue != 1 || start.Session == "" {
		t.Errorf("developer started: issue %d, session %q", start.Issue, start.Session)
	}
	end, ok := find(events, EventSessionEnded, config.RoleDeveloper)
	if !ok {
		t.Fatalf("no developer session-ended event: %v", events)
	}
	if end.Outcome != OutcomePROpened || end.PR != fakePR {
		t.Errorf("developer ended: outcome %q, PR %d; want %q on PR %d", end.Outcome, end.PR, OutcomePROpened, fakePR)
	}
	if end.CostUSD <= 0 {
		t.Errorf("developer ended with no cost: %v", end)
	}
	if _, ok := find(events, EventSessionStarted, config.RoleReviewer); !ok {
		t.Errorf("no reviewer session-started event: %v", events)
	}
	if _, ok := find(events, EventSessionEnded, config.RoleReviewer); !ok {
		t.Errorf("no reviewer session-ended event: %v", events)
	}
	stage, ok := find(events, EventStage, config.RoleDeveloper)
	if !ok {
		t.Fatalf("no stage event: %v", events)
	}
	if stage.Issue != 1 || stage.Stage == "" {
		t.Errorf("stage event: issue %d, stage %q", stage.Issue, stage.Stage)
	}
	// Once mode is exactly one full pass, so exactly one poll event.
	if got := count(events, EventPoll); got != 1 {
		t.Errorf("%d poll events, want 1: %v", got, events)
	}
	// Timestamps come from the injected clock, never time.Now: the harness
	// clock is frozen, so every event carries the same instant (#222).
	for _, ev := range events {
		if !ev.Time.Equal(h.clock.now()) {
			t.Fatalf("%s event is stamped %s, want the injected clock's %s", ev.Kind, ev.Time, h.clock.now())
		}
	}
}

// A subscriber that never reads loses events; it never slows the factory
// down. The pass runs with the buffer already full, so every publish in it
// takes the drop path: a blocking send would deadlock the whole run.
func TestEventsAreDroppedWhenTheSubscriberNeverReads(t *testing.T) {
	h := newHarnessAt(t, devOnlyTOML, time.Now())
	seedCounter(t, h, "review", 1)
	seedReady(h, 1, "s", h.clock.now().Add(-time.Hour))
	sub := h.sched.Subscribe()
	for range eventBuffer {
		h.sched.publish(Event{Kind: EventPoll})
	}

	done := make(chan error, 1)
	go func() { done <- h.sched.Run(context.Background()) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the pass never finished: publishing to a full subscriber blocked instead of dropping")
	}

	if got := len(sub); got != eventBuffer {
		t.Errorf("subscriber holds %d events, want the buffer still full at %d", got, eventBuffer)
	}
	if got := len(h.sessions(config.RoleDeveloper)); got == 0 {
		t.Errorf("no developer session ran while the subscriber was stalled")
	}
}

// publish drops rather than blocks, keeps the events a subscriber has not
// read yet, and gives every subscriber its own copy.
func TestPublishDropsRatherThanBlocks(t *testing.T) {
	at := time.Date(2026, 8, 31, 9, 0, 0, 0, time.UTC)
	s := &Scheduler{now: func() time.Time { return at }}
	// Publishing with nobody subscribed is a no-op, not a panic.
	s.publish(Event{Kind: EventPoll})

	first, second := s.Subscribe(), s.Subscribe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := range eventBuffer + 10 {
			s.publish(Event{Kind: EventSessionStarted, Issue: i})
		}
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("publish blocked on a subscriber that is not reading")
	}

	for name, sub := range map[string]<-chan Event{"first": first, "second": second} {
		got := drain(sub)
		if len(got) != eventBuffer {
			t.Fatalf("%s subscriber got %d events, want %d", name, len(got), eventBuffer)
		}
		// The buffer keeps what arrived first and drops the overflow.
		if got[0].Issue != 0 || got[len(got)-1].Issue != eventBuffer-1 {
			t.Errorf("%s subscriber kept issues %d..%d, want 0..%d", name, got[0].Issue, got[len(got)-1].Issue, eventBuffer-1)
		}
		if !got[0].Time.Equal(at) {
			t.Errorf("%s subscriber: event stamped %s, want the scheduler's clock %s", name, got[0].Time, at)
		}
	}
}

// A view re-reads status.json when a poll event arrives, so the file must
// already hold what the pass found when the event is published. The first
// pass of an idle factory writes status.json nowhere else, so an event
// published before writeStatus finds no file at all (#244).
func TestPollEventArrivesAfterStatusIsWritten(t *testing.T) {
	h := newHarnessAt(t, devOnlyTOML, time.Now())
	sub := h.sched.Subscribe()
	seen := make(chan state.Status, 1)
	go func() {
		for ev := range sub {
			if ev.Kind != EventPoll {
				continue
			}
			st, err := h.store.LoadStatus()
			if err != nil {
				t.Errorf("status.json: %v", err)
			}
			seen <- st
			return
		}
	}()
	if err := h.sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case st := <-seen:
		if !st.LastPoll.Equal(h.clock.now()) {
			t.Errorf("when the poll event arrived status.json said last_poll %s, want this pass's %s: the event was published before writeStatus",
				st.LastPoll, h.clock.now())
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no poll event")
	}
}
