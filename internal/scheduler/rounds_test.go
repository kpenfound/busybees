package scheduler

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/state"
)

// argsOfNamed reads the command line the runner built for a session, from
// the args.txt the fake agent wrote, by the session's exact name.
func argsOfNamed(t *testing.T, h *harness, name string) []string {
	t.Helper()
	for i, n := range h.sessionNames() {
		if n == name {
			return argsOf(t, filepath.Join(h.store.SessionsDir(), h.sessionOrder()[i]))
		}
	}
	t.Fatalf("no session %q ran; sessions: %v", name, h.sessionNames())
	return nil
}

// resumeOf returns the id a session was launched to resume, or "" when it
// was launched fresh.
func resumeOf(t *testing.T, h *harness, name string) string {
	t.Helper()
	args := argsOfNamed(t, h, name)
	i := slices.Index(args, "--resume")
	if i < 0 {
		if slices.Contains(args, "--system-prompt-snapshot") {
			t.Errorf("%s: --system-prompt-snapshot passed to a fresh session:\n%s", name, strings.Join(args, "\n"))
		}
		return ""
	}
	if i+1 >= len(args) {
		t.Fatalf("%s: --resume with no id:\n%s", name, strings.Join(args, "\n"))
	}
	// The snapshot is what would keep the resumed session's system prompt
	// at what round 1 said: the two flags travel together.
	if j := slices.Index(args, "--system-prompt-snapshot"); j < 0 || j+1 >= len(args) || args[j+1] != "off" {
		t.Errorf("%s: resumed without --system-prompt-snapshot off:\n%s", name, strings.Join(args, "\n"))
	}
	return args[i+1]
}

// TestASecondRoundResumesTheFirstRoundsSession is the point of #494: a
// developer handed back review feedback, and the reviewer looking at the
// fix, continue the conversation they had in round 1 instead of relearning
// the codebase. Round 1 of either role runs fresh; round 2 of each resumes
// its own role's round 1 and not the other's.
func TestASecondRoundResumesTheFirstRoundsSession(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	// The fake reviewer requests changes on its first review and approves
	// the second, so each role runs two rounds.
	runPass(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1", "developer-issue-1-r2", "reviewer-pr-201-r2")
	if got := resumeOf(t, h, "developer-issue-1-r1"); got != "" {
		t.Errorf("round 1 of the developer resumed %q, want a fresh session", got)
	}
	if got := resumeOf(t, h, "reviewer-pr-201-r1"); got != "" {
		t.Errorf("round 1 of the reviewer resumed %q, want a fresh session", got)
	}
	if got, want := resumeOf(t, h, "developer-issue-1-r2"), "sid-developer-issue-1-r1"; got != want {
		t.Errorf("round 2 of the developer resumed %q, want %q", got, want)
	}
	if got, want := resumeOf(t, h, "reviewer-pr-201-r2"), "sid-reviewer-pr-201-r1"; got != want {
		t.Errorf("round 2 of the reviewer resumed %q, want %q", got, want)
	}
	if last := h.gh.history[1][len(h.gh.history[1])-1]; last != "bees:approved" {
		t.Fatalf("history: %v", h.gh.history[1])
	}
}

// TestACodexRoundIsNeverResumed: codex exec has nothing to resume with, so
// a codex role's second round is launched like its first, whatever id the
// worker knows from round 1.
func TestACodexRoundIsNeverResumed(t *testing.T) {
	h := newHarness(t, devOnlyTOML+"[global]\nagent = \"codex\"\n")
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	runPass(t, h)

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1", "developer-issue-1-r2", "reviewer-pr-201-r2")
	for _, name := range []string{"developer-issue-1-r2", "reviewer-pr-201-r2"} {
		args := argsOfNamed(t, h, name)
		if len(args) < 2 || args[1] != "exec" {
			t.Fatalf("%s did not run codex exec: %v", name, args)
		}
		for _, a := range args {
			if a == "--resume" || strings.HasPrefix(a, "resume") || a == "--system-prompt-snapshot" {
				t.Errorf("%s: codex was passed %q:\n%s", name, a, strings.Join(args, "\n"))
			}
		}
	}
}

// TestAFailedResumeIsRetriedFresh: a session id claude no longer has makes
// the resumed launch die before any result event. That is an infrastructure
// failure like any other, retried under scheduler.retries, and the retry
// drops the id: it runs as the fresh session the round would have had before
// #494, and the loop goes on from its result.
func TestAFailedResumeIsRetriedFresh(t *testing.T) {
	t.Setenv("FAKE_RESUME_FAIL", "1")
	h := newHarness(t, strings.Replace(devOnlyTOML, "[scheduler]\n", "[scheduler]\nretries = 1\nretry_delay = \"0s\"\n", 1))
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	runPass(t, h)

	// Round 2 of each role is launched resumed, dies, and is retried fresh.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1",
		"developer-issue-1-r2", "developer-issue-1-r2-retry1",
		"reviewer-pr-201-r2", "reviewer-pr-201-r2-retry1")
	if got, want := resumeOf(t, h, "developer-issue-1-r2"), "sid-developer-issue-1-r1"; got != want {
		t.Errorf("the failed attempt resumed %q, want %q", got, want)
	}
	if got := resumeOf(t, h, "developer-issue-1-r2-retry1"); got != "" {
		t.Errorf("the retry resumed %q, want a fresh session", got)
	}
	if got := resumeOf(t, h, "reviewer-pr-201-r2-retry1"); got != "" {
		t.Errorf("the reviewer's retry resumed %q, want a fresh session", got)
	}
	// The failure was classified as infrastructure: the loop finished
	// instead of escalating, and the issue was approved.
	if last := h.gh.history[1][len(h.gh.history[1])-1]; last != "bees:approved" {
		t.Fatalf("history: %v", h.gh.history[1])
	}
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("unexpected escalation: %v", h.gh.comments[1])
	}
}

