package scheduler

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/procs"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// retentionTOML runs no session: every role but the developer is off, and
// the only open issue is held for a person, so the passes do nothing but
// poll and sweep.
const retentionTOML = `
version = 1
[project]
repo = "acme/widgets"
` + rolesOffTOML + `
[scheduler]
poll_interval = "1h"
retention_period = "24h"
`

// fakeSessionDir makes a session directory as the runner leaves one: named
// after the session, the issue it worked on recorded when issue is set, a
// result file when it finished and a pid file when pid is set.
func fakeSessionDir(t *testing.T, h *harness, name string, issue int, finished bool, pid int) string {
	t.Helper()
	dir := filepath.Join(h.store.SessionsDir(), "20260901-120000-"+name+"-123456")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, session.TranscriptFile), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if issue > 0 {
		if err := session.WriteIssue(dir, issue); err != nil {
			t.Fatal(err)
		}
	}
	if finished {
		if err := os.WriteFile(filepath.Join(dir, session.ResultFile), []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if pid > 0 {
		if err := procs.WritePID(dir, pid); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func TestRetentionRemovesAClosedIssuesStateOnceRetentionPeriodHasPassed(t *testing.T) {
	closed := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	merged := closed.Add(2 * time.Hour)
	h := newHarnessAt(t, retentionTOML, closed.Add(23*time.Hour))
	h.sched.alive = func(pid int) bool { return pid == 4242 }

	// 7 closed with no pull request; 9 closed, and its pull request merged
	// two hours later; 10 closed with a session still recorded; 8 is open.
	for _, n := range []int{7, 9, 10} {
		h.gh.issues[n] = &github.Issue{Number: n, Title: "done", State: "CLOSED", ClosedAt: &closed,
			Labels: []github.Label{{Name: "bees"}}}
	}
	h.gh.issues[8] = &github.Issue{Number: 8, Title: "open", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:needs-human"}}}
	h.gh.prs[209] = &github.PR{Number: 209, State: "MERGED", MergedAt: &merged, HeadRefName: "bees/issue-9"}

	for _, is := range []state.IssueState{{Number: 7, Round: 2}, {Number: 8, Round: 1}, {Number: 9, PR: 209}, {Number: 10}} {
		if err := h.store.SaveIssue(is); err != nil {
			t.Fatal(err)
		}
	}
	running := fakeSessionDir(t, h, "developer-issue-10-r1", 10, false, 4242)
	if err := h.store.SetIssueSession(10, &state.SessionRun{Role: "developer", Name: "developer-issue-10-r1", Dir: running}); err != nil {
		t.Fatal(err)
	}
	finished7 := fakeSessionDir(t, h, "developer-issue-7-r1", 7, true, 0)
	legacy7 := fakeSessionDir(t, h, "developer-issue-7-r2", 0, true, 0)
	marked7 := fakeSessionDir(t, h, "project_manager", 7, true, 0)
	interrupted7 := fakeSessionDir(t, h, "developer-issue-7-r3", 7, false, 0)
	legacy9 := fakeSessionDir(t, h, "reviewer-pr-209-r1", 0, true, 0)
	open8 := fakeSessionDir(t, h, "developer-issue-8-r1", 8, true, 0)
	unrelated := fakeSessionDir(t, h, "qa", 0, true, 0)

	issueFile := func(n int) string { return filepath.Join(h.store.Dir, "issues", strconv.Itoa(n)+".json") }
	keptAlways := func() {
		t.Helper()
		for _, p := range []string{issueFile(8), open8, issueFile(10), running, unrelated} {
			if !exists(p) {
				t.Errorf("%s was removed", p)
			}
		}
	}

	// 23h after the close: nothing is stale yet.
	runPass(t, h)
	for _, p := range []string{issueFile(7), finished7, legacy7, marked7, interrupted7, issueFile(9), legacy9} {
		if !exists(p) {
			t.Errorf("before retention_period: %s was removed", p)
		}
	}
	keptAlways()

	// 25h after the close, 23h after 9's merge: 7 goes, but for the session
	// that never finished; 9 stays.
	h.clock.advance(2 * time.Hour)
	runPass(t, h)
	for _, p := range []string{issueFile(7), finished7, legacy7, marked7} {
		if exists(p) {
			t.Errorf("after retention_period: %s was kept", p)
		}
	}
	if !exists(interrupted7) {
		t.Error("the interrupted session of a closed issue was removed")
	}
	for _, p := range []string{issueFile(9), legacy9} {
		if !exists(p) {
			t.Errorf("counted from the close rather than the merge: %s was removed", p)
		}
	}
	keptAlways()

	// 25h after 9's merge.
	h.clock.advance(2 * time.Hour)
	runPass(t, h)
	for _, p := range []string{issueFile(9), legacy9} {
		if exists(p) {
			t.Errorf("after retention_period from the merge: %s was kept", p)
		}
	}
	keptAlways()
}

func TestRetentionSweepsAtMostOncePerInterval(t *testing.T) {
	closed := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, retentionTOML, closed.Add(time.Hour))
	// 7 closed inside the retention period, so its close time is remembered;
	// 301 is bookkeeping gh cannot view (a requested review's pull request),
	// asked about again on every sweep.
	h.gh.issues[7] = &github.Issue{Number: 7, State: "CLOSED", ClosedAt: &closed}
	for _, n := range []int{7, 301} {
		if err := h.store.SaveIssue(state.IssueState{Number: n}); err != nil {
			t.Fatal(err)
		}
	}
	runPass(t, h)
	if got := h.gh.callCount("issue view"); got != 2 {
		t.Fatalf("issue view calls after the first sweep = %d, want 2", got)
	}
	// A poll is due, a sweep is not.
	h.clock.advance(59 * time.Minute)
	h.sched.mu.Lock()
	h.sched.nextPoll = time.Time{}
	h.sched.mu.Unlock()
	runPass(t, h)
	if got := h.gh.callCount("issue view"); got != 2 {
		t.Fatalf("issue view calls after a second pass inside the interval = %d, want 2", got)
	}
	// Due again: 301 is asked about again, 7's close is remembered.
	h.clock.advance(time.Hour)
	runPass(t, h)
	if got := h.gh.callCount("issue view"); got != 3 {
		t.Fatalf("issue view calls after a later sweep = %d, want 3 (7's close time is remembered)", got)
	}
	for _, n := range []int{7, 301} {
		if !exists(filepath.Join(h.store.Dir, "issues", strconv.Itoa(n)+".json")) {
			t.Errorf("issues/%d.json removed inside the retention period", n)
		}
	}
}
