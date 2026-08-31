package scheduler

import (
	"context"
	"errors"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/state"
)

// seedWorkItem seeds an open work item, ready and sized so reconcile leaves
// it alone (no developer runs in these tests).
func seedWorkItem(h *harness, n int, title string, created time.Time) {
	h.gh.issues[n] = &github.Issue{Number: n, Title: title, State: "OPEN",
		Author: github.Author{Login: "kyle"}, CreatedAt: created, UpdatedAt: created,
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}
}

// nextPass advances past the backoff dispatchSingletons puts on a role when
// its session ends (one poll_interval) and forces a full poll, so the pass
// that follows is decided by the product manager's own has-work check rather
// than by that backoff. It stays well inside product_manager_interval.
func nextPass(t *testing.T, h *harness) {
	t.Helper()
	h.clock.advance(2 * h.cfg.Scheduler.PollInterval.Duration)
	forcePoll(h)
	runPass(t, h)
}

// quietFeature makes a feature stale: a bee comment has the last word, so
// AwaitingBee is false, and it was updated before the product manager's last
// run, so freshIssues does not even fetch it. Nothing but the completeness
// check can bring it back to the product manager.
func quietFeature(h *harness, n int, at time.Time) {
	h.gh.issues[n].UpdatedAt = at
	h.gh.issues[n].Comments = []github.Comment{{Author: github.Author{Login: "kyle"},
		Body: "work items listed\n\n<!-- bees:product_manager -->", CreatedAt: at}}
}

// lastPMPrompt returns the task prompt of the product manager session that
// ran last. proposals_test.go's pmPrompt sorts the session directories
// instead, whose names carry a real-time stamp at one-second resolution: two
// sessions in the same second sort at random, so a test that runs more than
// one product manager session reads the order out of the fake-order file.
func lastPMPrompt(t *testing.T, h *harness) string {
	t.Helper()
	dirs := h.sessionOrder()
	for i := len(dirs) - 1; i >= 0; i-- {
		if strings.Contains(dirs[i], "-"+config.RoleProductManager+"-") {
			return readFile(t, filepath.Join(h.store.SessionsDir(), dirs[i], "prompt.md"))
		}
	}
	t.Fatal("no product manager session ran")
	return ""
}

// The last open sub-issue of a feature closing is an event nobody reports:
// the work items are gone from the queues and the feature sits open until the
// product manager happens to run for another reason. The scheduler notices it
// from the sub-issue numbers the last product manager run recorded, checked
// against the issues the poll still finds open, and wakes the product manager
// with the feature in a section of its own — once, not on every pass after.
func TestAFeatureWhoseWorkIsDoneWakesTheProductManagerOnce(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, pmOnlyTOML, now)
	seedFeature(h, 5, "Exports", now.Add(-2*time.Hour))
	quietFeature(h, 5, now.Add(-time.Hour))
	// The fake answers the parent query for issue 1 with feature #5.
	seedWorkItem(h, 1, "Export to CSV", now.Add(-time.Hour))

	runPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 1 {
		t.Fatalf("first pass: product manager sessions: %d, want 1", n)
	}
	is, err := h.store.Issue(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(is.OpenChildren) != 1 || is.OpenChildren[0] != 1 {
		t.Fatalf("the feature's open sub-issues were not recorded: %+v", is.OpenChildren)
	}
	if !is.CompleteReportedAt.IsZero() {
		t.Errorf("a feature with an open sub-issue was reported complete: %v", is.CompleteReportedAt)
	}

	// The last work item closes. Nothing else changes: the feature is stale
	// and the product manager ran a moment ago.
	h.gh.issues[1].State = "CLOSED"
	nextPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 2 {
		t.Fatalf("closing the last sub-issue did not wake the product manager: %d sessions, want 2", n)
	}
	done := section(t, lastPMPrompt(t, h), "## Features whose work is done")
	if !strings.Contains(done, "#5: Exports") {
		t.Errorf("the finished feature is not presented for a close decision:\n%s", done)
	}
	if !strings.Contains(h.logs.String(), "feature work is complete") {
		t.Error("the completed feature was not logged")
	}
	is, err = h.store.Issue(5)
	if err != nil {
		t.Fatal(err)
	}
	if is.CompleteReportedAt.IsZero() {
		t.Fatalf("the report was not recorded: %+v", is)
	}

	// A pass with nothing changed does not report it again: the product
	// manager may have looked at the feature and deliberately left it open.
	nextPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 2 {
		t.Errorf("the same finished feature woke the product manager again: %d sessions, want 2", n)
	}
}

