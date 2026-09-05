package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// managersTOML runs both managers and nobody else: an issue that lands in the
// wrong queue shows up as the wrong session having been started.
const managersTOML = baseTOML + `
[roles.qa]
enabled = false
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
`

// An issue a person filed with the factory's label and nothing else is an
// idea, not a spec: it goes to the product manager as feedback, who decides
// whether it becomes a feature, a work item or nothing. It must not be
// specced by the project manager on the way.
func TestUnlabelledIssueGoesToTheProductManager(t *testing.T) {
	h := newHarness(t, managersTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", Body: "exports would be nice",
		State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}}, CreatedAt: time.Now().Add(-time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:feedback" {
		t.Fatalf("issue 1 label history: %q, want bees:feedback", got)
	}
	if github.HasLabel(h.gh.issues[1].Labels, "bees:triage") {
		t.Fatalf("issue 1 was triaged: %v", h.gh.issues[1].Labels)
	}
	// The relabel and the product manager's run happen in the same pass: the
	// issue is appended to the feedback list the prompt renders, so nothing
	// waits for the next poll.
	pdm := h.sessions(config.RoleProductManager)
	if len(pdm) != 1 {
		t.Fatalf("product manager sessions: %d, want 1", len(pdm))
	}
	prompt, err := os.ReadFile(filepath.Join(pdm[0], "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "## Feedback from people (1)") ||
		!strings.Contains(string(prompt), "#1: Filed from the GitHub UI") {
		t.Fatalf("product manager prompt:\n%s", prompt)
	}
	// Nothing in the triage queue, so the project manager has no work at all.
	if got := h.sessions(config.RoleProjectManager); len(got) != 0 {
		t.Fatalf("the project manager ran for an idea: %v", got)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Queues["triage"] != 0 || st.Queues[queueNoState] != 0 {
		t.Fatalf("queues %+v, want the issue in neither triage nor no_state", st.Queues)
	}
}

// The same for a factory that does not require its label: an issue visible by
// assignee alone gets the label *and* bees:feedback, in one edit.
func TestUnlabelledIssueWithoutTheBaseLabel(t *testing.T) {
	h := newHarness(t, managersTOML+`
[filter]
assignee = "kyle"
require_label = false
`)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", State: "OPEN",
		Assignees: []github.Author{{Login: "kyle"}}, CreatedAt: time.Now().Add(-time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:feedback,bees" {
		t.Fatalf("issue 1 label history: %q, want bees:feedback,bees", got)
	}
	if n := h.gh.callCount("issue edit"); n != 1 {
		t.Fatalf("issue edit called %d times, want one edit for both labels", n)
	}
}

// A person's bug report with no state label goes to the product manager too:
// bees:bug says what an issue *is* (a bug work item), not where it should go,
// so it is not a kind that keeps the issue out of the feedback route the way
// bees:feature and bees:feedback are. A person who wants the bug built as
// written labels it bees:triage as well, which is the documented fast path to
// the project manager. Decided on #221 rather than changed: see that issue.
func TestBugIssueWithNoStateLabelGoesToTheProductManager(t *testing.T) {
	h := newHarness(t, managersTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Export writes an empty file", Body: "bees export gives me 0 bytes",
		State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:bug"}},
		CreatedAt: time.Now().Add(-time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:feedback" {
		t.Fatalf("issue 1 label history: %q, want bees:feedback", got)
	}
	// The kind label survives the relabel: the issue is still a bug report.
	if !github.HasLabel(h.gh.issues[1].Labels, "bees:bug") {
		t.Fatalf("issue 1 lost bees:bug: %v", h.gh.issues[1].Labels)
	}
	if github.HasLabel(h.gh.issues[1].Labels, "bees:triage") {
		t.Fatalf("issue 1 was triaged: %v", h.gh.issues[1].Labels)
	}
	pdm := h.sessions(config.RoleProductManager)
	if len(pdm) != 1 {
		t.Fatalf("product manager sessions: %d, want 1", len(pdm))
	}
	prompt, err := os.ReadFile(filepath.Join(pdm[0], "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "## Feedback from people (1)") ||
		!strings.Contains(string(prompt), "#1: Export writes an empty file") {
		t.Fatalf("product manager prompt:\n%s", prompt)
	}
	if got := h.sessions(config.RoleProjectManager); len(got) != 0 {
		t.Fatalf("the project manager ran for a bug report: %v", got)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Queues["triage"] != 0 || st.Queues[queueNoState] != 0 {
		t.Fatalf("queues %+v, want the issue in neither triage nor no_state", st.Queues)
	}
}

// projectManagerOnlyTOML disables the product manager: with nobody to hand
// feedback to, an unlabelled issue must not go quiet.
const projectManagerOnlyTOML = baseTOML + `
[roles.qa]
enabled = false
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
`

// With the product manager disabled and the project manager enabled, a
// person's unlabelled issue is routed to bees:triage instead of going quiet
// on a route nothing consumes (#418).
func TestUnlabelledIssueGoesToTheProjectManagerWhenProductManagerDisabled(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, projectManagerOnlyTOML, now)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", Body: "exports would be nice",
		State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}}, CreatedAt: now.Add(-time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:triage" {
		t.Fatalf("issue 1 label history: %q, want bees:triage", got)
	}
	if github.HasLabel(h.gh.issues[1].Labels, "bees:feedback") {
		t.Fatalf("issue 1 got bees:feedback with the product manager disabled: %v", h.gh.issues[1].Labels)
	}
	// The relabel and the project manager's run happen in the same pass.
	pjm := h.sessions(config.RoleProjectManager)
	if len(pjm) != 1 {
		t.Fatalf("project manager sessions: %d, want 1", len(pjm))
	}
	prompt, err := os.ReadFile(filepath.Join(pjm[0], "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "## Issues to triage (1 shown)") ||
		!strings.Contains(string(prompt), "#1: Filed from the GitHub UI") {
		t.Fatalf("project manager prompt:\n%s", prompt)
	}
}

// noManagersTOML disables both managers: an unlabelled issue has nobody to
// spec it, so it must go straight to the developer queue rather than
// silently dropping out of the factory.
const noManagersTOML = baseTOML + `
[roles.qa]
enabled = false
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
`

// With both managers disabled, a person's unlabelled issue goes straight to
// bees:ready and, in the same pass, picks up the default size from the
// existing sizing loop (#418).
func TestUnlabelledIssueGoesStraightToReadyWhenBothManagersDisabled(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, noManagersTOML, now)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", Body: "exports would be nice",
		State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}}, CreatedAt: now.Add(-time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:ready,bees:size/m" {
		t.Fatalf("issue 1 label history: %q, want bees:ready,bees:size/m", got)
	}
	if github.HasLabel(h.gh.issues[1].Labels, "bees:feedback") || github.HasLabel(h.gh.issues[1].Labels, "bees:triage") {
		t.Fatalf("issue 1 was routed to a manager with both disabled: %v", h.gh.issues[1].Labels)
	}
	if got := h.sessions(config.RoleProjectManager); len(got) != 0 {
		t.Fatalf("the project manager ran with both managers disabled: %v", got)
	}
	if got := h.sessions(config.RoleProductManager); len(got) != 0 {
		t.Fatalf("the product manager ran with both managers disabled: %v", got)
	}
}

// bees tick --only developer must not change where an unlabelled issue is
// routed: OnlyRoles scopes one invocation's dispatch, not the configured
// factory's routing decision (#418).
func TestUnlabelledIssueRoutingIgnoresOnlyRoles(t *testing.T) {
	now := time.Date(2026, 9, 5, 12, 0, 0, 0, time.UTC)
	h := newHarnessAt(t, managersTOML, now)
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", Body: "exports would be nice",
		State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}}, CreatedAt: now.Add(-time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Both managers are enabled in the config: the product manager is
	// excluded from this one tick by OnlyRoles, but that must not make the
	// routing fall through to the project manager or straight to ready.
	if got := strings.Join(h.gh.history[1], ","); got != "bees:feedback" {
		t.Fatalf("issue 1 label history: %q, want bees:feedback (OnlyRoles must not change routing)", got)
	}
}

// TestTheFactorysOwnLoginIsNotAPersonWaitingForAnAnswer pins what a
// configured github.login changes in the scheduler (#243). The orchestrator's
// escalation comment is the one comment the factory posts without a marker,
// so on a shared account nothing can tell it from a person's and it makes a
// feedback issue fresh; once the factory has an account of its own, the
// author says whose it is and the issue is not waiting for a bee. A person
// coming back after it is fresh either way.
func TestTheFactorysOwnLoginIsNotAPersonWaitingForAnAnswer(t *testing.T) {
	h := newHarness(t, managersTOML)
	now := time.Now()
	lastRun := now.Add(-time.Hour)
	issue := &github.Issue{Number: 3, Title: "Dark mode please", Body: "idea", State: "OPEN",
		Author:    github.Author{Login: "kyle"},
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:feedback"}},
		CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now,
		Comments: []github.Comment{
			{Author: github.Author{Login: "kyle"}, Body: "any news?", CreatedAt: now.Add(-30 * time.Minute)},
			{Author: github.Author{Login: "busybees-bot"}, Body: "🐝 **busybees needs a human.**", CreatedAt: now.Add(-20 * time.Minute)},
		}}
	h.gh.issues[3] = issue

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	fresh := func() int {
		got, err := h.sched.freshIssues(ctx, []github.Issue{*issue}, lastRun)
		if err != nil {
			t.Fatal(err)
		}
		return len(got)
	}
	if n := fresh(); n != 1 {
		t.Errorf("on a shared account the escalation comment reads as a person's: fresh = %d, want 1", n)
	}
	h.sched.gh.ActsAs = "busybees-bot"
	if n := fresh(); n != 0 {
		t.Errorf("the factory's own comment should not make the issue fresh: fresh = %d, want 0", n)
	}
	issue.Comments = append(issue.Comments,
		github.Comment{Author: github.Author{Login: "kyle"}, Body: "here you go", CreatedAt: now.Add(-10 * time.Minute)})
	if n := fresh(); n != 1 {
		t.Errorf("a person came back: fresh = %d, want 1", n)
	}
}
