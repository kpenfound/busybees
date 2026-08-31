package scheduler

import "time"

// Event kinds published on the scheduler's event stream.
const (
	// EventSessionStarted is emitted just before a claude session is
	// executed, once per attempt (a retry is its own session).
	EventSessionStarted = "session-started"
	// EventSessionEnded is emitted when that session has finished, whether
	// it reported an outcome, failed or could not be run at all.
	EventSessionEnded = "session-ended"
	// EventStage is emitted when a developer worker moves an issue to
	// another stage of the develop -> prereview -> review -> checks loop.
	EventStage = "stage"
	// EventPoll is emitted at the end of every full pass, after status.json
	// has been rewritten, so a view may re-read it when one arrives.
	EventPoll = "poll"
)

// Event is one thing the factory did, in the shape a view needs to render
// it: who did it, what it was about, when, and — for a finished session —
// how it ended and what it cost.
//
// The stream is a view mechanism and nothing else. No scheduler decision
// depends on whether anyone is subscribed, and an event a subscriber has no
// room for is dropped rather than waited on, so a stalled view can never
// slow a pass down. Views that need the whole queue state read status.json,
// which is still written after every pass: the event stream says when
// something happened, status.json says what the factory currently looks
// like.
type Event struct {
	// Kind is one of the Event* constants above.
	Kind string
	// Time is the scheduler's clock when the event was published.
	Time time.Time
	// Role is the role of the session an event is about; empty for a poll.
	// A stage event carries the developer role, since a stage belongs to a
	// developer worker.
	Role string
	// Session is the session name (the directory under sessions/), empty
	// for stage and poll events.
	Session string
	// Issue and PR are what the event is about; zero when it is about
	// neither (a singleton session, a poll).
	Issue int
	PR    int
	// Stage is the developer worker's stage on a stage event, empty
	// otherwise.
	Stage string
	// Round is the review round a stage event belongs to.
	Round int
	// Model is the claude model the session runs with and Fallback marks a
	// session running on the role's fallback model
	// (scheduler.retry_with_fallback). Both are set on session-started
	// only: a view renders them for a session that is still running, and
	// the model a finished session used is in the ledger.
	Model    string
	Fallback bool
	// Outcome and Note are what a finished session reported, or the
	// synthetic "failed" of a session that reported nothing.
	Outcome string
	Note    string
	// Turns, CostUSD and Duration are what a finished session took, cost
	// and how long it ran. claude reports all three in the final event of
	// its stream, so they arrive with session-ended and never before it.
	Turns    int
	CostUSD  float64
	Duration time.Duration
	// Err is set on a poll that failed and on a session that could not be
	// run at all.
	Err string
}

// eventBuffer is how far behind a subscriber may fall before its events
// start being dropped. It is generous enough that a view redrawing at any
// human rate never loses one, and small enough that a view that has stopped
// reading entirely costs a bounded amount of memory.
const eventBuffer = 64

// Subscribe returns a channel of events. The channel is buffered: an event
// that does not fit is dropped, so a subscriber that stops reading slows
// nothing down and loses events instead. It is never closed — a subscriber
// lives as long as the scheduler does — and callers must not assume they
// are the only one.
func (s *Scheduler) Subscribe() <-chan Event {
	ch := make(chan Event, eventBuffer)
	s.evMu.Lock()
	s.subs = append(s.subs, ch)
	s.evMu.Unlock()
	return ch
}

// publish sends ev to every subscriber, stamping it with the scheduler's
// clock (never time.Now, so two runs of the same fixture produce the same
// events). It never blocks: a subscriber whose buffer is full is skipped.
func (s *Scheduler) publish(ev Event) {
	ev.Time = s.now()
	s.evMu.Lock()
	subs := s.subs
	s.evMu.Unlock()
	// The slice is only ever appended to, so iterating the snapshot taken
	// under the lock is safe while another goroutine subscribes.
	for _, ch := range subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// sessionEvent builds the event for a session, filled in with the issue and
// pull request it is about.
func sessionEvent(kind string, spec sessionSpec) Event {
	ev := Event{Kind: kind, Role: spec.role, Session: spec.name}
	if spec.data.Issue != nil {
		ev.Issue = spec.data.Issue.Number
	}
	if spec.data.PR != nil {
		ev.PR = spec.data.PR.Number
	}
	return ev
}
