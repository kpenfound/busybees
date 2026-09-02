package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/procs"
	"github.com/kpenfound/busybees/internal/session"
)

// startHeldSession seeds one ready issue, starts the real loop and waits for
// the developer session on it to reach the point cond names. The session is
// held on FAKE_WAIT_FOR (the file at the returned release path), so it
// cannot finish before the test decides what happens to it — cancelling the
// loop is racing nothing. It returns the session directory, the release
// path, the loop's cancel and the channel Run's error arrives on.
//
// The developer is the only role that runs unless the caller has already
// said otherwise: a test about what the worker does after that session
// enables the reviewer too.
func startHeldSession(t *testing.T, h *harness, cond func(dir string) bool, what string) (dir, release string, cancel context.CancelFunc, done chan error) {
	t.Helper()
	if h.sched.OnlyRoles == nil {
		h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	}
	release = filepath.Join(t.TempDir(), "release")
	t.Setenv("FAKE_WAIT_FOR", release)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))

	h.sched.Once = false
	ctx, cancel := context.WithCancel(context.Background())
	done = make(chan error, 1)
	go func() { done <- h.sched.Run(ctx) }()

	waitFor(t, 30*time.Second, what, func() bool {
		dirs := h.sessions(config.RoleDeveloper)
		if len(dirs) == 0 {
			return false
		}
		dir = dirs[0]
		return cond(dir)
	})
	return dir, release, cancel, done
}

// waitRun waits for the loop started by startHeldSession to return.
func waitRun(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(time.Minute):
		t.Fatal("Run did not return")
		return nil
	}
}

// Cancelling the loop's context is the cool-down: the running session is
// left to finish — its result and outcome are written, and Run returns only
// after them — and the console says how many sessions it is waiting for and
// that a second interrupt stops them (#338). Before the split into two
// contexts the same cancellation reached `claude` and SIGKILLed it, so the
// promised drain threw the session's work away.
func TestCancellingTheLoopLetsTheRunningSessionFinish(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	dir, release, cancel, done := startHeldSession(t, h, func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "args.txt"))
		return err == nil
	}, "the developer session to start")
	defer cancel()

	cancel()
	// Room for the old behaviour — the loop's cancellation reaching the
	// session — to kill it before the release. The fixed code races
	// nothing: the session cannot finish before the file exists.
	time.Sleep(200 * time.Millisecond)
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, session.ResultFile)); err != nil {
		t.Errorf("the cancelled loop killed the session instead of letting it finish: %v", err)
	}
	o, ok, err := session.ReadOutcome(dir)
	if err != nil || !ok || o.Status != OutcomePROpened {
		t.Errorf("outcome = %+v (ok=%v, err=%v), want pr-opened: the session must run to completion", o, ok, err)
	}
	if logs := h.logs.String(); !strings.Contains(logs, "waiting for 1 running session to finish; interrupt again to stop them now") {
		t.Errorf("the console does not count the sessions or name the way out:\n%s", logs)
	}
}

// HardStop is the second interrupt: the running session is killed, no result
// file is written, and the directory reads as an interrupted session — with
// the record still in the issue's bookkeeping — so the next `bees run`
// resumes the work through the ordinary crash-recovery path.
func TestHardStopKillsTheRunningSession(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	// The release file is never created: only the hard stop ends the session.
	dir, _, cancel, done := startHeldSession(t, h, func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, procs.PIDFile))
		return err == nil
	}, "the developer session's process")
	defer cancel()

	cancel()
	h.sched.HardStop()
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, session.ResultFile)); err == nil {
		t.Error("a hard-stopped session wrote a result file, so the directory reads as finished instead of interrupted")
	}
	in, running := session.CheckInterrupted(config.RoleDeveloper, dir, nil)
	if running || in == nil {
		t.Fatalf("CheckInterrupted = (%v, running=%v), want an interrupted report", in, running)
	}
	if !in.Killed || !strings.Contains(in.Note, "stopped with the factory") {
		t.Errorf("the report does not say the session was stopped on purpose: killed=%v, note=%q", in.Killed, in.Note)
	}
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.Session == nil {
		t.Error("the running-session record was cleared: the next scheduler cannot tell its session the work was interrupted")
	}
	if logs := h.logs.String(); !strings.Contains(logs, "stopping 1 running session now") {
		t.Errorf("the hard stop does not say what it is stopping:\n%s", logs)
	}
}

// A pass that is still running when the loop's context is cancelled starts
// nothing: sessions no longer die with that context, so without a gate at
// both dispatch sites the pass would start work the cool-down promised not
// to — and the session would run to completion under its own context.
func TestNoSessionStartsAfterTheLoopIsCancelled(t *testing.T) {
	h := newHarness(t, baseTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	// Work for a singleton too: a triage issue is the project manager's.
	h.gh.issues[3] = &github.Issue{Number: 3, Title: "Refine me", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: time.Now().Add(-time.Hour)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 0 {
		t.Errorf("%d developer sessions ran on a cancelled loop, want 0", n)
	}
	if n := len(h.sessions(config.RoleProjectManager)); n != 0 {
		t.Errorf("%d project manager sessions ran on a cancelled loop, want 0", n)
	}
}

// Outside Run there is no session context and nothing to stop: HardStop is
// a no-op rather than a panic.
func TestHardStopOutsideRunDoesNothing(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	h.sched.HardStop()
}

// One work item's develop -> review loop is one piece of work in progress,
// so a cool-down carries it to a natural end: the developer session that was
// running when the loop's context was cancelled is followed by the review
// that belongs with it, and the issue reaches approval (#339). Before the
// change the worker checked the loop's context between stages, so a stop
// landed mid-issue and the review never ran.
func TestACooldownCarriesTheWorkerIntoItsReview(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true, config.RoleReviewer: true}
	// The pre-review read has an answer, and the first review approves
	// rather than asking for a round the cool-down would have to carry too.
	h.gh.checks = []checksResponse{{`[{"name":"go / test","bucket":"pass","state":"SUCCESS"}]`, nil}}
	seedCounter(t, h, "review", 1)
	_, release, cancel, done := startHeldSession(t, h, func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "args.txt"))
		return err == nil
	}, "the developer session to start")
	defer cancel()

	cancel()
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := len(h.sessions(config.RoleReviewer)); n != 1 {
		t.Fatalf("%d reviewer sessions after the loop was cancelled, want 1: the worker must run the stages its issue has left", n)
	}
	if got := h.gh.history[1]; len(got) == 0 || got[len(got)-1] != "bees:approved" {
		t.Errorf("issue 1 label history %v, want it to end at bees:approved", got)
	}
}

