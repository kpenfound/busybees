package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/prompts"
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
	if !end.CostKnown {
		t.Errorf("developer ended with a reported cost but CostKnown false: %v", end)
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

// A session that ends without the event that closes its stream carries no
// known cost, and the stream must say so rather than let the field default
// to a confident-looking zero (#371, the live view's half of #359).
func TestSessionEndedEventCarriesWhetherTheCostIsKnown(t *testing.T) {
	t.Setenv("FAKE_SIGNAL", "9")
	h := newHarness(t, strings.Replace(devOnlyTOML, "[scheduler]\n", "[scheduler]\nretries = 0\n", 1))
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
		CreatedAt: time.Now().Add(-time.Hour),
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}

	sub := h.sched.Subscribe()
	h.sched.Once = true
	if err := h.sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}

	events := drain(sub)
	end, ok := find(events, EventSessionEnded, config.RoleDeveloper)
	if !ok {
		t.Fatalf("no developer session-ended event: %v", events)
	}
	if end.CostKnown {
		t.Errorf("a signalled session's cost is reported as known: %v", end)
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

// fallbackTOML makes the developer's first attempt hang until its timeout
// kills it, so the retry runs — and runs on the role's fallback model.
const fallbackTOML = baseTOML + `
retries = 1
retry_delay = "0s"
[roles.developer]
model = "opus"
fallback_model = "haiku"
timeout = "3s"
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

// A session-started event names the model the session runs on and says when
// it is the role's fallback rather than its model: a view drawing what the
// factory is doing right now has no other way to know, because the model is
// resolved per session (the size picks it, a retry overrides it) and a
// running session has reported nothing yet. What the session took is on the
// other end of the pair: turns arrive with session-ended, because an agent
// reports them in the event that ends its stream.
func TestSessionEventsNameTheModelTheFallbackAndTheTurns(t *testing.T) {
	t.Setenv("FAKE_DEV_HANG", "1")
	h := newHarness(t, fallbackTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now().Add(-time.Hour)}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true} // reviewer disabled: the PR is approved without one

	sub := h.sched.Subscribe()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	events := drain(sub)

	var starts []Event
	for _, ev := range events {
		if ev.Kind == EventSessionStarted && ev.Role == config.RoleDeveloper {
			starts = append(starts, ev)
		}
	}
	if len(starts) != 2 {
		t.Fatalf("%d developer session-started events, want the attempt and its retry: %v", len(starts), starts)
	}
	if starts[0].Model != "opus" || starts[0].Fallback {
		t.Errorf("first attempt ran on model %q (fallback %v), want opus and no fallback", starts[0].Model, starts[0].Fallback)
	}
	if starts[1].Model != "haiku" || !starts[1].Fallback {
		t.Errorf("the retry ran on model %q (fallback %v), want haiku and the fallback marked", starts[1].Model, starts[1].Fallback)
	}

	// The first attempt was killed and reported nothing; the retry is the
	// session that has turns to report.
	var done Event
	for _, ev := range events {
		if ev.Kind == EventSessionEnded && ev.Outcome == OutcomePROpened {
			done = ev
		}
	}
	if done.Kind == "" {
		t.Fatalf("no developer session ended with %q: %v", OutcomePROpened, events)
	}
	if done.Turns <= 0 {
		t.Errorf("the finished session reported %d turns: %v", done.Turns, done)
	}
}

// A session-started event names the directory the session runs in, which is
// where its transcript.jsonl is written. That is the one thing a view
// needs to follow a running session and the one thing it cannot work out
// for itself: NewSessionDir stamps a timestamp on the front of the session
// name and a random suffix on the end, so the name alone does not say.
func TestTheSessionStartedEventNamesTheSessionsDirectory(t *testing.T) {
	h := newHarnessAt(t, devOnlyTOML, time.Now())
	events, _ := runEventFixture(t, h, true)

	start, ok := find(events, EventSessionStarted, config.RoleDeveloper)
	if !ok {
		t.Fatalf("no developer session-started event: %v", events)
	}
	if start.Dir == "" {
		t.Fatal("the session-started event names no directory")
	}
	// It is the session's real directory: the prompt the scheduler wrote
	// for that session is in it.
	if _, err := os.Stat(filepath.Join(start.Dir, "prompt.md")); err != nil {
		t.Errorf("the directory the event names is not the session's: %v", err)
	}
	if got := filepath.Base(filepath.Dir(start.Dir)); got != "sessions" {
		t.Errorf("the event names %q, which is not under the sessions directory", start.Dir)
	}
	// A session's directory belongs to that session and to no other.
	seen := map[string]string{}
	for _, ev := range events {
		if ev.Kind != EventSessionStarted {
			continue
		}
		if other, ok := seen[ev.Dir]; ok {
			t.Errorf("sessions %q and %q were both started in %q", other, ev.Session, ev.Dir)
		}
		seen[ev.Dir] = ev.Session
	}
	// Only a started session has one: a stage or poll event is about no
	// session at all, and an ended one is already written to disk.
	for _, ev := range events {
		if ev.Kind != EventSessionStarted && ev.Dir != "" {
			t.Errorf("a %s event names a session directory: %v", ev.Kind, ev)
		}
	}
}

// Both entry points must publish the same ordered lifecycle and identify the
// judge that replaces it, even when one of the concurrent angles fails.
func TestReviewActivityLifecycleAndJudgeHandoff(t *testing.T) {
	for _, requested := range []bool{false, true} {
		for _, failedAngle := range []string{"", "documentation accuracy"} {
			t.Run(fmt.Sprintf("requested=%v/failed=%s", requested, failedAngle), func(t *testing.T) {
				t.Setenv("FAKE_ANGLE_FAIL", failedAngle)
				t.Setenv("FAKE_REVIEW_SIZE", "xl") // five concurrent angles
				cfg := devOnlyTOML
				if requested {
					cfg = reviewOnlyTOML
				}
				h := newHarnessAt(t, cfg, requestedReviewClock)
				issue, pr, id := 1, 201, "reviewer-pr-201-r1"
				if requested {
					pushBranch(t, h.clone, "fix-widget")
					h.gh.prs[42] = personsPR("bees", "bees:review-requested")
					issue, pr, id = 0, 42, "reviewer-requested-pr-42"
				} else {
					seedReady(h, 1, "s", requestedReviewClock.Add(-time.Hour))
					seedCounter(t, h, "review", 1)
				}
				sub := h.sched.Subscribe()
				runPass(t, h)
				events := drain(sub)
				assertReviewLifecycle(t, events, id, issue, pr, 1, 5, true)
				var end, judge = -1, -1
				for i, ev := range events {
					if ev.Kind == EventReviewEnded {
						end = i
					}
					if ev.Kind == EventSessionStarted && ev.Role == config.RoleReviewer {
						judge = i
						if ev.Activity != id || ev.Issue != issue || ev.PR != pr || ev.Round != 1 || ev.Dir == "" {
							t.Fatalf("judge handoff: %+v", ev)
						}
					}
				}
				if judge <= end || end < 0 {
					t.Fatalf("end at %d, judge at %d: %+v", end, judge, events)
				}
			})
		}
	}
}

func assertReviewLifecycle(t *testing.T, events []Event, id string, issue, pr, round, total int, success bool) {
	t.Helper()
	var lifecycle []Event
	for _, ev := range events {
		if ev.Kind == EventReviewStarted || ev.Kind == EventReviewProgress || ev.Kind == EventReviewEnded {
			lifecycle = append(lifecycle, ev)
		}
	}
	want := 2
	if total > 0 {
		want += total + 1 // initial 0/total, then every completion
	}
	if len(lifecycle) != want {
		t.Fatalf("lifecycle has %d events, want %d: %+v", len(lifecycle), want, lifecycle)
	}
	start, end := lifecycle[0], lifecycle[len(lifecycle)-1]
	if start.Kind != EventReviewStarted || start.Phase != "brief" || start.Completed != 0 || start.Total != 0 || start.Started.IsZero() || start.Time.Before(start.Started) {
		t.Fatalf("start: %+v", start)
	}
	if end.Kind != EventReviewEnded || end.Success != success || (!success && end.Err == "") || (success && end.Err != "") {
		t.Fatalf("end: %+v", end)
	}
	for i, ev := range lifecycle {
		if ev.Activity != id || ev.Role != config.RoleReviewer || ev.Issue != issue || ev.PR != pr || ev.Round != round || ev.Started != start.Started || ev.Session != "" || ev.Dir != "" {
			t.Errorf("identity at %d: %+v", i, ev)
		}
		if i > 0 && total > 0 {
			completed := min(i-1, total)
			if ev.Phase != "angles" || ev.Total != total || ev.Completed != completed {
				t.Errorf("progress at %d: %+v, want %d/%d", i, ev, completed, total)
			}
		}
	}
}

func TestReviewActivityFailureAndCancellation(t *testing.T) {
	for _, failure := range []string{"config", "brief", "angles", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			h := newHarness(t, devOnlyTOML)
			pr := github.PR{Number: 42, BaseRefName: "main"}
			h.gh.prs[42] = &pr
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			total := 0
			switch failure {
			case "config":
				if err := os.WriteFile(filepath.Join(h.clone, "context.toml"), []byte("invalid = ["), 0o644); err != nil {
					t.Fatal(err)
				}
			case "brief":
				t.Setenv("FAKE_REVIEW_SIZE", "invalid")
			case "angles":
				t.Setenv("FAKE_ANGLE_FAIL", "all")
				total = 2
			case "cancelled":
				cancel()
			}
			sub := h.sched.Subscribe()
			_, _, err := h.sched.runReview(ctx, h.sched.log, pr, h.clone, "review-42-r3", 7, 3, "")
			if err == nil {
				t.Fatal("pipeline succeeded")
			}
			events := drain(sub)
			assertReviewLifecycle(t, events, "review-42-r3", 7, 42, 3, total, false)
			if count(events, EventSessionStarted) != 0 || count(events, EventSessionEnded) != 0 {
				t.Fatalf("pipeline published factory session events: %+v", events)
			}
		})
	}
}

func TestReviewActivityStartsBeforeGatheringAndNeverBlocks(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	slow, live := h.sched.Subscribe(), h.sched.Subscribe()
	for range eventBuffer {
		h.sched.publish(Event{Kind: EventPoll})
	}
	drain(live)
	seen := false
	h.sched.gh.Exec = func(context.Context, ...string) ([]byte, error) {
		if !seen {
			seen = true
			select {
			case ev := <-live:
				if ev.Kind != EventReviewStarted || ev.Phase != "brief" {
					t.Errorf("before gathering: %+v", ev)
				}
			default:
				t.Error("context gathering began before activity")
			}
		}
		return nil, errors.New("context unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := h.sched.runReview(ctx, h.sched.log, github.PR{Number: 42}, h.clone, "review-42", 0, 1, "")
	if err == nil || !seen || len(slow) != eventBuffer {
		t.Fatalf("err=%v gathered=%v slow buffer=%d", err, seen, len(slow))
	}
	if evs := drain(live); len(evs) != 1 || evs[0].Kind != EventReviewEnded || evs[0].Success {
		t.Fatalf("terminal event: %+v", evs)
	}
}

func TestReviewActivityIsRemovedWhenJudgePreparationFails(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	sub := h.sched.Subscribe()
	_, err := h.sched.runSessionWithRetry(context.Background(), sessionSpec{
		role: config.RoleReviewer, name: "judge", task: "missing-template", workDir: h.clone,
		reviewActivity: "review-42", judge: true,
		data: prompts.Data{PR: &github.PR{Number: 42}, Round: 2},
	})
	if err == nil {
		t.Fatal("judge preparation succeeded")
	}
	events := drain(sub)
	if len(events) != 1 || events[0].Kind != EventReviewEnded || events[0].Activity != "review-42" || events[0].PR != 42 || events[0].Round != 2 || events[0].Success || events[0].Err == "" {
		t.Fatalf("cleanup: %+v", events)
	}
}

// The total comes from the brief's size, role overrides and project switches,
// rather than the full built-in angle list or the count of started callbacks.
func TestReviewActivityCountsEnabledAngles(t *testing.T) {
	t.Setenv("FAKE_REVIEW_SIZE", "m")
	h := newHarness(t, anglesReviewerTOML)
	pr := github.PR{Number: 42, BaseRefName: "main"}
	h.gh.prs[42] = &pr
	if err := os.WriteFile(filepath.Join(h.clone, "context.toml"), []byte("[angles]\ngeneral = false\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := h.sched.Subscribe()
	_, _, err := h.sched.runReview(context.Background(), h.sched.log, pr, h.clone, "review-42-r2", 9, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	assertReviewLifecycle(t, drain(sub), "review-42-r2", 9, 42, 2, 1, true)
}
