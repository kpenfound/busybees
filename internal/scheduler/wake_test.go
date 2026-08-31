package scheduler

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
)

// wakeTOML polls once an hour and runs one developer at a time: with the
// harness's fixed clock frozen, nothing but a wake can produce a second
// pass, so any session after the first one proves the wake channel did it.
const wakeTOML = `
version = 1
[project]
repo = "acme/widgets"
[scheduler]
poll_interval = "1h"
max_developers = 1
max_review_rounds = 3
`

// runLoop starts the scheduler's real loop (Once off) in the background and
// returns the function that stops it and waits for it to finish. Tests about
// the wake need it: Once breaks out of Run before the wait, which is where
// the wake lives.
func runLoop(t *testing.T, h *harness) func() {
	t.Helper()
	h.sched.Once = false
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.sched.Run(ctx) }()
	return func() {
		cancel()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("scheduler: %v", err)
			}
		case <-time.After(2 * time.Minute):
			t.Fatal("the scheduler did not stop")
		}
	}
}

// polls counts the GitHub polls the scheduler made. A pass starts with the
// only issue list that asks for open issues: the visibility backstop after
// every session lists --state all, and nothing else lists issues at all.
func polls(h *harness) int {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	n := 0
	for _, c := range h.gh.calls {
		if len(c) < 2 || c[0] != "issue" || c[1] != "list" {
			continue
		}
		for i, a := range c {
			if a == "--state" && i+1 < len(c) && c[i+1] == "open" {
				n++
			}
		}
	}
	return n
}

// idle reports that no developer worker is running, so a test can stop the
// loop without killing a session.
func idle(h *harness) bool {
	h.sched.mu.Lock()
	defer h.sched.mu.Unlock()
	return len(h.sched.owned) == 0
}

// waitFor blocks until cond holds, and fails the test if it does not within
// d. what names the thing waited for, so a timeout is readable.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
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

// A developer slot freed by a finished worker is filled at once rather than
// at the next tick: the worker signals the wake channel when it returns its
// slot, and the local pass that follows dispatches the next ready issue. The
// clock never moves and GitHub is polled once, so the second session cannot
// have come from a second poll.
func TestAFreedDeveloperSlotIsFilledOnTheWake(t *testing.T) {
	now := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, wakeTOML+rolesOffTOML, now)
	base := now.Add(-24 * time.Hour)
	seedReady(h, 1, "s", base)
	seedReady(h, 2, "s", base.Add(time.Hour))

	stop := runLoop(t, h)
	defer stop()

	waitFor(t, time.Minute, "both ready issues to be worked", func() bool {
		return len(h.sessions(config.RoleDeveloper)) == 2 && idle(h)
	})
	if got := polls(h); got != 1 {
		t.Fatalf("the scheduler polled %d times, want 1: the second issue must go out on a wake, not on a second poll", got)
	}
	if got := h.clock.now(); !got.Equal(now) {
		t.Fatalf("the clock moved to %s: the wake must not need a poll interval to elapse", got)
	}
}

// Mail written to the mailbox by a running session — a different process
// appending to the same directory, which no in-process channel can see —
// reaches its recipient when that session finishes: the wake runs a local
// pass, which re-reads the mailbox from disk. Without it the project manager
// would sit idle for the rest of the poll interval.
func TestMailFromAFinishedSessionStartsItsRecipientOnTheWake(t *testing.T) {
	t.Setenv("FAKE_DEV_MAIL_TO", config.RoleProjectManager)
	now := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, wakeTOML+`
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
`, now)
	// Nothing in triage: the project manager has no work of its own, so the
	// only thing that can start it is the developer's mail.
	seedReady(h, 1, "s", now.Add(-24*time.Hour))

	stop := runLoop(t, h)
	defer stop()

	waitFor(t, time.Minute, "the project manager to answer the developer's mail", func() bool {
		unread, err := h.box.List(mail.Filter{To: config.RoleProjectManager, UnreadOnly: true})
		return err == nil && len(h.sessions(config.RoleProjectManager)) == 1 && len(unread) == 0
	})
	if got := polls(h); got != 1 {
		t.Fatalf("the scheduler polled %d times, want 1: the mail must be picked up on a wake, not on a second poll", got)
	}
	unread, err := h.box.List(mail.Filter{To: config.RoleProjectManager, UnreadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(unread) != 0 {
		t.Fatalf("%d messages still unread: the session woken by the wake must consume them", len(unread))
	}
}

// A burst of signals costs the loop one local pass, not one per signal: the
// wake channel holds a single slot and signal() drops what does not fit,
// because the pass it asks for has not run yet and will see everything the
// dropped signals would have.
//
// The fixture makes every pass countable: relabelling the unlabelled issue
// fails, so reconcile retries it (and only it) on every pass.
func TestABurstOfSignalsCostsOneLocalPass(t *testing.T) {
	now := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, wakeTOML+rolesOffTOML, now)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "An idea", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}}, CreatedAt: now.Add(-24 * time.Hour)}
	h.gh.errFor["issue edit"] = errors.New("boom")

	runPass(t, h)
	before := h.gh.callCount("issue edit")
	if before != 1 {
		t.Fatalf("the full pass made %d label edits, want 1: the fixture cannot count passes", before)
	}

	// Signalled before anything is waiting on the channel, so the burst is
	// complete before the loop can consume any of it.
	signalled := make(chan struct{})
	go func() {
		for i := 0; i < 10; i++ {
			h.sched.signal()
		}
		close(signalled)
	}()
	select {
	case <-signalled:
	case <-time.After(10 * time.Second):
		t.Fatal("signal() blocked: a burst of finished sessions must never hold up the factory")
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	waited := make(chan bool, 1)
	go func() { waited <- h.sched.waitForTick(ctx) }()
	waitFor(t, time.Minute, "the local pass the wake asked for", func() bool {
		return h.gh.callCount("issue edit") > before
	})
	// Ten passes would all have run by now; the poll timer is an hour away.
	time.Sleep(200 * time.Millisecond)
	if got := h.gh.callCount("issue edit") - before; got != 1 {
		t.Fatalf("ten signals ran %d local passes, want 1", got)
	}
	cancel()
	if <-waited {
		t.Fatal("waitForTick reported a tick was due; it was cancelled")
	}
}

