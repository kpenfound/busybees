package scheduler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/workspace"
)

func issue(n int, body string) github.Issue {
	return github.Issue{Number: n, Body: body}
}

func TestWaitingOn(t *testing.T) {
	open := map[int]bool{2: true, 3: true, 5: true}
	for _, c := range []struct {
		name  string
		issue github.Issue
		want  []int
	}{
		{"no declaration", issue(1, "just work"), nil},
		{"open blocker", issue(1, "Blocked by #2"), []int{2}},
		{"closed blocker is not a blocker", issue(1, "Blocked by #9"), nil},
		{"mixed", issue(1, "depends on: #2, #9 and #3"), []int{2, 3}},
		{"self-reference dropped", issue(5, "Blocked by #5 and #2"), []int{2}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := waitingOn(c.issue, open); fmt.Sprint(got) != fmt.Sprint(c.want) {
				t.Fatalf("waitingOn = %v, want %v", got, c.want)
			}
		})
	}
}

// A cycle would hold both issues back forever, so the scheduler ignores the
// declared dependencies of every issue in one and warns once per issue.
func TestFillWaitingCycle(t *testing.T) {
	var buf bytes.Buffer
	s := &Scheduler{
		cfg:          &config.Config{},
		log:          slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
		warnedCycles: map[int]bool{},
	}
	byNumber := map[int]github.Issue{
		1: issue(1, "Blocked by #2"),
		2: issue(2, "Blocked by #1"),
		3: issue(3, "Blocked by #1"), // not itself in the cycle: still held
	}
	snap := &snapshot{
		open:    map[int]bool{1: true, 2: true, 3: true},
		waiting: map[int][]int{},
		byState: map[string][]github.Issue{"ready": {byNumber[1], byNumber[2], byNumber[3]}},
	}
	s.fillWaiting(context.Background(), snap, byNumber)
	if len(snap.waiting[1]) != 0 || len(snap.waiting[2]) != 0 {
		t.Fatalf("issues in a cycle must be dispatchable: %v", snap.waiting)
	}
	if fmt.Sprint(snap.waiting[3]) != "[1]" {
		t.Fatalf("#3 is not in the cycle and must still wait: %v", snap.waiting)
	}
	if n := strings.Count(buf.String(), "dependency cycle"); n != 2 {
		t.Fatalf("want one warning per issue in the cycle, got %d:\n%s", n, buf.String())
	}
	// Warnings are once per process, not once per poll.
	buf.Reset()
	snap.waiting = map[int][]int{}
	s.fillWaiting(context.Background(), snap, byNumber)
	if strings.Contains(buf.String(), "dependency cycle") {
		t.Fatalf("cycle warned twice:\n%s", buf.String())
	}
	if len(snap.waiting[1]) != 0 || len(snap.waiting[2]) != 0 {
		t.Fatalf("cycle still ignored on the second pass: %v", snap.waiting)
	}
}

func TestBlockerCycleSelfReference(t *testing.T) {
	byNumber := map[int]github.Issue{1: issue(1, "Blocked by #1")}
	if blockerCycle(1, byNumber) {
		t.Fatal("a pure self-reference is dropped by waitingOn, not a cycle")
	}
}

const devOnlyTOML = baseTOML + `
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
`

// A ready issue that declares an open blocker is not dispatched, and is not
// relabelled either: it just waits. When the blocker closes it goes out on
// the next pass.
func TestDependencyHoldsReadyIssue(t *testing.T) {
	h := newHarnessAt(t, devOnlyTOML, time.Now())
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true} // reviewer disabled: PR auto-approved
	// #1 is older, so it is first in the ready queue: skipping it must not
	// cost #2 its pool slot.
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Dependent", Body: "Blocked by #2\n\nDo the thing.", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/m"}}, CreatedAt: time.Now().Add(-time.Hour)}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "Prerequisite", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/m"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-2", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.history[1]; len(got) != 0 {
		t.Fatalf("#1 is blocked by open #2 and must not be touched: %v", got)
	}
	if got := strings.Join(h.gh.history[2], ","); got != "bees:in-progress,bees:approved" {
		t.Fatalf("#2 history: %s", got)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 1 {
		t.Fatalf("developer sessions: %d want 1", n)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(st.WaitingOnDeps) != "map[1:[2]]" {
		t.Fatalf("status waiting_on_deps: %v", st.WaitingOnDeps)
	}

	// #2 closes: #1 is dispatched on the next poll, with no label change in
	// between. A local pass in the meantime still sees the cached, open #2.
	h.gh.issues[2].State = "CLOSED"
	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.gh.history[1]; len(got) == 0 || got[0] != "bees:in-progress" {
		t.Fatalf("#1 should have been dispatched once #2 closed: %v", got)
	}
	if st, err = h.store.LoadStatus(); err != nil {
		t.Fatal(err)
	} else if len(st.WaitingOnDeps) != 0 {
		t.Fatalf("nothing is waiting any more: %v", st.WaitingOnDeps)
	}
}

// A blocker the factory cannot see (closed, or outside the filter) blocks
// nothing.
func TestInvisibleBlockerDoesNotHold(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Dependent", Body: "Blocked by #404", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/m"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:approved" {
		t.Fatalf("#1 history: %s", got)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.WaitingOnDeps) != 0 {
		t.Fatalf("an invisible blocker must not hold anything: %v", st.WaitingOnDeps)
	}
}

