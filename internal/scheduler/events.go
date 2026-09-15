package scheduler

import (
	"github.com/kpenfound/busybees/core/ops"
)

// Event kinds published on the scheduler's event stream.
const (
	// EventSessionStarted is emitted just before a session is executed,
	// once per attempt (a retry is its own session).
	EventSessionStarted = "session-started"
	// EventSessionEnded is emitted when that session has finished, whether
	// it reported an outcome, failed or could not be run at all.
	EventSessionEnded = "session-ended"
	// Review events describe a synthetic activity, not a factory session.
	// A successful end waits for the judge's session-started handoff; a
	// failed end removes the activity immediately.
	EventReviewStarted  = "review-started"
	EventReviewProgress = "review-progress"
	EventReviewEnded    = "review-ended"
	// EventStage is emitted when a developer worker moves an issue to
	// another stage of the develop -> fan-out -> assembler -> prereview ->
	// review -> stack-wait -> checks loop.
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
type Event = ops.Event

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
func (s *Scheduler) Subscribe() <-chan Event { return s.events.Subscribe() }

func (s *Scheduler) publish(ev Event) { s.events.Publish(ev) }

// sessionEvent builds the event for a session, filled in with the issue and
// pull request it is about.
func sessionEvent(kind string, spec sessionSpec) Event {
	ev := Event{Kind: kind, Role: spec.role, Session: spec.name, Activity: spec.reviewActivity, Round: spec.data.Round}
	ev.Work = sessionWork(spec)
	return ev
}
