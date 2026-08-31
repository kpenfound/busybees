package scheduler

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/procs"
	"github.com/kpenfound/busybees/internal/state"
)

// liveProcess starts a process the test may stop, records its pid the way a
// running session does and returns the session directory holding it. It is
// never claude: what is being exercised is procs' stop path, and a real
// process is the only honest way to exercise it. It is not a process group
// leader either, so procs.Kill signals it alone and never the test binary's
// own group.
func liveProcess(t *testing.T, dir string) *exec.Cmd {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", "sleep 60 & wait")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	if err := procs.WritePID(dir, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	return cmd
}

// wantExited waits for a stopped process to be reaped. procs.Alive is true
// for a zombie — the test binary is its parent and nothing has waited on it
// — so "it is gone" is asserted by reaping it, not by asking about the pid.
func wantExited(t *testing.T, cmd *exec.Cmd) {
	t.Helper()
	done := make(chan error, 1)
	go func() { _, err := cmd.Process.Wait(); done <- err }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Errorf("pid %d is still running after the kill", cmd.Process.Pid)
	}
}

// TestKillingASessionStopsItAndEscalatesItsIssue: the live view's kill key
// is the two halves the factory already has and not a third implementation
// of either — procs stops the process, and escalate hands the issue to a
// person with a reason on GitHub. It also leaves the mark that stops the
// session's own worker from retrying it or escalating it a second time.
func TestKillingASessionStopsItAndEscalatesItsIssue(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	seedIssue(h, 1, "bees:in-progress", "m", time.Now().Add(-time.Hour))

	dir := filepath.Join(t.TempDir(), "session")
	cmd := liveProcess(t, dir)
	h.sched.recordLiveSession("developer-issue-1-r1", liveSession{
		role: config.RoleDeveloper, dir: dir, issue: 1, pr: 201,
	})

	if err := h.sched.KillSession(context.Background(), "developer-issue-1-r1"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	wantExited(t, cmd)
	if got := h.stateOfIssue(1); got != "needs-human" {
		t.Errorf("issue #1 is %q, want needs-human: the kill did not escalate it", got)
	}
	// Why, in the reason the factory records for a view and in the comment
	// it leaves for the person it was handed to.
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bk.Escalation, "A person stopped") {
		t.Errorf("the recorded escalation reason does not say a person stopped it: %q", bk.Escalation)
	}
	if !strings.Contains(commentOn(h, 1), "A person stopped") {
		t.Errorf("no comment says why the issue was handed over:\n%s", commentOn(h, 1))
	}
	// The session directory says it was stopped, so the next session for
	// this issue is not left guessing whether the machine crashed.
	if _, err := os.Stat(filepath.Join(dir, "interrupted")); err != nil {
		t.Errorf("the stopped session was not marked: %v", err)
	}
	// And the mark the worker reads is there, exactly once.
	if !h.sched.tookKill("developer-issue-1-r1") {
		t.Error("the stopped session left no mark for its worker")
	}
	if h.sched.tookKill("developer-issue-1-r1") {
		t.Error("the kill mark was not consumed by the attempt it was set for")
	}
}

// A session name no session is running under is an error, not a kill of
// something else, and it leaves no mark behind for a later session of that
// name to trip over.
func TestKillingASessionThatIsNotRunning(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	err := h.sched.KillSession(context.Background(), "developer-issue-9-r1")
	if err == nil || !strings.Contains(err.Error(), "developer-issue-9-r1") {
		t.Fatalf("KillSession of an unknown session returned %v, want an error naming it", err)
	}
	if h.sched.tookKill("developer-issue-9-r1") {
		t.Error("a failed kill left a mark behind")
	}
	// A session that is recorded but whose process has gone is the same
	// answer: nothing to stop, and no issue handed over on the strength of
	// a kill that did not happen.
	seedIssue(h, 1, "bees:in-progress", "m", time.Now().Add(-time.Hour))
	h.sched.recordLiveSession("developer-issue-1-r1", liveSession{
		role: config.RoleDeveloper, dir: t.TempDir(), issue: 1,
	})
	if err := h.sched.KillSession(context.Background(), "developer-issue-1-r1"); err == nil {
		t.Fatal("KillSession of a session with no live process returned no error")
	}
	if h.sched.tookKill("developer-issue-1-r1") {
		t.Error("a failed kill left a mark behind")
	}
	if got := h.stateOfIssue(1); got == "needs-human" {
		t.Error("a kill that stopped nothing still escalated the issue")
	}
}

// Stopping a singleton stops a session and nothing more: a product manager
// run owns no work item, so there is nothing to hand to anybody.
func TestKillingASingletonSessionEscalatesNothing(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	dir := filepath.Join(t.TempDir(), "session")
	cmd := liveProcess(t, dir)
	h.sched.recordLiveSession("product-manager-1", liveSession{role: config.RoleProductManager, dir: dir})

	before := h.gh.total()
	if err := h.sched.KillSession(context.Background(), "product-manager-1"); err != nil {
		t.Fatalf("KillSession: %v", err)
	}
	wantExited(t, cmd)
	if got := h.gh.total(); got != before {
		t.Errorf("stopping a singleton cost %d gh calls, want none", got-before)
	}
}

