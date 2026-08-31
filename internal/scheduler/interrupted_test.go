package scheduler

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/procs"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// killedSession writes what a scheduler killed mid-session leaves behind: a
// session directory with a transcript no result file ever closed, and the
// record of it in the issue's bookkeeping. It returns the directory.
func killedSession(t *testing.T, h *harness, issue int, role, name string, files map[string]string) string {
	t.Helper()
	dir := filepath.Join(h.store.SessionsDir(), "20260101-000000-"+name+"-killed")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for f, body := range files {
		if err := os.WriteFile(filepath.Join(dir, f), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run := &state.SessionRun{Role: role, Name: name, Dir: dir, StartedAt: time.Now().Add(-time.Hour)}
	// Through SetIssueSession, not SaveIssue: the record is owned by that
	// one writer and SaveIssue carries the field over from the file.
	if err := h.store.SetIssueSession(issue, run); err != nil {
		t.Fatal(err)
	}
	return dir
}

// twoTurns is an unfinished transcript: two assistant messages and no result
// event, which is what a session that was killed writes.
const twoTurns = `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"role":"assistant"}}
{"type":"user","message":{"role":"user"}}
{"type":"assistant","message":{"role":"assistant"}}
`

// TestAKilledSessionIsReportedToTheNextSessionOfItsRole is the point of #250:
// a scheduler killed while a developer session ran leaves a branch that may
// carry work nobody reported, and the session that takes over used to start
// as if nothing had happened. It is told what was interrupted, how far it
// got and where to read what it did.
func TestAKilledSessionIsReportedToTheNextSessionOfItsRole(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	dir := killedSession(t, h, 1, config.RoleDeveloper, "developer-issue-1-r1", map[string]string{
		session.TranscriptFile:  twoTurns,
		session.InterruptedFile: "stopped by bees kill\n",
	})
	// Nothing is running: the killed process is gone.
	h.sched.alive = func(int) bool { return false }
	runPass(t, h)

	prompt := promptOf(t, h, 0)
	for _, want := range []string{
		"developer session that ran for this issue before you was stopped after 2 turns",
		filepath.Join(dir, session.TranscriptFile),
		"The branch may carry work it never reported",
	} {
		if !strings.Contains(flowedPrompt(prompt), want) {
			t.Errorf("the developer was not told about the interrupted session (missing %q):\n%s", want, prompt)
		}
	}
	// The record is gone — the sessions that ran for this issue overwrote it
	// as they started and cleared it as they ended — and the reviewer session
	// of the same worker is not told about a developer session.
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.Session != nil {
		t.Errorf("the record of the interrupted session outlived it: %+v", bk.Session)
	}
	for i, name := range h.sessionNames() {
		if i == 0 {
			continue
		}
		if strings.Contains(promptOf(t, h, i), "never reported an outcome") {
			t.Errorf("session %d (%s) was told about an interruption that was not its own", i, name)
		}
	}
}

// TestARunningSessionIsNeverReportedAsInterrupted: a session that has not
// written its result file yet is the ordinary state of a session that is
// still running, so absence of the file cannot be the signal on its own. A
// live pid means another scheduler owns that session: nothing is reported,
// and the record is left where it is rather than consumed.
func TestARunningSessionIsNeverReportedAsInterrupted(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	killedSession(t, h, 1, config.RoleDeveloper, "developer-issue-1-r1", map[string]string{
		session.TranscriptFile: twoTurns,
		procs.PIDFile:          "4242\n",
	})
	h.sched.alive = func(pid int) bool { return pid == 4242 }
	runPass(t, h)

	if prompt := promptOf(t, h, 0); strings.Contains(prompt, "never reported an outcome") {
		t.Errorf("a running session was reported as interrupted:\n%s", prompt)
	}
	if !strings.Contains(h.logs.String(), "a session recorded for this issue is still running") {
		t.Errorf("the running session was not noticed at all:\n%s", h.logs.String())
	}
}

// TestTakeInterruptedConsumesOnlyWhatIsNoLongerRunning pins the bookkeeping
// the loop cannot show. The record of a session that is still running, and
// of one that was interrupted, are both left where they are — the first
// because its scheduler owns it, the second because no session has been told
// yet; only a record whose session turns out to have finished is cleared, and
// that one silently.
func TestTakeInterruptedConsumesOnlyWhatIsNoLongerRunning(t *testing.T) {
	cases := []struct {
		name   string
		files  map[string]string
		alive  func(int) bool
		report bool
		kept   bool
	}{
		{name: "interrupted", files: map[string]string{session.TranscriptFile: twoTurns},
			alive: func(int) bool { return false }, report: true, kept: true},
		{name: "still running", files: map[string]string{session.TranscriptFile: twoTurns, procs.PIDFile: "4242\n"},
			alive: func(int) bool { return true }, kept: true},
		{name: "finished after all", files: map[string]string{session.TranscriptFile: twoTurns, session.ResultFile: "{}"},
			alive: func(int) bool { return false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, devOnlyTOML)
			killedSession(t, h, 1, config.RoleDeveloper, "developer-issue-1-r1", tc.files)
			h.sched.alive = tc.alive
			bk, err := h.store.Issue(1)
			if err != nil {
				t.Fatal(err)
			}
			in := h.sched.takeInterrupted(h.sched.log, &bk)
			if (in != nil) != tc.report {
				t.Errorf("reported %+v, want a report: %v", in, tc.report)
			}
			stored, err := h.store.Issue(1)
			if err != nil {
				t.Fatal(err)
			}
			if (stored.Session != nil) != tc.kept {
				t.Errorf("the record on disk is %+v, want kept: %v", stored.Session, tc.kept)
			}
			if (bk.Session != nil) != tc.kept {
				t.Errorf("the caller's copy holds %+v, want kept: %v", bk.Session, tc.kept)
			}
		})
	}
}

// TestAFirstRunReportsNoInterruption: with nothing recorded — every issue
// before this version, and every issue of a factory that was never killed —
// the prompt is the one it always was.
func TestAFirstRunReportsNoInterruption(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	runPass(t, h)

	if prompt := promptOf(t, h, 0); strings.Contains(prompt, "never reported an outcome") {
		t.Errorf("an interruption was reported out of nothing:\n%s", prompt)
	}
	// The session that ran cleared its own record, so the next worker for
	// this issue has nothing to report either.
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.Session != nil {
		t.Errorf("a session that finished is still recorded as running: %+v", bk.Session)
	}
}

// TestAnInterruptedReviewerSessionIsReportedToTheReviewer: the role that was
// interrupted is the role that hears about it. A developer session started
// for the same issue is not told about a reviewer's transcript — it would
// learn nothing it could act on — and the reviewer that follows is.
func TestAnInterruptedReviewerSessionIsReportedToTheReviewer(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	killedSession(t, h, 1, config.RoleReviewer, "reviewer-pr-101-r1", map[string]string{
		session.TranscriptFile: twoTurns,
	})
	h.sched.alive = func(int) bool { return false }
	runPass(t, h)

	names := h.sessionNames()
	if len(names) < 2 || !strings.HasPrefix(names[0], "developer") || !strings.HasPrefix(names[1], "reviewer") {
		t.Fatalf("fixture ran %v, want a developer then a reviewer", names)
	}
	if dev := promptOf(t, h, 0); strings.Contains(dev, "never reported an outcome") {
		t.Errorf("the developer was told about a reviewer's interrupted session:\n%s", dev)
	}
	review := promptOf(t, h, 1)
	if !strings.Contains(flowedPrompt(review), "reviewer session that ran for this issue before you was interrupted after 2 turns") {
		t.Errorf("the reviewer was not told about its own interrupted session:\n%s", review)
	}
	// And told what a reviewer can act on: the round starts over. The branch
	// advice belongs to the developer — a reviewer session commits nothing.
	if strings.Contains(flowedPrompt(review), "The branch may carry work it never reported") {
		t.Errorf("the reviewer was given the developer's branch advice:\n%s", review)
	}
	if !strings.Contains(flowedPrompt(review), "It reported no verdict, so this round starts over") {
		t.Errorf("the reviewer was not told its round starts over:\n%s", review)
	}
}

// TestAResumedWorkerIsMarkedInTheStatus: `bees status` must tell a worker
// that took over from an interrupted session from one that started fresh,
// because the branch of the first may already carry work. The status file is
// read the moment the worker reports its first stage — after the run it holds
// no workers at all, the pass being over.
func TestAResumedWorkerIsMarkedInTheStatus(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	killedSession(t, h, 1, config.RoleDeveloper, "developer-issue-1-r1", map[string]string{
		session.TranscriptFile: twoTurns,
	})
	h.sched.alive = func(int) bool { return false }

	sub := h.sched.Subscribe()
	var mu sync.Mutex
	var workers []state.Worker
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range sub {
			if ev.Kind != EventStage {
				continue
			}
			st, err := h.store.LoadStatus()
			if err != nil {
				continue
			}
			mu.Lock()
			workers = append(workers, st.Workers...)
			mu.Unlock()
		}
	}()
	runPass(t, h)

	mu.Lock()
	defer mu.Unlock()
	if len(workers) == 0 {
		t.Fatal("no worker was in status.json while the pass ran")
	}
	for _, w := range workers {
		if !w.Resumed {
			t.Errorf("worker %s (issue %d, stage %s) is not marked resumed", w.Name, w.Issue, w.Stage)
		}
	}
}