// The hard stop is the other half: it cancels the context the worker itself
// runs under, so the developer session is killed and the review that would
// have followed it does not run.
func TestAHardStopEndsTheWorkerBeforeItsReview(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true, config.RoleReviewer: true}
	// The release file is never created: only the hard stop ends the session.
	_, _, cancel, done := startHeldSession(t, h, func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, procs.PIDFile))
		return err == nil
	}, "the developer session's process")
	defer cancel()

	cancel()
	h.sched.HardStop()
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := len(h.sessions(config.RoleReviewer)); n != 0 {
		t.Errorf("%d reviewer sessions after a hard stop, want 0: the worker must end where it stands", n)
	}
}

// The exits the loop already has are what bounds a cool-down, and they still
// work in one: a worker that runs out of review rounds escalates the issue
// and returns, exactly as it does with the loop still running.
func TestACooldownStillEscalatesAtTheRoundLimit(t *testing.T) {
	h := newHarness(t, strings.Replace(prereviewTOML, "max_review_rounds = 3", "max_review_rounds = 1", 1))
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true, config.RoleReviewer: true}
	h.gh.checks = []checksResponse{{`[{"name":"go / test","bucket":"pass","state":"SUCCESS"}]`, nil}}
	// The one round this issue has requests changes, which is the round
	// limit reached.
	_, release, cancel, done := startHeldSession(t, h, func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "args.txt"))
		return err == nil
	}, "the developer session to start")
	defer cancel()

	cancel()
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if n := len(h.sessions(config.RoleReviewer)); n != 1 {
		t.Fatalf("%d reviewer sessions, want 1: the round limit must end the loop, not the cool-down", n)
	}
	if got := h.gh.history[1]; len(got) == 0 || got[len(got)-1] != "bees:needs-human" {
		t.Errorf("issue 1 label history %v, want it to end at bees:needs-human", got)
	}
	if c := strings.Join(h.gh.comments[1], "\n"); !strings.Contains(c, "review round") {
		t.Errorf("the escalation does not say the pull request ran out of review rounds: %q", c)
	}
}

// A cool-down that lands between two of a worker's stages has no session to
// count, and the console must not fall silent for as long as the worker
// takes to finish the issue it is carrying. The counted line stays exactly
// what it was: it counts the sessions running at that instant, which is
// still true, and TestCancellingTheLoopLetsTheRunningSessionFinish pins it
// through a real run.
func TestStopNoticeSaysWhatTheWaitIsFor(t *testing.T) {
	cases := []struct {
		name     string
		sessions int
		held     int
		cooling  bool
		want     string
	}{
		{"a cool-down with a session running", 1, 1, true,
			"waiting for 1 running session to finish; interrupt again to stop them now"},
		{"a cool-down between two stages", 0, 1, true,
			"waiting for the work in flight to finish; interrupt again to stop it now"},
		{"a --once run whose sessions are still going", 2, 1, false,
			"waiting for 2 running sessions to finish"},
		// Nothing was interrupted, so there is no "again" to offer and the
		// worker is not being waited out by anybody's request.
		{"a --once run between two stages", 0, 1, false, ""},
		{"nothing in flight", 0, 0, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stopNotice(c.sessions, c.held, c.cooling); got != c.want {
				t.Errorf("stopNotice(%d, %d, %v) = %q, want %q", c.sessions, c.held, c.cooling, got, c.want)
			}
		})
	}
}

// The wait a person is in is what the line is about, so Run reads the
// issues workers hold as well as the sessions running: a cool-down landing
// between two of a worker's stages — here in the pre-review checks wait —
// has no session to count, and silence would leave the console saying
// nothing for as long as the worker takes to finish its issue.
func TestACooldownBetweenTwoStagesSaysWhatItIsWaitingFor(t *testing.T) {
	// Two seconds of pre-review wait is the gap the cancel lands in. The
	// developer session is released first and the reviewer's cannot start
	// until the wait is over, so the worker is between stages by
	// construction rather than by timing.
	h := newHarness(t, prereviewTOML+"[roles.reviewer]\nchecks_wait = \"2s\"\nchecks_poll_interval = \"10ms\"\n")
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true, config.RoleReviewer: true}
	h.gh.checks = []checksResponse{{`[{"name":"go / test","bucket":"pass","state":"SUCCESS"}]`, nil}}
	seedCounter(t, h, "review", 1)
	_, release, cancel, done := startHeldSession(t, h, func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "args.txt"))
		return err == nil
	}, "the developer session to start")
	defer cancel()

	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the worker to be between two stages", func() bool {
		return h.sched.liveCount() == 0 && h.sched.heldIssues() > 0
	})
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if logs := h.logs.String(); !strings.Contains(logs, "waiting for the work in flight to finish; interrupt again to stop it now") {
		t.Errorf("the console says nothing about the issue the worker is still carrying:\n%s", logs)
	}
}
