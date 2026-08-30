package scheduler

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/state"
)

// checksTOML runs the developer/reviewer loop alone, with auto_merge on and
// the checks timings squeezed so a whole wait takes milliseconds. The
// pre-review read is off: these tests are about the post-approval gate, and
// prereview_test.go covers the other one.
const checksTOML = baseTOML + `
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
[roles.reviewer]
auto_merge = true
checks_wait = "1ms"
checks_poll_interval = "10ms"
checks_timeout = "5s"
max_check_fix_rounds = 1
pre_review_checks = false
`

const (
	passingJSON = `[{"name":"go / test","bucket":"pass","state":"SUCCESS"}]`
	failingJSON = `[{"name":"go / test","bucket":"fail","state":"FAILURE","link":"https://ci.example.com/run/1","description":"1 test failed","workflow":"CI"}]`
	pendingJSON = `[{"name":"go / test","bucket":"pending","state":"PENDING"}]`
)

// seedChecksIssue seeds a ready issue and its pull request, with the reviewer
// approving straight away so the run reaches the checks stage in one round.
func seedChecksIssue(t *testing.T, h *harness) {
	t.Helper()
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Ship it", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}}
	seedCounter(t, h, "review", 1)
}

// checksCalls counts the two `gh pr checks` flavours separately.
func checksCalls(h *harness) (required, reported int) {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	for _, c := range h.gh.calls {
		if len(c) < 2 || c[0] != "pr" || c[1] != "checks" {
			continue
		}
		if strings.Contains(strings.Join(c, " "), "--required") {
			required++
		} else {
			reported++
		}
	}
	return required, reported
}

func runChecksLoop(t *testing.T, h *harness) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
}

// TestRequiredChecksAreTheWholeGate pins the unchanged path: when the branch
// requires checks, they decide, and the second (unrequired) gh call is never
// made — the failing reported checks here would block the merge if it were.
func TestRequiredChecksAreTheWholeGate(t *testing.T) {
	h := newHarness(t, checksTOML)
	seedChecksIssue(t, h)
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	h.gh.checksAll = []checksResponse{{failingJSON, fmt.Errorf("exit status 1")}}
	runChecksLoop(t, h)

	if len(h.gh.merged) != 1 || h.gh.merged[0] != fakePR {
		t.Fatalf("merged: %v", h.gh.merged)
	}
	required, reported := checksCalls(h)
	if required == 0 || reported != 0 {
		t.Fatalf("checks calls: %d required, %d reported; the unrequired call must not be made", required, reported)
	}
	if !strings.Contains(h.logs.String(), "required checks passed; merging") {
		t.Fatalf("log does not name the required gate:\n%s", h.logs.String())
	}
}

// TestReportedChecksGateTheMergeWhenNothingIsRequired is the fallback: with no
// branch protection the checks the pull request reports are the gate.
func TestReportedChecksGateTheMergeWhenNothingIsRequired(t *testing.T) {
	h := newHarness(t, checksTOML)
	seedChecksIssue(t, h)
	h.gh.checksAll = []checksResponse{{passingJSON, nil}}
	runChecksLoop(t, h)

	if len(h.gh.merged) != 1 || h.gh.merged[0] != fakePR {
		t.Fatalf("merged: %v", h.gh.merged)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "no required checks; 1 reported checks passed; merging") {
		t.Fatalf("log does not name the reported gate:\n%s", logs)
	}
	if strings.Contains(logs, "required checks passed") || strings.Contains(logs, "no checks reported; merging") {
		t.Fatalf("log calls an unrequired check a required one:\n%s", logs)
	}
}

// TestFailingReportedCheckBlocksTheMerge: a failing unrequired check feeds the
// reviewer-diagnoses / developer-fixes loop exactly as a required one does,
// max_check_fix_rounds still applies, and the escalation names the check.
func TestFailingReportedCheckBlocksTheMerge(t *testing.T) {
	h := newHarness(t, checksTOML)
	seedChecksIssue(t, h)
	h.gh.checksAll = []checksResponse{{failingJSON, fmt.Errorf("exit status 1")}}
	runChecksLoop(t, h)

	if len(h.gh.merged) != 0 {
		t.Fatalf("merged with a failing check: %v", h.gh.merged)
	}
	if got := h.gh.history[1]; got[len(got)-1] != "bees:needs-human" {
		t.Fatalf("history: %v", got)
	}
	// max_check_fix_rounds = 1: one diagnosis, then the escalation.
	if n := len(h.sessions(config.RoleReviewer)); n != 2 {
		t.Fatalf("reviewer sessions: %d, want 2 (one review, one checks diagnosis)", n)
	}
	if len(h.gh.comments[1]) != 1 || !strings.Contains(h.gh.comments[1][0], "go / test") {
		t.Fatalf("the escalation must name the failing check: %v", h.gh.comments[1])
	}
	if !strings.Contains(h.gh.comments[1][0], "still fail after 1 fix rounds") {
		t.Fatalf("comment: %v", h.gh.comments[1])
	}
}