// The completeness check runs on every pass, so it must cost nothing: the
// sub-issue numbers come from the state dir and the open set from the poll
// that has already happened. Neither branch of it may reach GitHub.
func TestTheCompletedFeatureCheckCostsNoGitHubCalls(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, pmOnlyTOML, now)
	seedFeature(h, 5, "Exports", now.Add(-2*time.Hour))
	quietFeature(h, 5, now.Add(-time.Hour))
	seedWorkItem(h, 1, "Export to CSV", now.Add(-time.Hour))
	// The product manager ran a moment ago, so only an event wakes it.
	if err := h.store.SaveRole(config.RoleProductManager, state.RoleState{LastRun: now}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}

	// Nothing recorded: the pass gh makes are the ones it made before #239.
	before := h.gh.total()
	if h.sched.productManagerHasWork(ctx, snap) {
		t.Fatal("a stale feature with no recorded sub-issues woke the product manager")
	}
	base := h.gh.total() - before

	// A recorded sub-issue that is still open: the same calls, no more.
	// Seeded through the owner of the two fields — SaveIssue writes the
	// developer worker's bookkeeping and carries these over from the file.
	if err := h.store.SetOpenChildren(5, []int{1}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	before = h.gh.total()
	if h.sched.productManagerHasWork(ctx, snap) {
		t.Fatal("a feature with an open sub-issue woke the product manager")
	}
	if got := h.gh.total() - before; got != base {
		t.Errorf("the completeness check added %d gh calls to the polling path (%d, was %d)", got-base, got, base)
	}

	// And the check that fires makes no call at all.
	if err := h.store.SetOpenChildren(5, []int{77}, time.Time{}); err != nil {
		t.Fatal(err)
	}
	before = h.gh.total()
	if !h.sched.productManagerHasWork(ctx, snap) {
		t.Fatal("a feature whose recorded sub-issue is gone did not wake the product manager")
	}
	if got := h.gh.total() - before; got != 0 {
		t.Errorf("reporting a completed feature made %d gh calls, want 0: %v", got, h.gh.calls[len(h.gh.calls)-got:])
	}
}

// A feature that gains a sub-issue after being reported complete is a feature
// whose work is not finished after all: the recorded set changes, which clears
// the report, and closing the new item reports it again. Without that, a
// feature could only ever be reported once in its life.
func TestAFeatureThatGainsASubIssueIsReportedAgain(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, pmOnlyTOML, now)
	h.gh.parents = map[int]int{1: 5, 7: 5}
	seedFeature(h, 5, "Exports", now.Add(-2*time.Hour))
	quietFeature(h, 5, now.Add(-time.Hour))
	seedWorkItem(h, 1, "Export to CSV", now.Add(-time.Hour))

	runPass(t, h)
	h.gh.issues[1].State = "CLOSED"
	nextPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 2 {
		t.Fatalf("closing the last sub-issue did not wake the product manager: %d sessions, want 2", n)
	}

	// The product manager decided the feature was not finished and created
	// another work item. Its next run (mail, here) records the new set, which
	// re-arms the check.
	seedWorkItem(h, 7, "Export to XLSX", now)
	if _, err := h.box.Send(mail.Message{From: HumanSender, To: config.RoleProductManager,
		Subject: "one more thing", Body: "xlsx too"}); err != nil {
		t.Fatal(err)
	}
	nextPass(t, h)
	is, err := h.store.Issue(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(is.OpenChildren) != 1 || is.OpenChildren[0] != 7 {
		t.Fatalf("the new sub-issue was not recorded: %+v", is.OpenChildren)
	}
	if !is.CompleteReportedAt.IsZero() {
		t.Fatal("a feature with a new open sub-issue is still reported complete")
	}

	// Closing that one reports the feature complete a second time.
	h.gh.issues[7].State = "CLOSED"
	nextPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 4 {
		t.Fatalf("the re-armed feature did not wake the product manager: %d sessions, want 4", n)
	}
	done := section(t, lastPMPrompt(t, h), "## Features whose work is done")
	if !strings.Contains(done, "#5: Exports") {
		t.Errorf("the finished feature is not presented for a close decision:\n%s", done)
	}
}

