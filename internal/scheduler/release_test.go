package scheduler

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// releaseTOML runs the release manager on its own, so every pass that starts
// a session started it for a milestone.
const releaseTOML = baseTOML + `
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
[roles.release_manager]
enabled = true
`

// seedMilestone gives the fake one open milestone, v1.0.0 (#4), and issue 7
// closed in it.
func seedMilestone(h *harness, closed, open int) {
	h.gh.Milestones = []github.Milestone{{Number: 4, Title: "v1.0.0", State: "open", ClosedIssues: closed, OpenIssues: open}}
	h.gh.Issues[7] = &github.Issue{Number: 7, Title: "Export CSV", State: "CLOSED",
		Labels: []github.Label{{Name: "bees"}}, Milestone: &github.MilestoneRef{Title: "v1.0.0"}}
}

// nextReleasePass runs one more full pass once the poll interval the last
// release manager run backed the role off for has passed.
func nextReleasePass(t *testing.T, h *harness) {
	t.Helper()
	h.clock.advance(time.Minute)
	forcePoll(h)
	runPass(t, h)
}

// A finished milestone — closed issues, none open, no pull request in flight
// — starts one release manager session, told the milestone and a closed issue
// in it to relate what it files to. Once it has shipped, the milestone is
// closed and nothing starts it again.
func TestReleaseManagerShipsAFinishedMilestone(t *testing.T) {
	t.Setenv("FAKE_RELEASE", "ship:4")
	h := newHarnessAt(t, releaseTOML, time.Now())
	seedMilestone(h, 2, 0)
	// A pull request for an issue outside the milestone holds nothing back.
	h.gh.Issues[8] = &github.Issue{Number: 8, Title: "Next thing", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}}
	h.gh.PRs[30] = &github.PR{Number: 30, Title: "Next thing", State: "OPEN", Body: "Closes #8", HeadRefName: "bees/issue-8", BaseRefName: "main"}
	runPass(t, h)

	sessions := h.sessions(config.RoleReleaseManager)
	if len(sessions) != 1 {
		t.Fatalf("release manager sessions: %d, want 1", len(sessions))
	}
	prompt := readFile(t, filepath.Join(sessions[0], "prompt.md"))
	for _, want := range []string{"# Task: ship milestone v1.0.0", "`related: 7`", "`release_ship`\n(`milestone: 4`)"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the task is missing %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(h.logs.String(), "singleton role failed") {
		t.Errorf("a run that shipped the milestone failed:\n%s", h.logs.String())
	}

	nextReleasePass(t, h)
	if got := len(h.sessions(config.RoleReleaseManager)); got != 1 {
		t.Fatalf("release manager sessions after the milestone closed: %d, want 1", got)
	}
}

// A milestone that is not finished starts nothing: one with no closed issue,
// one with an issue still open, and one with an open pull request in it or
// for one of its issues. The pull requests carry no factory label: release
// eligibility counts every open pull request, not only the ones the filter
// shows.
func TestReleaseManagerWaitsForAFinishedMilestone(t *testing.T) {
	for name, tc := range map[string]struct {
		closed, open int
		pr           *github.PR
	}{
		"empty milestone": {closed: 0, open: 0},
		"open issue":      {closed: 2, open: 1},
		"pull request in the milestone": {closed: 2, pr: &github.PR{Number: 30, State: "OPEN", HeadRefName: "fix",
			BaseRefName: "main", Milestone: &github.MilestoneRef{Title: "v1.0.0"}}},
		"pull request for a milestone issue": {closed: 2, pr: &github.PR{Number: 30, State: "OPEN", HeadRefName: "fix",
			BaseRefName: "main", Body: "Fixes #7"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarnessAt(t, releaseTOML, time.Now())
			seedMilestone(h, tc.closed, tc.open)
			if tc.pr != nil {
				h.gh.PRs[tc.pr.Number] = tc.pr
			}
			runPass(t, h)
			if got := len(h.sessions(config.RoleReleaseManager)); got != 0 {
				t.Fatalf("release manager sessions: %d, want 0", got)
			}
		})
	}
}

// The role is off unless a project enables it: a finished milestone starts
// nothing, and the milestones are not even read.
func TestDisabledReleaseManagerIsNotDispatched(t *testing.T) {
	toml := strings.Replace(releaseTOML, "[roles.release_manager]\nenabled = true\n", "", 1)
	h := newHarnessAt(t, toml, time.Now())
	seedMilestone(h, 2, 0)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReleaseManager)); got != 0 {
		t.Fatalf("release manager sessions: %d, want 0", got)
	}
	h.gh.Lock()
	defer h.gh.Unlock()
	for _, c := range h.gh.Calls {
		if strings.Contains(strings.Join(c, " "), "/milestones") {
			t.Errorf("a disabled release manager read the milestones: %v", c)
		}
	}
}

// A refused tag ends in a needs-human issue in the milestone. While it is
// open the milestone is not dispatched again; once a person closes it, the
// milestone is finished again and the release manager is started for it.
func TestEscalatedMilestoneWaitsForItsIssue(t *testing.T) {
	t.Setenv("FAKE_RELEASE", "escalate:50:v1.0.0")
	h := newHarnessAt(t, releaseTOML, time.Now())
	seedMilestone(h, 2, 0)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReleaseManager)); got != 1 {
		t.Fatalf("release manager sessions: %d, want 1", got)
	}
	if strings.Contains(h.logs.String(), "singleton role failed") {
		t.Errorf("a run that filed the escalation failed:\n%s", h.logs.String())
	}

	nextReleasePass(t, h)
	if got := len(h.sessions(config.RoleReleaseManager)); got != 1 {
		t.Fatalf("release manager sessions while the escalation is open: %d, want 1", got)
	}

	h.gh.Lock()
	h.gh.Issues[50].State = "CLOSED"
	h.gh.Milestones[0].OpenIssues, h.gh.Milestones[0].ClosedIssues = 0, 3
	h.gh.Unlock()
	nextReleasePass(t, h)
	if got := len(h.sessions(config.RoleReleaseManager)); got != 2 {
		t.Fatalf("release manager sessions once the escalation closed: %d, want 2", got)
	}
}

// A session that reports done while its milestone is still open with nothing
// open in it neither shipped nor filed what holds the milestone back: the run
// fails, rather than counting as a release.
func TestReleaseManagerDoneWithoutEffectFails(t *testing.T) {
	h := newHarnessAt(t, releaseTOML, time.Now())
	seedMilestone(h, 2, 0)
	runPass(t, h)
	if got := len(h.sessions(config.RoleReleaseManager)); got != 1 {
		t.Fatalf("release manager sessions: %d, want 1", got)
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "singleton role failed") || !strings.Contains(logs, "milestone v1.0.0 is still open and nothing open holds it back") {
		t.Errorf("the run did not fail:\n%s", logs)
	}
}
