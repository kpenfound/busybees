package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// devAndReviewerTOML leaves the developer/reviewer loop running and switches
// the singleton roles off, so a test's session count is only the loop's.
const devAndReviewerTOML = `
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

// runPass runs one scheduler pass (the harness sets Once) and waits for the
// work it started.
func runPass(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

// forcePoll makes the next pass a full poll rather than a local pass: the
// scheduler only polls once per poll_interval, and a test that runs two
// passes back to back would otherwise dispatch from the first pass's stale
// snapshot.
func forcePoll(h *harness) {
	h.sched.mu.Lock()
	defer h.sched.mu.Unlock()
	h.sched.nextPoll = time.Time{}
}

// sessionCount is how many sessions of any role have run.
func sessionCount(h *harness) int {
	n := 0
	for _, r := range config.Roles {
		n += len(h.sessions(r))
	}
	return n
}

// TestIssueOverItsCostBudgetIsEscalated drives the developer/reviewer loop
// with sessions that cost $1 each against a $1.50 per-issue budget: the
// session that passes the budget finishes, and the issue then goes to a
// human with what it spent.
func TestIssueOverItsCostBudgetIsEscalated(t *testing.T) {
	t.Setenv("FAKE_COST", "1.0")
	h := newHarness(t, baseTOML+"max_cost_per_issue = 1.5\n"+devAndReviewerTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
		HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	runPass(t, h)

	// One developer and one reviewer session ran; the check between stages
	// stopped the worker before it started a third.
	for role, want := range map[string]int{config.RoleDeveloper: 1, config.RoleReviewer: 1} {
		if got := len(h.sessions(role)); got != want {
			t.Errorf("%s sessions: got %d want %d", role, got, want)
		}
	}
	want := "bees:in-progress,bees:review,bees:needs-human"
	if got := strings.Join(h.gh.history[1], ","); got != want {
		t.Errorf("issue 1 label history: %q want %q", got, want)
	}
	comments := h.gh.comments[1]
	if len(comments) != 1 {
		t.Fatalf("want one escalation comment, got %v", comments)
	}
	for _, part := range []string{"has cost $2.00 across 2 sessions", "`max_cost_per_issue` budget of $1.50"} {
		if !strings.Contains(comments[0], part) {
			t.Errorf("escalation comment does not name %q:\n%s", part, comments[0])
		}
	}
	// The running total survived the worker's own bookkeeping writes.
	if is, _ := h.store.Issue(1); is.Cost != 2 || is.Sessions != 2 {
		t.Errorf("issue bookkeeping: cost %v over %d sessions, want 2 over 2", is.Cost, is.Sessions)
	}
}

// TestIssueCostBudgetOffChangesNothing is the same fixture without a budget:
// the loop runs to approval as it does today.
func TestIssueCostBudgetOffChangesNothing(t *testing.T) {
	t.Setenv("FAKE_COST", "1.0")
	h := newHarness(t, baseTOML+devAndReviewerTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
		HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	runPass(t, h)

	want := "bees:in-progress,bees:review,bees:in-progress,bees:review,bees:approved"
	if got := strings.Join(h.gh.history[1], ","); got != want {
		t.Errorf("issue 1 label history: %q want %q", got, want)
	}
	if len(h.gh.comments[1]) != 0 {
		t.Errorf("no escalation expected: %v", h.gh.comments[1])
	}
}

// TestDailyBudgetStopsNewSessions seeds the ledger over the daily budget and
// checks that a whole pass dispatches nothing — no developer worker and no
// singleton — while still reporting where the money went.
func TestDailyBudgetStopsNewSessions(t *testing.T) {
	h := newHarness(t, baseTOML+"max_cost_per_day = 100.0\n")
	now := time.Now()
	for _, e := range []state.LedgerEntry{
		{Time: now.Add(-2 * time.Hour), Role: config.RoleDeveloper, Session: "old", Issue: 1, CostUSD: 60},
		{Time: now.Add(-1 * time.Hour), Role: config.RoleReviewer, Session: "old2", Issue: 1, CostUSD: 41.20},
		// Outside the rolling window: not counted.
		{Time: now.Add(-30 * time.Hour), Role: config.RoleQA, Session: "ancient", CostUSD: 500},
	} {
		if err := h.store.AppendLedger(e); err != nil {
			t.Fatal(err)
		}
	}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: now}
	h.gh.issues[2] = &github.Issue{Number: 2, Title: "Needs triage", Body: "hi", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: now}

	runPass(t, h)

	if n := sessionCount(h); n != 0 {
		t.Errorf("%d sessions started while over the daily budget", n)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !st.BudgetPaused || st.DaySpendUSD != 101.20 || st.DayBudgetUSD != 100 {
		t.Errorf("status: paused %v, spent %v of %v", st.BudgetPaused, st.DaySpendUSD, st.DayBudgetUSD)
	}
	if !strings.Contains(h.logs.String(), "daily cost budget reached ($101.20 of $100.00 in the last 24h)") {
		t.Errorf("pause not reported:\n%s", h.logs.String())
	}
}

// TestDailyBudgetDoesNotInterruptARunningWorker crosses the budget in the
// middle of a worker's loop: the worker finishes to approval, and it is the
// next pass that starts nothing.
func TestDailyBudgetDoesNotInterruptARunningWorker(t *testing.T) {
	t.Setenv("FAKE_COST", "1.0")
	h := newHarness(t, baseTOML+"max_cost_per_day = 1.5\n"+devAndReviewerTOML)
	seedCounter(t, h, "review", 1) // the reviewer approves its first review
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
		HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	// Pass one: the developer session already spends $1 of the $1.50 and the
	// reviewer session takes it over, but the worker runs to the end.
	runPass(t, h)
	want := "bees:in-progress,bees:review,bees:approved"
	if got := strings.Join(h.gh.history[1], ","); got != want {
		t.Fatalf("issue 1 label history: %q want %q", got, want)
	}
	if n := sessionCount(h); n != 2 {
		t.Fatalf("want the developer and reviewer session, got %d", n)
	}

	// Pass two: a second ready issue is waiting and stays waiting.
	seedReady(h, 2, "s", time.Now())
	forcePoll(h)
	runPass(t, h)
	if n := sessionCount(h); n != 2 {
		t.Errorf("%d sessions after the budget was reached, want the same 2", n)
	}
	if got := h.gh.history[2]; len(got) != 0 {
		t.Errorf("issue 2 was picked up: %v", got)
	}
	st, _ := h.store.LoadStatus()
	if !st.BudgetPaused || st.DaySpendUSD != 2 {
		t.Errorf("status: paused %v, spent %v", st.BudgetPaused, st.DaySpendUSD)
	}
}

// TestSessionOverItsBudgetIsTreatedAsFailed: a single over-budget session is
// logged and fails, whatever it reported.
func TestSessionOverItsBudgetIsTreatedAsFailed(t *testing.T) {
	t.Setenv("FAKE_COST", "1.0")
	h := newHarness(t, baseTOML+"max_cost_per_session = 0.5\nretries = 0\n"+devAndReviewerTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
		HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	runPass(t, h)

	if got := len(h.sessions(config.RoleDeveloper)); got != 1 {
		t.Errorf("developer sessions: got %d want 1 (retries are off)", got)
	}
	if !strings.Contains(h.logs.String(), "session over its cost budget") {
		t.Errorf("over-budget session not logged:\n%s", h.logs.String())
	}
	want := "bees:in-progress,bees:needs-human"
	if got := strings.Join(h.gh.history[1], ","); got != want {
		t.Errorf("issue 1 label history: %q want %q", got, want)
	}
	comments := h.gh.comments[1]
	if len(comments) != 1 || !strings.Contains(comments[0], "the session cost $1.00, over the `max_cost_per_session` budget of $0.50") {
		t.Fatalf("escalation comment: %v", comments)
	}
}

// TestTwoOverBudgetSessionsInARowEscalate: the first over-budget session is
// retried (with the fallback model), and only the second one gives up — and
// says that the role's limits, not the work, are the problem.
func TestTwoOverBudgetSessionsInARowEscalate(t *testing.T) {
	t.Setenv("FAKE_COST", "1.0")
	h := newHarness(t, baseTOML+"max_cost_per_session = 0.5\nretries = 1\nretry_delay = \"0s\"\n"+devAndReviewerTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
		HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	runPass(t, h)

	if got := len(h.sessions(config.RoleDeveloper)); got != 2 {
		t.Errorf("developer sessions: got %d want 2 (one retry)", got)
	}
	comments := h.gh.comments[1]
	if len(comments) != 1 {
		t.Fatalf("want one escalation comment, got %v", comments)
	}
	for _, part := range []string{"2 sessions in a row have", "roles.developer.max_turns"} {
		if !strings.Contains(comments[0], part) {
			t.Errorf("escalation comment does not name %q:\n%s", part, comments[0])
		}
	}
}

func TestOverSessionBudget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cost   float64
		budget float64
		want   bool
	}{
		{"no budget", 99, 0, false},
		{"under", 1, 2, false},
		{"exactly at the budget is not over", 2, 2, false},
		{"over", 2.01, 2, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			note, got := overSessionBudget(resultCosting(tc.cost), tc.budget)
			if got != tc.want {
				t.Fatalf("over = %v, want %v", got, tc.want)
			}
			if got && !strings.Contains(note, "max_cost_per_session") {
				t.Errorf("note does not name the budget: %q", note)
			}
		})
	}
}

func TestOverBudgetNote(t *testing.T) {
	if got := overBudgetNote("too much", 1, config.RoleDeveloper); got != "too much" {
		t.Errorf("a first over-budget session should say only what it cost: %q", got)
	}
	got := overBudgetNote("too much", 2, config.RoleDeveloper)
	if !strings.Contains(got, "2 sessions in a row") || !strings.Contains(got, "roles.developer.max_turns") {
		t.Errorf("a streak should name the role's limits: %q", got)
	}
}

// resultCosting is a finished session that cost that much.
func resultCosting(cost float64) *session.Result {
	return &session.Result{CostUSD: cost}
}

// TestOverBudgetStreak: the streak is per work item, and any session within
// budget clears it — two expensive sessions a week apart are not a pattern.
func TestOverBudgetStreak(t *testing.T) {
	h := newHarness(t, baseTOML)
	for i, want := range []int{1, 2, 3} {
		if got := h.sched.overBudgetStreak("issue-1", true); got != want {
			t.Fatalf("over-budget session %d: streak %d want %d", i+1, got, want)
		}
	}
	if got := h.sched.overBudgetStreak("issue-1", false); got != 0 {
		t.Errorf("a session within budget should clear the streak, got %d", got)
	}
	if got := h.sched.overBudgetStreak("issue-1", true); got != 1 {
		t.Errorf("streak after the reset: got %d want 1", got)
	}
	if got := h.sched.overBudgetStreak("issue-2", true); got != 1 {
		t.Errorf("another work item shares the streak: got %d want 1", got)
	}
}

func TestBudgetKey(t *testing.T) {
	withIssue := sessionSpec{role: config.RoleDeveloper}
	withIssue.data.Issue = &github.Issue{Number: 12}
	if got := budgetKey(withIssue); got != "issue-12" {
		t.Errorf("issue spec: %q", got)
	}
	if got := budgetKey(sessionSpec{role: config.RoleQA}); got != "role-qa" {
		t.Errorf("singleton spec: %q", got)
	}
}

// TestIssueSpendSeedsFromTheLedger: an issue whose bookkeeping predates
// budgets (or was deleted) still has a history in the ledger, and the total
// is seeded from it once rather than starting again at zero.
func TestIssueSpendSeedsFromTheLedger(t *testing.T) {
	h := newHarness(t, baseTOML)
	for _, e := range []state.LedgerEntry{
		{Time: time.Now().Add(-72 * time.Hour), Role: config.RoleDeveloper, Session: "a", Issue: 5, CostUSD: 3},
		{Time: time.Now().Add(-71 * time.Hour), Role: config.RoleReviewer, Session: "b", Issue: 5, CostUSD: 1.5},
		{Time: time.Now().Add(-70 * time.Hour), Role: config.RoleDeveloper, Session: "c", Issue: 6, CostUSD: 99},
	} {
		if err := h.store.AppendLedger(e); err != nil {
			t.Fatal(err)
		}
	}
	cost, sessions := h.sched.issueSpend(5)
	if cost != 4.5 || sessions != 2 {
		t.Fatalf("seeded $%v over %d sessions, want $4.50 over 2", cost, sessions)
	}
	// Seeded once: the total is stored, so the ledger is not re-read (and
	// pruning it later does not lose the spend).
	if is, _ := h.store.Issue(5); is.Cost != 4.5 || is.Sessions != 2 {
		t.Errorf("total not stored: %+v", is)
	}
	if cost, _ := h.sched.issueSpend(7); cost != 0 {
		t.Errorf("an issue with no history: $%v", cost)
	}
}

// TestOverIssueBudgetPluralisesTheSessionCount: the escalation names one
// session as "1 session", not "1 sessions" — a single session can blow the
// per-issue budget on its own.
func TestOverIssueBudgetPluralisesTheSessionCount(t *testing.T) {
	h := newHarness(t, baseTOML+"max_cost_per_issue = 0.5\n")
	for _, tc := range []struct {
		sessions int
		want     string
	}{
		{1, "across 1 session,"},
		{2, "across 2 sessions,"},
	} {
		if _, err := h.store.SetIssueCost(1, 1, tc.sessions); err != nil {
			t.Fatal(err)
		}
		note, over := h.sched.overIssueBudget(1)
		if !over {
			t.Fatalf("%d sessions costing $1 against a $0.50 budget is not over it", tc.sessions)
		}
		if !strings.Contains(note, tc.want) {
			t.Errorf("%d sessions: note %q does not contain %q", tc.sessions, note, tc.want)
		}
	}
}
