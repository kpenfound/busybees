package scheduler

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// filteredTOML is a developer-only factory whose filter asks for an assignee
// and a milestone as well as the base label.
const filteredTOML = devOnlyTOML + `
[filter]
assignee = "kyle"
milestone = "v0.1.0"
`

// visibilityFixes returns the history entries ensureVisible produced for an
// item: the base label, the assignee and the milestone.
func visibilityFixes(h *harness, number int) []string {
	var out []string
	for _, e := range h.gh.history[number] {
		if e == "bees" || strings.HasPrefix(e, "assignee:") || strings.HasPrefix(e, "milestone:") {
			out = append(out, e)
		}
	}
	return out
}

// A session that ignores the `--assignee` instruction in its prompt still
// ends up with a pull request the factory can see: without the label, the
// assignee and the milestone the filter asks for, the PR is never polled
// again and strands its branch and its issue.
func TestPROpenedIsMadeVisible(t *testing.T) {
	h := newHarness(t, filteredTOML)
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true} // reviewer disabled: PR auto-approved
	h.gh.milestones = []github.Milestone{{Number: 3, Title: "v0.1.0"}}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}},
		Assignees: []github.Author{{Login: "kyle"}}, CreatedAt: time.Now().Add(-time.Hour)}
	// The PR the session opened carries none of the three.
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := "bees,assignee:kyle,milestone:v0.1.0"
	if got := strings.Join(visibilityFixes(h, fakePR), ","); got != want {
		t.Fatalf("PR visibility fixes: got %q want %q (history %v)", got, want, h.gh.history[fakePR])
	}
	if !github.HasAssignee(h.gh.prs[fakePR].Assignees, "kyle") {
		t.Fatalf("PR assignees: %v", h.gh.prs[fakePR].Assignees)
	}
}

// A pull request that already matches the filter costs no gh calls: the
// factory must not re-label, re-assign and re-milestone it on every pass.
func TestVisiblePRIsLeftAlone(t *testing.T) {
	h := newHarness(t, filteredTOML)
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	h.gh.milestones = []github.Milestone{{Number: 3, Title: "v0.1.0"}}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}},
		Assignees: []github.Author{{Login: "kyle"}}, CreatedAt: time.Now().Add(-time.Hour)}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}}, Assignees: []github.Author{{Login: "Kyle"}},
		Milestone: &github.MilestoneRef{Title: "v0.1.0"}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := visibilityFixes(h, fakePR); len(got) != 0 {
		t.Fatalf("an already visible PR was edited: %v", got)
	}
	// The milestone was not even looked up.
	if n := h.gh.callCount("api repos/acme/widgets/milestones?state=open&per_page=100"); n != 0 {
		t.Fatalf("milestones listed %d times for a PR already in the milestone", n)
	}
}

// Without filter.milestone nothing sets a milestone, and the assignee is
// still applied on its own.
func TestPRVisibilityWithoutAMilestoneFilter(t *testing.T) {
	h := newHarness(t, devOnlyTOML+"\n[filter]\nassignee = \"kyle\"\n")
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}},
		Assignees: []github.Author{{Login: "kyle"}}, CreatedAt: time.Now().Add(-time.Hour)}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	want := "bees,assignee:kyle"
	if got := strings.Join(visibilityFixes(h, fakePR), ","); got != want {
		t.Fatalf("PR visibility fixes: got %q want %q", got, want)
	}
}

// A PR the factory cannot make visible is a stranded PR, so the warning must
// name the PR and what could not be done — but it must not fail the worker.
func TestPRVisibilityFailureIsWarnedWithThePRNumber(t *testing.T) {
	h := newHarness(t, filteredTOML)
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	h.gh.milestones = []github.Milestone{{Number: 3, Title: "v0.1.0"}}
	// Both REST calls (assign, milestone) fail.
	h.gh.errFor["api --method"] = errors.New("boom")
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN",
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}},
		Assignees: []github.Author{{Login: "kyle"}}, CreatedAt: time.Now().Add(-time.Hour)}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main"}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	logs := h.logs.String()
	for _, want := range []string{"the pull request may be invisible to the factory", "pr=101", "assign it to kyle", "put it in milestone v0.1.0"} {
		if !strings.Contains(logs, want) {
			t.Errorf("warning does not mention %q:\n%s", want, logs)
		}
	}
	// The worker carried on: the issue still reached its next state.
	if got := strings.Join(h.gh.history[1], ","); got != "bees:in-progress,bees:approved" {
		t.Fatalf("issue 1 history: %s", got)
	}
}