// The project manager's prompt shows the open blockers of every work item.
func TestProjectManagerSeesBlockers(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n")
	h.sched.OnlyRoles = map[string]bool{config.RoleProjectManager: true}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Needs triage", Body: "Blocked by #2\n\nvague", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "Prerequisite", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/m"}}, CreatedAt: time.Now()}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	sessions := h.sessions(config.RoleProjectManager)
	if len(sessions) != 1 {
		t.Fatalf("project manager sessions: %d", len(sessions))
	}
	prompt := readFile(t, sessions[0]+"/prompt.md")
	if !strings.Contains(prompt, "blocked by: #2 (open)") {
		t.Fatalf("triage header missing blockers:\n%s", prompt)
	}
	if !strings.Contains(prompt, "| # | State | Kind | Blocked by | Milestone | Title |") {
		t.Fatalf("issue table missing the Blocked by column:\n%s", prompt)
	}
}

// warnCycle is called from poll, which may run while workers hold the lock.
func TestWarnCycleIsConcurrencySafe(t *testing.T) {
	var buf bytes.Buffer
	s := &Scheduler{log: slog.New(slog.NewTextHandler(&buf, nil)), warnedCycles: map[int]bool{}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); s.warnCycle(7) }()
	}
	wg.Wait()
	if n := strings.Count(buf.String(), "dependency cycle"); n != 1 {
		t.Fatalf("want exactly one warning, got %d", n)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// stackedTOML is devOnlyTOML with scheduler.stacked_prs on.
var stackedTOML = strings.Replace(devOnlyTOML, "max_review_rounds = 3\n", "max_review_rounds = 3\nstacked_prs = true\n", 1)

// Under scheduler.stacked_prs a blocker holds an issue back until it can be
// stacked on: it must have an open pull request and be a sub-issue of the
// same feature. Everything else waits exactly as before, and the parent
// lookups are only paid for a blocker with a pull request.
func TestStackedPRsRelaxTheHold(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stacked bool
		parents map[int]int
		pr      bool
		err     error
		waits   bool
		lookups int
	}{
		{"off: an open blocker with a PR still holds", false, map[int]int{1: 5, 2: 5}, true, nil, true, 0},
		{"on, same feature, open PR: dispatchable", true, map[int]int{1: 5, 2: 5}, true, nil, false, 2},
		{"on, same feature, no PR yet: holds", true, map[int]int{1: 5, 2: 5}, false, nil, true, 0},
		{"on, another feature: holds", true, map[int]int{1: 5, 2: 6}, true, nil, true, 2},
		{"on, the blocker has no parent: holds", true, map[int]int{1: 5}, true, nil, true, 2},
		{"on, the issue has no parent: holds", true, map[int]int{2: 5}, true, nil, true, 1},
		{"on, parent lookup fails: holds", true, map[int]int{1: 5, 2: 5}, true, fmt.Errorf("graphql: boom"), true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			toml := devOnlyTOML
			if tc.stacked {
				toml = stackedTOML
			}
			h := newHarness(t, toml)
			h.gh.parents = tc.parents
			if tc.err != nil {
				h.gh.parentErr = map[int]error{1: tc.err}
			}
			issues := []github.Issue{
				{Number: 1, Body: "Blocked by #2", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}},
				{Number: 2, State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}},
			}
			var prs []github.PR
			if tc.pr {
				prs = []github.PR{{Number: 7, State: "OPEN", HeadRefName: "bees/issue-2", BaseRefName: "main"}}
			}
			snap := h.sched.classify(context.Background(), issues, prs)
			if got := len(snap.waiting[1]) > 0; got != tc.waits {
				t.Fatalf("waiting[1] = %v, want waits=%v", snap.waiting[1], tc.waits)
			}
			if got := h.gh.callCount("api graphql"); got != tc.lookups {
				t.Fatalf("parent lookups: %d, want %d", got, tc.lookups)
			}
		})
	}
}

