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
	"github.com/kpenfound/busybees/internal/state"
)

// pmOnlyTOML disables everyone but the product manager, so a pass is one
// session and the prompt it was given is unambiguous.
const pmOnlyTOML = baseTOML + `
[roles.qa]
enabled = false
[roles.developer]
enabled = false
[roles.project_manager]
enabled = false
[roles.reviewer]
enabled = false
`

// seedFeature seeds an open feature issue, last touched just now so the
// product manager's has-work check looks at it at all.
func seedFeature(h *harness, n int, title string, created time.Time, labels ...string) {
	i := &github.Issue{Number: n, Title: title, Body: "why " + title, State: "OPEN",
		Author: github.Author{Login: "kyle"}, CreatedAt: created, UpdatedAt: time.Now(),
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}}}
	for _, l := range labels {
		i.Labels = append(i.Labels, github.Label{Name: l})
	}
	h.gh.issues[n] = i
}

// dropProposal removes bees:proposal from a seeded issue, the way a person
// approving it does. It is the only way the label ever comes off.
func dropProposal(h *harness, n int) {
	var kept []github.Label
	for _, l := range h.gh.issues[n].Labels {
		if l.Name != "bees:proposal" {
			kept = append(kept, l)
		}
	}
	h.gh.issues[n].Labels = kept
}

// section returns what stands under a heading of the product manager's task
// prompt, up to the next heading.
func section(t *testing.T, prompt, heading string) string {
	t.Helper()
	_, rest, ok := strings.Cut(prompt, heading)
	if !ok {
		t.Fatalf("prompt has no %q heading:\n%s", heading, prompt)
	}
	body, _, _ := strings.Cut(rest, "\n## ")
	return body
}