// A session that writes mail in the middle of a developer worker's
// develop → review loop does not make its recipient wait for the whole
// worker: runSession signals when the session itself finishes, not only when
// the worker returns its slot. The pre-review checks wait holds the worker
// open long enough for the assertion to be about that and nothing else.
func TestMailSentMidWorkerReachesItsRecipientBeforeTheWorkerEnds(t *testing.T) {
	t.Setenv("FAKE_DEV_MAIL_TO", config.RoleProjectManager)
	now := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, wakeTOML+`
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.reviewer]
checks_wait = "2s"
`, now)
	seedReady(h, 1, "s", now.Add(-24*time.Hour))

	stop := runLoop(t, h)
	defer stop()

	waitFor(t, time.Minute, "the project manager to be started by the developer's mail", func() bool {
		return len(h.sessions(config.RoleProjectManager)) == 1
	})
	if idle(h) {
		t.Fatal("the project manager only ran once the developer worker had finished: mail must not wait for the whole review loop")
	}
}

// The two places the scheduler sends mail itself signal the wake, so the
// developer session that answers a conflict notice or a person's review
// comment starts on the pass that follows rather than at the next tick.
func TestTheSchedulersOwnMailSignalsTheWake(t *testing.T) {
	ctx := context.Background()

	t.Run("conflict notice", func(t *testing.T) {
		h := newHarness(t, devOnlyTOML)
		seedApprovedPR(t, h, github.MergeableConflicting, "DIRTY", "aaa")

		checkOnce(t, h)

		if got := len(h.sched.wake); got != 1 {
			t.Fatalf("%d wakes pending after the conflict notice, want 1", got)
		}
	})

	t.Run("human feedback", func(t *testing.T) {
		h := newHarness(t, devOnlyTOML)
		seedApprovedPR(t, h, "MERGEABLE", "CLEAN", "aaa")
		h.gh.prs[fakePR].UpdatedAt = time.Now()
		when := time.Now().UTC().Format(time.RFC3339)
		h.gh.activity["repos/acme/widgets/pulls/101/comments"] = fmt.Sprintf(`[
			{"id": 555, "user": {"login": "kyle"}, "body": "please rename this", "path": "seed.txt", "line": 1, "html_url": "https://x/555", "created_at": %q}
		]`, when)

		snap, err := h.sched.poll(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.sched.deliverHumanFeedback(ctx, snap); err != nil {
			t.Fatal(err)
		}
		if n := len(developerMail(t, h)); n != 1 {
			t.Fatalf("%d messages in the developer's inbox, want the person's feedback", n)
		}
		if got := len(h.sched.wake); got != 1 {
			t.Fatalf("%d wakes pending after the feedback was delivered, want 1", got)
		}
	})
}

// A wake still pending when the next full pass comes round is dropped: the
// pass does everything the local pass it asked for would have done, and a
// leftover signal would run a second one straight after it.
func TestAFullPassSupersedesAPendingWake(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	h.sched.signal()

	runPass(t, h)

	if got := len(h.sched.wake); got != 0 {
		t.Fatalf("%d wakes survived the full pass, want 0", got)
	}
}