// With stacking on, a work item blocked by another one under the same
// feature is dispatched as soon as the blocker's pull request is open, its
// branch is cut from the blocker's branch rather than the default branch,
// and its developer is told to target and merge that branch. The predecessor
// itself, blocked by nothing, is built from the default branch as before.
func TestStackedPRsBuildOnThePredecessorBranch(t *testing.T) {
	h := newHarnessAt(t, stackedTOML, time.Now())
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true} // reviewer disabled: PR auto-approved
	h.gh.parents = map[int]int{1: 5, 2: 5}
	// #1 is older, so it is first in the ready queue and would go out first
	// if the hold were relaxed before #2 has a pull request.
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Second step", Body: "Blocked by #2\n\nBuild on it.", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/m"}}, CreatedAt: time.Now().Add(-time.Hour)}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "First step", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/m"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-2", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}}
	h.gh.prs[201] = &github.PR{Number: 201, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "bees/issue-2",
		Labels: []github.Label{{Name: "bees"}}}
	h.gh.hidden[201] = true // opened by #1's developer session

	// Pass 1: #2 has no pull request yet, so #1 waits and #2 goes out.
	runPass(t, h)
	if got := h.gh.history[1]; len(got) != 0 {
		t.Fatalf("#1 has nothing to stack on yet and must wait: %v", got)
	}
	if got := strings.Join(h.gh.history[2], ","); got != "bees:in-progress,bees:approved" {
		t.Fatalf("#2 history: %s", got)
	}
	if !strings.Contains(systemPromptOf(t, h, 0), "--base main --head bees/issue-2") {
		t.Fatalf("#2 is the bottom of the stack and targets the default branch:\n%s", systemPromptOf(t, h, 0))
	}

	// Pass 2: #2 is still open (approved, not merged) but its pull request
	// exists, so #1 is dispatched and stacked on bees/issue-2.
	forcePoll(h)
	runPass(t, h)
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:approved" {
		t.Fatalf("#1 should have been dispatched once #2's PR opened: %s", got)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 2 {
		t.Fatalf("developer sessions: %d want 2", n)
	}
	sys := systemPromptOf(t, h, 1)
	for _, want := range []string{"--base bees/issue-2 --head bees/issue-1", "git fetch origin && git merge origin/bees/issue-2", "not the local `bees/issue-2` branch", "the branch of the work item yours is"} {
		if !strings.Contains(sys, want) {
			t.Errorf("#1's developer system prompt missing %q:\n%s", want, sys)
		}
	}
	if strings.Contains(sys, "--base main") || strings.Contains(sys, "merge origin/main") {
		t.Errorf("#1's developer system prompt still targets the default branch:\n%s", sys)
	}
	task := promptOf(t, h, 1)
	if !strings.Contains(task, "based on `bees/issue-2`") || !strings.Contains(task, "stacked on it") {
		t.Errorf("#1's developer task does not say it is stacked:\n%s", task)
	}
	// The branch really was cut from the predecessor's: it carries the
	// predecessor's commit (work-1.txt, from the first fake session) under
	// its own (work-2.txt), and the default branch has neither.
	ctx := context.Background()
	if _, err := workspace.Git(ctx, h.clone, "fetch", "-q", "origin"); err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Git(ctx, h.clone, "merge-base", "--is-ancestor", "origin/bees/issue-2", "origin/bees/issue-1"); err != nil {
		t.Fatalf("bees/issue-1 was not cut from bees/issue-2: %v", err)
	}
	files, err := workspace.Git(ctx, h.clone, "ls-tree", "--name-only", "origin/bees/issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(files, "work-1.txt") || !strings.Contains(files, "work-2.txt") {
		t.Fatalf("bees/issue-1 should carry both steps' work: %s", files)
	}
	if st, err := h.store.LoadStatus(); err != nil {
		t.Fatal(err)
	} else if len(st.WaitingOnDeps) != 0 {
		t.Fatalf("nothing is waiting any more: %v", st.WaitingOnDeps)
	}
}