// The recorded sub-issues are a memory, not a mirror of the last parent
// lookup: a feature with no open children is exactly the state the
// completeness check exists to spot, so an empty lookup never overwrites what
// the scheduler remembers. That also makes the check survive a failed
// ParentIssue query, which is a warning rather than a failed run.
func TestAnEmptyParentLookupKeepsTheRecordedSubIssues(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, pmOnlyTOML, now)
	seedFeature(h, 5, "Exports", now.Add(-2*time.Hour))
	quietFeature(h, 5, now.Add(-time.Hour))
	seedWorkItem(h, 1, "Export to CSV", now.Add(-time.Hour))
	runPass(t, h)

	// The sub-issue query fails from here on. The product manager still runs
	// (the Parent column just shows `-` for everything), and the numbers it
	// recorded before survive the run.
	h.gh.errFor["api graphql"] = errors.New("sub-issue query is down")
	if _, err := h.box.Send(mail.Message{From: HumanSender, To: config.RoleProductManager,
		Subject: "how is Exports going", Body: "?"}); err != nil {
		t.Fatal(err)
	}
	nextPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 2 {
		t.Fatalf("mail did not start a product manager session: %d, want 2", n)
	}
	is, err := h.store.Issue(5)
	if err != nil {
		t.Fatal(err)
	}
	if len(is.OpenChildren) != 1 || is.OpenChildren[0] != 1 {
		t.Fatalf("a run that saw no sub-issues erased the recorded ones: %+v", is.OpenChildren)
	}

	// So the last work item closing is still noticed, without any GitHub call
	// to notice it with.
	h.gh.issues[1].State = "CLOSED"
	nextPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 3 {
		t.Fatalf("the finished feature did not wake the product manager: %d sessions, want 3", n)
	}
	if !strings.Contains(section(t, lastPMPrompt(t, h), "## Features whose work is done"), "#5: Exports") {
		t.Error("the finished feature is not presented for a close decision")
	}
}

// A session that never ran cannot have delivered the report, so the trigger
// is not spent by it: the account hitting its session limit must not cost the
// feature its one presentation. The house pattern at the same seam is the
// same — markRun and MarkRead both happen after the session.
func TestAFailedProductManagerSessionDoesNotSpendTheReport(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, pmOnlyTOML, now)
	seedFeature(h, 5, "Exports", now.Add(-2*time.Hour))
	quietFeature(h, 5, now.Add(-time.Hour))
	seedWorkItem(h, 1, "Export to CSV", now.Add(-time.Hour))
	runPass(t, h)

	// The last work item closes while the account is out of capacity: the
	// session dies without reporting an outcome, the way one that could not
	// start does.
	t.Setenv("FAKE_LIMIT", strconv.FormatInt(now.Add(time.Hour).Unix(), 10))
	h.gh.issues[1].State = "CLOSED"
	nextPass(t, h)
	is, err := h.store.Issue(5)
	if err != nil {
		t.Fatal(err)
	}
	if !is.CompleteReportedAt.IsZero() {
		t.Fatalf("a session that never ran spent the report: %v", is.CompleteReportedAt)
	}

	// With capacity back, the finished feature is still presented.
	t.Setenv("FAKE_LIMIT", "")
	h.clock.advance(3 * time.Hour)
	forcePoll(h)
	runPass(t, h)
	done := section(t, lastPMPrompt(t, h), "## Features whose work is done")
	if !strings.Contains(done, "#5: Exports") {
		t.Errorf("the finished feature was lost by the failed session:\n%s", done)
	}
}