// flowedPrompt joins a rendered prompt onto one line, so an assertion does
// not depend on where a sentence happens to wrap.
func flowedPrompt(s string) string { return strings.Join(strings.Fields(s), " ") }

// TestTheRunningSessionIsRecordedWhileItRuns: the record is the whole
// mechanism — a scheduler that is killed writes nothing on its way out, so
// the session it was running has to already be on disk, and has to be gone
// again the moment the session ends. The fake session copies the issue's
// bookkeeping as it finds it, which is the only way to observe the record
// while it is meant to exist.
func TestTheRunningSessionIsRecordedWhileItRuns(t *testing.T) {
	t.Setenv("FAKE_COPY_ISSUE_STATE", "1")
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	runPass(t, h)

	first := h.sessionOrder()[0]
	b, err := os.ReadFile(filepath.Join(h.store.Dir, "running-"+first+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var during state.IssueState
	if err := json.Unmarshal(b, &during); err != nil {
		t.Fatal(err)
	}
	if during.Session == nil {
		t.Fatalf("the session running for issue 1 was not recorded while it ran: %s", b)
	}
	if during.Session.Role != config.RoleDeveloper || during.Session.Name != "developer-issue-1-r1" {
		t.Errorf("the record names %+v, want the developer session that was running", during.Session)
	}
	if filepath.Base(during.Session.Dir) != first {
		t.Errorf("the record points at %q, want the session directory %q", during.Session.Dir, first)
	}
	if during.Session.StartedAt.IsZero() {
		t.Errorf("the record carries no start time: %+v", during.Session)
	}
	after, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if after.Session != nil {
		t.Errorf("a session that ended is still recorded as running: %+v", after.Session)
	}
}

// TestAnInterruptionSurvivesAWorkerThatRanNoSession: the record is the only
// thing that remembers a killed session, so a worker that returns before it
// starts a session must not spend it on the way out. One failing `gh issue
// edit` on the pass that takes the issue over is enough: no session is told,
// and without the record no later session can be either, while the branch
// still carries the work the killed session never reported.
func TestAnInterruptionSurvivesAWorkerThatRanNoSession(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	killedSession(t, h, 1, config.RoleDeveloper, "developer-issue-1-r1", map[string]string{
		session.TranscriptFile: twoTurns,
	})
	h.sched.alive = func(int) bool { return false }
	h.gh.errFor["issue edit"] = errors.New("gh: API rate limit exceeded")
	runPass(t, h)

	if names := h.sessionNames(); len(names) != 0 {
		t.Fatalf("the fixture ran %v, want a worker that returns before it starts a session", names)
	}
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.Session == nil {
		t.Fatal("the interruption was consumed by a worker that told no session about it")
	}
}