// A predecessor whose pull request merged is not stacked on, even with its
// issue still declared as a blocker: its branch is in the default branch and
// may be gone from the remote, so the work item is cut from the default
// branch, as it would be without stacking.
func TestStackedPRsFallBackToTheDefaultBranchOnceThePredecessorMerged(t *testing.T) {
	h := newHarnessAt(t, stackedTOML, time.Now())
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	h.gh.parents = map[int]int{1: 5, 2: 5}
	// #2 is closed: not in the poll, so it holds #1 back no longer; its
	// branch was deleted on merge, and no open pull request names it.
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Second step", Body: "Blocked by #2\n\nBuild on it.", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/m"}}, CreatedAt: time.Now()}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "First step", State: "CLOSED",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:approved"}, {Name: "bees:size/m"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}}

	runPass(t, h)
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:approved" {
		t.Fatalf("#1 history: %s", got)
	}
	if sys := systemPromptOf(t, h, 0); !strings.Contains(sys, "--base main --head bees/issue-1") {
		t.Fatalf("#1 should target the default branch:\n%s", sys)
	}
}

// stackedPR is the pull request #1's developer opens on top of #2's branch
// in the tests below.
const stackedPR = 201

// seedStack sets up a two-step stack under feature #5: #1 is blocked by #2,
// #2's pull request (fakePR, on bees/issue-2) is open, and #2 itself is
// blocked on a question to the project manager, so no worker takes it and
// the test alone decides when it is approved. #1's developer opens stackedPR
// on bees/issue-2.
func seedStack(t *testing.T, h *harness) {
	t.Helper()
	h.gh.parents = map[int]int{1: 5, 2: 5}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Second step", Body: "Blocked by #2\n\nBuild on it.", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/m"}}, CreatedAt: time.Now().Add(-time.Hour)}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "First step", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:blocked"}, {Name: "bees:size/m"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-2", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}}
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	pushBranch(t, h.clone, "bees/issue-2") // #2's developer pushed its branch
	h.gh.prs[stackedPR] = &github.PR{Number: stackedPR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "bees/issue-2",
		Labels: []github.Label{{Name: "bees"}}}
	h.gh.hidden[stackedPR] = true
}

// waitForStage waits until #1's worker has recorded the stage in the issue's
// bookkeeping and reports it in the status file.
func waitForStage(t *testing.T, h *harness, stage string) {
	t.Helper()
	waitFor(t, 10*time.Second, "the worker to reach "+stage, func() bool {
		bk, err := h.store.Issue(1)
		if err != nil || bk.WorkerStage != stage {
			return false
		}
		st, err := h.store.LoadStatus()
		return err == nil && len(st.Workers) == 1 && st.Workers[0].Stage == stage
	})
}

// waitWorkers waits for every worker the pass started to finish, and fails
// the test — cancelling the workers — when one is still running after d: a
// stack-wait that never clears would otherwise hang the test instead of
// failing it.
func waitWorkers(t *testing.T, h *harness, cancel context.CancelFunc, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() { h.sched.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		cancel()
		<-done
		t.Fatalf("a worker was still running after %s", d)
	}
}

// labelApproved gives an issue the label approve() would, as the
// predecessor's own worker does when its review passes.
func labelApproved(h *harness, n int) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	h.gh.issues[n].Labels = append(h.gh.issues[n].Labels, github.Label{Name: "bees:approved"})
}

