package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/session"
)

// cachedIssue is the copy of an issue in the lists kept from the last poll,
// which is what every local pass classifies.
func cachedIssue(t *testing.T, h *harness, n int) (github.Issue, bool) {
	t.Helper()
	h.sched.mu.Lock()
	defer h.sched.mu.Unlock()
	for _, i := range h.sched.lastIssues {
		if i.Number == n {
			return i, true
		}
	}
	return github.Issue{}, false
}

// The issues a session changed through the MCP server are read back into the
// cached poll when it ends: a relabelled one replaces the copy the poll took,
// and one the session created — which no poll has seen — is appended.
func TestTheIssuesASessionChangedAreRefreshedIntoTheCache(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, noRolesTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Spec me", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	if _, err := h.sched.poll(ctx); err != nil {
		t.Fatal(err)
	}
	// What the session did on GitHub, which the cache knows nothing about.
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "A sub-issue it filed", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	dir := t.TempDir()
	for _, n := range []int{1, 2} {
		if err := session.RecordTouched(dir, n); err != nil {
			t.Fatal(err)
		}
	}

	h.sched.refreshTouched(ctx, dir)

	got, ok := cachedIssue(t, h, 1)
	if !ok {
		t.Fatal("issue 1 fell out of the cache")
	}
	if state := h.sched.stateOf(got.Labels); state != "ready" {
		t.Fatalf("cached issue 1 is %q, want ready: the session's label move must reach the cache", state)
	}
	got, ok = cachedIssue(t, h, 2)
	if !ok {
		t.Fatal("the issue the session created is not in the cache: a local pass would not see it before the next poll")
	}
	if state := h.sched.stateOf(got.Labels); state != "triage" {
		t.Fatalf("cached issue 2 is %q, want triage", state)
	}
}

// The cache holds what a poll would have returned, so what a poll would not
// return is dropped: an issue that has been closed, and one that does not
// match the factory's filter.
func TestARefreshDropsWhatAPollWouldNotReturn(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, noRolesTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Closed since", State: "CLOSED",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "Not the factory's", State: "OPEN",
		Labels: []github.Label{{Name: "bees:ready"}}, CreatedAt: time.Now()}
	if _, err := h.sched.poll(ctx); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for _, n := range []int{1, 2} {
		if err := session.RecordTouched(dir, n); err != nil {
			t.Fatal(err)
		}
	}

	h.sched.refreshTouched(ctx, dir)

	if _, ok := cachedIssue(t, h, 1); ok {
		t.Fatal("a closed issue was written into the cache; a poll would not have returned it")
	}
	if _, ok := cachedIssue(t, h, 2); ok {
		t.Fatal("an issue outside the factory's filter was written into the cache")
	}
}

// A session that changed nothing costs nothing: the refresh reads one file
// from the session directory and makes no GitHub call at all.
func TestASessionThatTouchedNoIssueCostsNoCall(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	before := h.gh.total()

	h.sched.refreshTouched(context.Background(), t.TempDir())

	if got := h.gh.total(); got != before {
		t.Fatalf("the refresh made %d gh calls for a session that touched nothing, want 0", got-before)
	}
}

// touchedTOML runs the project manager and the developer, and polls once an
// hour with the harness's clock frozen: a second session can only come from
// the wake a finished session signals, never from a second poll.
const touchedTOML = `
version = 1
[project]
repo = "acme/widgets"
[scheduler]
poll_interval = "1h"
max_developers = 1
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
`

// The project manager moves a work item to bees:ready with issue_set_state,
// and the developer starts on the local pass that follows its session rather
// than waiting out the poll interval — an hour here, and up to
// off_hours_poll_interval in a real factory.
func TestAnIssueTriagedByASessionIsDispatchedOnTheWake(t *testing.T) {
	t.Setenv("FAKE_TRIAGE", "3")
	now := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, touchedTOML, now)
	seedIssue(h, 3, "bees:triage", "s", now.Add(-24*time.Hour))
	h.gh.hidden[203] = true

	stop := runLoop(t, h)
	defer stop()

	waitFor(t, 30*time.Second, "the developer to start on the issue the project manager triaged", func() bool {
		return len(h.sessions(config.RoleDeveloper)) == 1
	})
	if got := polls(h); got != 1 {
		t.Fatalf("the scheduler polled %d times, want 1: the triaged issue must go out on a wake, not on a second poll", got)
	}
	if got := h.clock.now(); !got.Equal(now) {
		t.Fatalf("the clock moved to %s: the dispatch must not need a poll interval to elapse", got)
	}
	if dirs := h.sessions(config.RoleDeveloper); !strings.Contains(dirs[0], "issue-3") {
		t.Fatalf("the developer session is %q, want one for issue 3", dirs[0])
	}
}

// An issue a session filed with issue_create is triage work on the very next
// local pass. Without the refresh a feature breakdown costs two polls before
// anybody looks at the work items it produced.
func TestAnIssueASessionFiledIsTriageWorkOnTheNextLocalPass(t *testing.T) {
	t.Setenv("FAKE_FILE_ISSUE", "7")
	now := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, pmOnlyTOML, now) // the product manager alone: one session in the pass
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "An idea", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feedback"}}, CreatedAt: now.Add(-24 * time.Hour)}

	runPass(t, h)

	if got := triageQueue(h); got != 0 {
		t.Fatalf("the triage queue is %d after the poll, want 0: the issue is filed by the session, not before it", got)
	}
	h.sched.localPass(context.Background())
	if got := triageQueue(h); got != 1 {
		t.Fatalf("the triage queue is %d on the local pass after the session, want 1", got)
	}
	if got := polls(h); got != 1 {
		t.Fatalf("the scheduler polled %d times, want 1: the new issue must be seen without a poll", got)
	}
}

// triageQueue is the triage count the scheduler last classified, which is
// what `bees status` shows and what dispatches the project manager.
func triageQueue(h *harness) int {
	h.sched.mu.Lock()
	defer h.sched.mu.Unlock()
	return h.sched.queues["triage"]
}

// The refresh replaces the cached list rather than writing into it. A pass
// takes the list under s.mu and then classifies it without the lock, while
// refreshTouched runs on the goroutine of the session that ended (sessions.go),
// so writing an element in place is a write to a slice another goroutine is
// reading. `go test -race` reports that; dagger check runs no -race, so this
// pins it by the value a pass in flight would see.
func TestARefreshDoesNotWriteIntoTheListAPassIsClassifying(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t, noRolesTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Spec me", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	if _, err := h.sched.poll(ctx); err != nil {
		t.Fatal(err)
	}
	// The header a pass in flight is holding while it classifies.
	h.sched.mu.Lock()
	inFlight := h.sched.lastIssues
	h.sched.mu.Unlock()

	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}
	dir := t.TempDir()
	if err := session.RecordTouched(dir, 1); err != nil {
		t.Fatal(err)
	}
	h.sched.refreshTouched(ctx, dir)

	if state := h.sched.stateOf(inFlight[0].Labels); state != "triage" {
		t.Fatalf("the list a pass is classifying became %q: the refresh wrote into it from another goroutine", state)
	}
	got, ok := cachedIssue(t, h, 1)
	if !ok || h.sched.stateOf(got.Labels) != "ready" {
		t.Fatalf("the refresh did not reach the cache: %v %q", ok, h.sched.stateOf(got.Labels))
	}
}
