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
	s.fillWaiting(snap, byNumber)
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
	s.fillWaiting(snap, byNumber)
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