// A stacked pull request whose own review passed is not approved — no label
// on the pull request or the issue, nothing for the Approved PRs panel, and
// no post-approval checks — while the pull request beneath it is not. The
// worker holds its slot in the stack-wait stage instead, and the moment the
// predecessor is approved it approves this one, without another review.
func TestAStackedPullRequestWaitsForThePredecessorsApproval(t *testing.T) {
	h := newHarnessAt(t, stackedTOML, time.Now())
	seedStack(t, h)
	seedCounter(t, h, "review", 1) // the reviewer approves on the first round
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitForStage(t, h, "stack-wait")

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1")
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:review" {
		t.Fatalf("#1 must not be approved while #2 is not: %s", got)
	}
	h.gh.mu.Lock()
	prLabels := append([]github.Label(nil), h.gh.prs[stackedPR].Labels...)
	issues := []github.Issue{*h.gh.issues[1], *h.gh.issues[2]}
	prs := []github.PR{*h.gh.prs[fakePR], *h.gh.prs[stackedPR]}
	h.gh.mu.Unlock()
	if github.HasLabel(prLabels, "bees:approved") {
		t.Fatalf("the stacked pull request is labelled approved: %v", prLabels)
	}
	if got := h.sched.approvedPRs(h.sched.classify(ctx, issues, prs)); len(got) != 0 {
		t.Fatalf("nothing is waiting for a person to merge it: %v", got)
	}

	labelApproved(h, 2)
	waitWorkers(t, h, cancel, 10*time.Second)
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:review,bees:approved" {
		t.Fatalf("#1 history after #2's approval: %s", got)
	}
	if !github.HasLabel(h.gh.prs[stackedPR].Labels, "bees:approved") {
		t.Fatalf("the stacked pull request was not labelled: %v", h.gh.prs[stackedPR].Labels)
	}
	// The wait ended in an approval, not a fresh review round.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1")
	if len(h.gh.merged) != 0 {
		t.Fatalf("auto_merge is off; nothing should be merged: %v", h.gh.merged)
	}
}

// The reviewer-disabled path, where a pull request is treated as approved
// the moment it opens, waits for the stack the same way. And a predecessor
// that will never be approved ends the wait: its issue closed with the pull
// request still open, or closed unmerged, and the stacked pull request
// targets a branch nobody approved — escalate. A predecessor a person merged
// by hand, without the label, is beneath the stack no longer: the pull
// request is approved.
func TestAStackedPullRequestWhosePredecessorClosesUnapproved(t *testing.T) {
	for _, tc := range []struct {
		name   string
		closed func(h *harness)
		want   string
	}{
		{"the predecessor's pull request is still open", func(h *harness) {}, "bees:in-progress,bees:needs-human"},
		{"the predecessor's pull request closed unmerged", func(h *harness) { h.gh.prs[fakePR].State = "CLOSED" }, "bees:in-progress,bees:needs-human"},
		{"the predecessor's pull request merged", func(h *harness) {
			now := time.Now()
			h.gh.prs[fakePR].State = "MERGED"
			h.gh.prs[fakePR].MergedAt = &now
		}, "bees:in-progress,bees:approved"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarnessAt(t, stackedTOML, time.Now())
			h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true} // reviewer disabled
			seedStack(t, h)
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			if err := h.sched.pass(ctx); err != nil {
				t.Fatal(err)
			}
			waitForStage(t, h, "stack-wait")
			if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress" {
				t.Fatalf("#1 must not be approved while #2 is not: %s", got)
			}

			h.gh.mu.Lock()
			h.gh.issues[2].State = "CLOSED"
			tc.closed(h)
			h.gh.mu.Unlock()
			waitWorkers(t, h, cancel, 10*time.Second)
			if got := strings.Join(h.gh.history[1], ","); got != tc.want {
				t.Fatalf("#1 history: %s, want %s", got, tc.want)
			}
			if tc.want == "bees:in-progress,bees:needs-human" {
				if c := h.gh.comments[1]; len(c) != 1 || !strings.Contains(c[0], "stacked on #2's pull request #101") || !strings.Contains(c[0], "#2 closed without being approved") {
					t.Fatalf("escalation comment: %v", c)
				}
			} else if len(h.gh.comments[1]) != 0 {
				t.Fatalf("no escalation expected: %v", h.gh.comments[1])
			}
			h.wantOrder("developer-issue-1-r1")
		})
	}
}