// commentOn returns everything the fake gh was asked to comment on an issue.
func commentOn(h *harness, n int) string {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	var b strings.Builder
	for _, c := range h.gh.calls {
		if len(c) >= 3 && c[0] == "issue" && c[1] == "comment" && c[2] == strconv.Itoa(n) {
			b.WriteString(strings.Join(c, " ") + "\n")
		}
	}
	return b.String()
}

// TestTheNeedsHumanAndApprovedQueuesCarryTheirDetail: the two panels a
// person watching the factory reads when it is stuck are built from the
// poll that counted the queues and from the state directory, never from a
// GitHub call of their own. The escalation reason is what escalate recorded
// on the issue; an issue a person labelled bees:needs-human by hand has
// none, and is listed all the same.
func TestTheNeedsHumanAndApprovedQueuesCarryTheirDetail(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	old := time.Now().Add(-72 * time.Hour)
	seedIssue(h, 1, "bees:needs-human", "m", old)
	seedIssue(h, 2, "bees:needs-human", "m", old)
	seedIssue(h, 3, "bees:approved", "m", old)
	seedIssue(h, 4, "bees:approved", "m", old)
	// The later issue holds the older pull request, so the wanted order
	// contradicts the order the issues themselves come in: only a list
	// really sorted by the pull request's age passes.
	h.gh.prs[203].CreatedAt = time.Now().Add(-24 * time.Hour)
	h.gh.prs[204].CreatedAt = time.Now().Add(-48 * time.Hour)
	// #2 was escalated by the factory; #1 carries the label from a person.
	if err := h.store.SetEscalation(2, "3 review rounds and no approval", time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	before := h.gh.total()
	h.sched.setQueues(snap)
	if got := h.gh.total(); got != before {
		t.Errorf("building the two lists cost %d gh calls, want none", got-before)
	}
	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}

	want := []state.Escalated{
		{Issue: 2, Title: "Issue 2", Reason: "3 review rounds and no approval"},
		{Issue: 1, Title: "Issue 1"},
	}
	if len(st.NeedsHuman) != len(want) {
		t.Fatalf("needs_human: got %+v, want %d entries", st.NeedsHuman, len(want))
	}
	for i, w := range want {
		got := st.NeedsHuman[i]
		if got.Issue != w.Issue || got.Title != w.Title || got.Reason != w.Reason {
			t.Errorf("needs_human[%d]: got %+v want issue %d %q %q", i, got, w.Issue, w.Title, w.Reason)
		}
	}
	if st.NeedsHuman[0].Since.IsZero() {
		t.Error("the escalated issue records no time, so the list cannot be ordered by it")
	}

	// Oldest pull request first: #204 was opened a day before #203.
	if len(st.Approved) != 2 || st.Approved[0].PR != 204 || st.Approved[1].PR != 203 {
		t.Fatalf("approved: got %+v, want PRs 204 then 203", st.Approved)
	}
	if st.Approved[0].Issue != 4 || st.Approved[0].Title != "Issue 4" || st.Approved[0].Since.IsZero() {
		t.Errorf("approved[0]: got %+v, want the issue, title and open time of PR 204", st.Approved[0])
	}
}

// An approved issue whose pull request the poll no longer finds open is not
// something a person is being asked to merge: it has been merged or closed
// and the label has yet to catch up.
func TestAnApprovedIssueWithNoOpenPullRequestIsNotListed(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	seedIssue(h, 1, "bees:approved", "m", time.Now().Add(-time.Hour))
	delete(h.gh.prs, 201)

	snap, err := h.sched.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h.sched.setQueues(snap)
	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Approved) != 0 {
		t.Errorf("approved: got %+v, want nothing", st.Approved)
	}
	if st.Queues["approved"] != 1 {
		t.Errorf("the approved queue count is %d, want 1: only the detail list drops it", st.Queues["approved"])
	}
}

// A session a person stopped is not retried and does not escalate its issue
// a second time: KillSession has already handed it over, and a retry would
// start the work again on an issue that is now a person's.
func TestAStoppedSessionIsNotRetried(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	h.sched.mu.Lock()
	h.sched.killed["developer-issue-1-r1"] = true
	h.sched.mu.Unlock()

	spec := sessionSpec{role: config.RoleDeveloper, name: "developer-issue-1-r1", workDir: h.cfg.Dir()}
	spec.data.Issue = &github.Issue{Number: 1, Title: "Issue 1"}
	_, err := h.sched.runSessionWithRetry(context.Background(), spec)
	if !errors.Is(err, errSessionKilled) {
		t.Fatalf("a stopped session reported %v, want errSessionKilled", err)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 1 {
		t.Errorf("%d developer sessions ran, want exactly 1: the stopped one was retried", n)
	}
}
