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