// A scheduler killed while a worker waits for the stack comes back into the
// same wait: the label still says bees:review, which alone would restart the
// review, and the recorded stage is what says the review has already passed.
// The first run dies on a poll of the predecessor; the second polls on and
// approves the pull request once the predecessor is, with no session at all.
func TestAWorkerKilledInStackWaitResumesInIt(t *testing.T) {
	h := newHarnessAt(t, stackedTOML, time.Now())
	seedStack(t, h)
	seedCounter(t, h, "review", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitForStage(t, h, "stack-wait")
	h.gh.mu.Lock()
	h.gh.errFor["issue view"] = fmt.Errorf("gh: could not reach github") // the poll the scheduler dies on
	h.gh.mu.Unlock()
	waitWorkers(t, h, cancel, 10*time.Second)
	h.gh.mu.Lock()
	delete(h.gh.errFor, "issue view")
	h.gh.mu.Unlock()

	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1")
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.WorkerStage != "stack-wait" || bk.PR != stackedPR {
		t.Fatalf("bookkeeping after the crash: %+v", bk)
	}
	if got := h.stateOfIssue(1); got != "review" {
		t.Fatalf("issue state label after the crash is %q", got)
	}

	// Restart. The failed worker set a backoff on the issue; a real restart
	// is a new process, so step over it. #2 is still not approved, so the
	// resumed worker must wait again rather than review again.
	h.clock.advance(6 * h.cfg.Scheduler.PollInterval.Duration)
	forcePoll(h)
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitForStage(t, h, "stack-wait")
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1")
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:review" {
		t.Fatalf("#1 history after the restart: %s", got)
	}

	labelApproved(h, 2)
	waitWorkers(t, h, cancel, 10*time.Second)
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:review,bees:approved" {
		t.Fatalf("#1 history after #2's approval: %s", got)
	}
	// Not one extra session: the review has already happened and is paid for.
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1")
}

// A worker resumed into stack-wait after the predecessor's pull request
// merged finds nothing to stack on any more — the default branch has the
// predecessor, and this pull request targets it — and approves at once.
func TestAWorkerResumedIntoStackWaitAfterThePredecessorMergedApproves(t *testing.T) {
	h := newHarnessAt(t, stackedTOML, time.Now())
	seedStack(t, h)
	seedCounter(t, h, "review", 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitForStage(t, h, "stack-wait")
	h.gh.mu.Lock()
	h.gh.errFor["issue view"] = fmt.Errorf("gh: could not reach github")
	h.gh.mu.Unlock()
	waitWorkers(t, h, cancel, 10*time.Second)

	// While the scheduler was down a person merged #2 by hand and its issue
	// closed, never labelled approved; GitHub retargeted #201 at main.
	h.gh.mu.Lock()
	delete(h.gh.errFor, "issue view")
	now := time.Now()
	h.gh.issues[2].State = "CLOSED"
	h.gh.prs[fakePR].State, h.gh.prs[fakePR].MergedAt = "MERGED", &now
	h.gh.prs[stackedPR].BaseRefName = "main"
	h.gh.mu.Unlock()

	h.clock.advance(6 * h.cfg.Scheduler.PollInterval.Duration)
	forcePoll(h)
	runPass(t, h)
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:review,bees:approved" {
		t.Fatalf("#1 history: %s", got)
	}
	h.wantOrder("developer-issue-1-r1", "reviewer-pr-201-r1")
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("no escalation expected: %v", h.gh.comments[1])
	}
}

// The wait polls the predecessor at roles.reviewer.checks_poll_interval, the
// cadence the checks stage already uses, not on a loop of its own: with the
// interval at an hour the predecessor is read once, and an approval that
// lands afterwards goes unnoticed until the next poll.
func TestStackWaitPollsAtTheChecksPollInterval(t *testing.T) {
	h := newHarnessAt(t, stackedTOML+"[roles.reviewer]\nchecks_poll_interval = \"1h\"\n", time.Now())
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	seedStack(t, h)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitForStage(t, h, "stack-wait")
	polls := func() int {
		h.gh.mu.Lock()
		defer h.gh.mu.Unlock()
		n := 0
		for _, c := range h.gh.calls {
			if len(c) >= 3 && c[0] == "issue" && c[1] == "view" && c[2] == "2" {
				n++
			}
		}
		return n
	}
	waitFor(t, 10*time.Second, "the first poll of #2", func() bool { return polls() >= 1 })
	before := polls()
	labelApproved(h, 2)
	time.Sleep(300 * time.Millisecond)
	if got := polls(); got != before {
		t.Fatalf("#2 was polled %d more times inside checks_poll_interval", got-before)
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress" {
		t.Fatalf("#1 was approved between polls: %s", got)
	}
	// The wait ends with the context, as a hard stop ends it.
	cancel()
	waitWorkers(t, h, cancel, 10*time.Second)
}