// TestAWorkerStartedAfterARestartResumesNoSession: the id lives only as
// long as the worker that learned it. A `bees run` restarted between rounds
// finds the issue's bookkeeping, an open pull request and round 1's
// result.json with its session id on disk, and starts the round fresh all
// the same: the new worker has a new worktree, and the old conversation's
// paths point into the one that is gone.
func TestAWorkerStartedAfterARestartResumesNoSession(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedIssue(h, 1, "bees:in-progress", "s", time.Now().Add(-time.Hour))
	pushBranch(t, h.clone, "bees/issue-1")
	// What the last scheduler left: the loop in the develop stage of round
	// 2, and round 1's sessions with their ids recorded.
	if err := h.store.SaveIssue(state.IssueState{Number: 1, Round: 2, PR: 201, Branch: "bees/issue-1",
		WorkerStage: "develop", AfterDevelop: "review", PreReviewDone: true}); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"developer-issue-1-r1", "reviewer-pr-201-r1"} {
		dir := filepath.Join(h.store.SessionsDir(), "20260101-000000-"+name+"-old")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "result.json"), []byte(`{"name":"`+name+`","claude_session_id":"sid-`+name+`"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	seedCounter(t, h, "review", 1) // the reviewer approves round 2
	runPass(t, h)

	h.wantOrder("developer-issue-1-r2", "reviewer-pr-201-r2")
	if got := resumeOf(t, h, "developer-issue-1-r2"); got != "" {
		t.Errorf("the developer of a restarted worker resumed %q, want a fresh session", got)
	}
	if got := resumeOf(t, h, "reviewer-pr-201-r2"); got != "" {
		t.Errorf("the reviewer of a restarted worker resumed %q, want a fresh session", got)
	}
	if !slices.Contains(h.gh.history[1], "bees:approved") {
		t.Fatalf("history: %v", h.gh.history[1])
	}
}