// A parent lookup that only partly answered is not a memory either: recording
// the truncated set would look exactly like the missing children having
// closed, and the next pass would report the feature complete while a real
// sub-issue is still open.
func TestAPartialParentLookupKeepsTheRecordedSubIssues(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, pmOnlyTOML, now)
	h.gh.parents = map[int]int{1: 5, 7: 5}
	seedFeature(h, 5, "Exports", now.Add(-2*time.Hour))
	quietFeature(h, 5, now.Add(-time.Hour))
	seedWorkItem(h, 1, "Export to CSV", now.Add(-time.Hour))
	seedWorkItem(h, 7, "Export to XLSX", now.Add(-time.Hour))
	runPass(t, h)
	is, err := h.store.Issue(5)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(is.OpenChildren, []int{1, 7}) {
		t.Fatalf("the feature's open sub-issues were not recorded: %+v", is.OpenChildren)
	}

	// #7's query fails on the next run: #1 still answers, so the lookup is
	// non-empty but short of one child.
	h.gh.parentErr = map[int]error{7: errors.New("sub-issue query is down")}
	if _, err := h.box.Send(mail.Message{From: HumanSender, To: config.RoleProductManager,
		Subject: "how is Exports going", Body: "?"}); err != nil {
		t.Fatal(err)
	}
	nextPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 2 {
		t.Fatalf("mail did not start a product manager session: %d, want 2", n)
	}
	is, err = h.store.Issue(5)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(is.OpenChildren, []int{1, 7}) {
		t.Fatalf("a partial lookup was recorded over the remembered set: %+v", is.OpenChildren)
	}

	// So closing #1 while #7 is still open reports nothing.
	h.gh.issues[1].State = "CLOSED"
	nextPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 2 {
		t.Errorf("a feature with an open sub-issue was reported complete: %d sessions, want 2", n)
	}
}

// A failed run still saw the sub-issues, so it records them even though it
// does not spend the report: a feature that gains a work item while the
// account is out of capacity would otherwise keep the mark it was given
// before that item existed, and closing the new item would clear nothing —
// the feature could never be reported again.
func TestAFailedProductManagerSessionStillRecordsTheSubIssues(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, pmOnlyTOML, now)
	h.gh.parents = map[int]int{1: 5, 7: 5}
	seedFeature(h, 5, "Exports", now.Add(-2*time.Hour))
	quietFeature(h, 5, now.Add(-time.Hour))
	seedWorkItem(h, 1, "Export to CSV", now.Add(-time.Hour))
	runPass(t, h)
	h.gh.issues[1].State = "CLOSED"
	nextPass(t, h)
	if is, err := h.store.Issue(5); err != nil || is.CompleteReportedAt.IsZero() {
		t.Fatalf("the feature was not reported complete: %+v (%v)", is, err)
	}

	// The product manager decided the feature needed one more work item, and
	// the run that would have recorded it dies on the session limit.
	seedWorkItem(h, 7, "Export to XLSX", now)
	if _, err := h.box.Send(mail.Message{From: HumanSender, To: config.RoleProductManager,
		Subject: "how is Exports going", Body: "?"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_LIMIT", strconv.FormatInt(now.Add(10*time.Minute).Unix(), 10))
	nextPass(t, h)
	is, err := h.store.Issue(5)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(is.OpenChildren, []int{7}) {
		t.Fatalf("the failed run did not record the new sub-issue: %+v", is.OpenChildren)
	}
	if !is.CompleteReportedAt.IsZero() {
		t.Fatalf("the new sub-issue did not re-arm the check: %v", is.CompleteReportedAt)
	}

	// Closing it reports the feature complete again, well inside
	// product_manager_interval so nothing but the trigger can be the wake.
	t.Setenv("FAKE_LIMIT", "")
	h.clock.advance(15 * time.Minute)
	h.gh.issues[7].State = "CLOSED"
	forcePoll(h)
	runPass(t, h)
	if !strings.Contains(section(t, lastPMPrompt(t, h), "## Features whose work is done"), "#5: Exports") {
		t.Error("the re-armed feature is not presented for a close decision")
	}
}