// pmPrompt returns the task prompt of the product manager's newest session.
func pmPrompt(t *testing.T, h *harness) string {
	t.Helper()
	dirs := h.sessions(config.RoleProductManager)
	if len(dirs) == 0 {
		t.Fatal("no product manager session ran")
	}
	b, err := os.ReadFile(filepath.Join(dirs[len(dirs)-1], "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A proposal is never presented for breakdown, but it is presented: it has
// its own section, it stays in the feature table as waiting on a person, and
// `bees status` counts it.
func TestProposalIsPresentedButNeverForBreakdown(t *testing.T) {
	h := newHarness(t, pmOnlyTOML)
	now := time.Now()
	seedFeature(h, 5, "Exports", now.Add(-time.Hour))
	seedFeature(h, 6, "Bee-written idea", now.Add(-time.Hour), "bees:proposal")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	prompt := pmPrompt(t, h)
	breakdown := section(t, prompt, "## Feature issues needing you")
	if strings.Contains(breakdown, "#6") {
		t.Errorf("the proposal is presented for breakdown:\n%s", breakdown)
	}
	if !strings.Contains(breakdown, "#5: Exports") {
		t.Errorf("the approved feature is not presented for breakdown:\n%s", breakdown)
	}
	proposals := section(t, prompt, "## Proposals awaiting a person's approval")
	if !strings.Contains(proposals, "#6: Bee-written idea") || !strings.Contains(proposals, "why Bee-written idea") {
		t.Errorf("the proposal is not presented with its body:\n%s", proposals)
	}
	if strings.Contains(proposals, "#5") {
		t.Errorf("the approved feature is listed as a proposal:\n%s", proposals)
	}
	table := section(t, prompt, "## All open feature issues")
	if !strings.Contains(table, "| 6 |") || !strings.Contains(table, "| proposal |") {
		t.Errorf("the proposal is not in the feature table as waiting on a person:\n%s", table)
	}

	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Queues["proposals"] != 1 {
		t.Errorf("proposals queue: %d, want 1 (%v)", st.Queues["proposals"], st.Queues)
	}
	if st.Queues["features"] != 2 {
		t.Errorf("features queue: %d, want 2 — a proposal is still a feature (%v)", st.Queues["features"], st.Queues)
	}
}

// A bee-written proposal stays "fresh" forever (nobody has commented on it),
// so counting it as work would wake the product manager on every poll for a
// decision only a person can make.
func TestAProposalDoesNotWakeTheProductManager(t *testing.T) {
	h := newHarness(t, pmOnlyTOML)
	now := time.Now()
	seedFeature(h, 6, "Bee-written idea", now.Add(-time.Hour), "bees:proposal")
	if err := h.store.SaveRole(config.RoleProductManager, state.RoleState{LastRun: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.sched.productManagerHasWork(ctx, snap) {
		t.Error("a proposal woke the product manager before the interval elapsed")
	}

	// The same feature, once a person has approved it, is ordinary work.
	dropProposal(h, 6)
	snap, err = h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !h.sched.productManagerHasWork(ctx, snap) {
		t.Error("an approved feature nobody has broken down is work")
	}
}

// A person approves by removing a label, which leaves no comment: nothing in
// AwaitingBee can notice it, so the scheduler remembers the transition and
// the feature reaches the product manager on its next run.
func TestApprovingAProposalIsNoticed(t *testing.T) {
	h := newHarness(t, pmOnlyTOML)
	now := time.Now()
	seedFeature(h, 6, "Bee-written idea", now.Add(-3*time.Hour), "bees:proposal")
	// A bee comment newer than every human one: the feature is not "fresh"
	// by any comment-based measure, before or after the approval.
	h.gh.issues[6].Comments = []github.Comment{{Author: github.Author{Login: "kyle"},
		Body: "refined this <!-- bees:product_manager -->", CreatedAt: now.Add(-time.Hour)}}
	// The product manager ran a minute ago (product_manager_interval is an
	// hour), so only the approval can bring the feature back to it.
	if err := h.store.SaveRole(config.RoleProductManager, state.RoleState{LastRun: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	is, err := h.store.Issue(6)
	if err != nil {
		t.Fatal(err)
	}
	if !is.Proposal || !is.ProposalApprovedAt.IsZero() {
		t.Fatalf("a pass with the label still on approved something: %+v", is)
	}
	if len(h.sessions(config.RoleProductManager)) != 0 {
		t.Fatal("the product manager ran for a decision only a person can make")
	}

	dropProposal(h, 6)
	forcePoll(h)
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	is, err = h.store.Issue(6)
	if err != nil {
		t.Fatal(err)
	}
	if is.Proposal || is.ProposalApprovedAt.IsZero() {
		t.Fatalf("the approval was not noticed: %+v", is)
	}
	if !strings.Contains(h.logs.String(), "person approved a proposal") {
		t.Error("the approval was not logged")
	}
	approved := is.ProposalApprovedAt
	breakdown := section(t, pmPrompt(t, h), "## Feature issues needing you")
	if !strings.Contains(breakdown, "#6: Bee-written idea") {
		t.Errorf("the approved proposal did not reach the product manager:\n%s", breakdown)
	}

	// A later pass finds nothing new to approve.
	forcePoll(h)
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	is, err = h.store.Issue(6)
	if err != nil {
		t.Fatal(err)
	}
	if !is.ProposalApprovedAt.Equal(approved) {
		t.Errorf("approval timestamp moved: %v -> %v", approved, is.ProposalApprovedAt)
	}
}

// An already approved feature the scheduler has never seen before (a restart
// that lost the state dir) is an ordinary feature, not a fresh approval.
func TestAFeatureFirstSeenApprovedApprovesNothing(t *testing.T) {
	h := newHarness(t, pmOnlyTOML)
	seedFeature(h, 5, "Exports", time.Now().Add(-time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	is, err := h.store.Issue(5)
	if err != nil {
		t.Fatal(err)
	}
	if !is.ProposalApprovedAt.IsZero() {
		t.Errorf("an ordinary feature was recorded as just approved: %+v", is)
	}
}