// TestPendingReportedCheckIsWaitedForNotIgnored: a still-running unrequired
// check is treated exactly as a pending required one — polled until
// checks_timeout, then escalated, never merged.
func TestPendingReportedCheckIsWaitedForNotIgnored(t *testing.T) {
	h := newHarness(t, strings.Replace(checksTOML, `checks_timeout = "5s"`, `checks_timeout = "50ms"`, 1))
	seedChecksIssue(t, h)
	h.gh.checksAll = []checksResponse{{pendingJSON, fmt.Errorf("exit status 8")}}
	runChecksLoop(t, h)

	if len(h.gh.merged) != 0 {
		t.Fatalf("merged with a pending check: %v", h.gh.merged)
	}
	if got := h.gh.history[1]; got[len(got)-1] != "bees:needs-human" {
		t.Fatalf("history: %v", got)
	}
	if len(h.gh.comments[1]) != 1 || !strings.Contains(h.gh.comments[1][0], "still pending") {
		t.Fatalf("comments: %v", h.gh.comments[1])
	}
	if _, reported := checksCalls(h); reported < 2 {
		t.Fatalf("a pending reported check must be polled, not read once: %d reads", reported)
	}
}

// TestNoChecksAtAllMergesAndSaysSo: merging a repository with no CI is
// legitimate, but it takes two consecutive empty observations and it is logged
// as what it is.
func TestNoChecksAtAllMergesAndSaysSo(t *testing.T) {
	h := newHarness(t, checksTOML)
	seedChecksIssue(t, h)
	// Both queues stay empty: the fake answers gh's "no checks reported".
	runChecksLoop(t, h)

	if len(h.gh.merged) != 1 || h.gh.merged[0] != fakePR {
		t.Fatalf("merged: %v", h.gh.merged)
	}
	required, reported := checksCalls(h)
	if required < 2 || reported < 2 {
		t.Fatalf("one empty observation must not conclude there is no CI: %d required, %d reported reads", required, reported)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "no checks reported; merging without a check gate") {
		t.Fatalf("log does not say the merge was ungated:\n%s", logs)
	}
	if strings.Contains(logs, "checks passed") {
		t.Fatalf("nothing passed, so the log must not say so:\n%s", logs)
	}
}

// TestACheckAppearingOnTheSecondPollIsHonoured: a workflow can take longer
// than checks_wait to register, and must not be raced past.
func TestACheckAppearingOnTheSecondPollIsHonoured(t *testing.T) {
	h := newHarness(t, checksTOML)
	seedChecksIssue(t, h)
	h.gh.checksAll = []checksResponse{
		{"", fmt.Errorf("no checks reported on the 'bees/issue-1' branch")},
		{failingJSON, fmt.Errorf("exit status 1")},
	}
	runChecksLoop(t, h)

	if len(h.gh.merged) != 0 {
		t.Fatalf("a check that appeared on the second poll was raced past: %v", h.gh.merged)
	}
	if got := h.gh.history[1]; got[len(got)-1] != "bees:needs-human" {
		t.Fatalf("history: %v", got)
	}
}

// TestTheWorkerStageNamesTheGate: a worker sitting in a 30-minute wait shows
// in `bees status` what it is waiting for.
func TestTheWorkerStageNamesTheGate(t *testing.T) {
	for _, tc := range []struct {
		name              string
		required, all     []checksResponse
		wantStage         string
		wantGate          checksGate
		wantChecksSummary github.ChecksStatus
	}{
		{"required", []checksResponse{{passingJSON, nil}}, nil, "checks (required)", gateRequired, github.ChecksPassed},
		{"reported", nil, []checksResponse{{passingJSON, nil}}, "checks (reported)", gateReported, github.ChecksPassed},
		{"none", nil, nil, "checks (none)", gateNone, github.ChecksNone},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, checksTOML)
			h.gh.checks, h.gh.checksAll = tc.required, tc.all
			w := &state.Worker{Name: "dev-1", Issue: 1, Stage: "checks", Round: 1}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			policy := h.sched.cfg.Merge()
			status, _, gate, err := h.sched.awaitChecks(ctx, fakePR, policy, checksWatch{timeout: policy.ChecksTimeout, stage: "checks"}, w, 1)
			if err != nil {
				t.Fatal(err)
			}
			if status != tc.wantChecksSummary || gate != tc.wantGate {
				t.Fatalf("status %q gate %q", status, gate)
			}
			if w.Stage != tc.wantStage {
				t.Fatalf("worker stage %q, want %q", w.Stage, tc.wantStage)
			}
		})
	}
}
