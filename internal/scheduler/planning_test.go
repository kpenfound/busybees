package scheduler

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/state"
)

// humanComment gives a seeded issue a person's comment, dated at, and dates
// the issue's own update with it: freshIssues' pre-filter compares UpdatedAt
// strictly against the last run, so both need real margin.
func humanComment(h *harness, n int, body string, at time.Time) {
	h.gh.issues[n].UpdatedAt = at
	h.gh.issues[n].Comments = append(h.gh.issues[n].Comments,
		github.Comment{Author: github.Author{Login: "kyle"}, Body: body, CreatedAt: at})
}

// A person's comment on an issue they put in planning mode wakes the product
// manager, and the task it is given presents the issue as a conversation: in
// the planning section, with no breakdown step, and nowhere near the section
// that asks for one.
func TestACommentOnAPlanningIssueStartsADiscussionOnlySession(t *testing.T) {
	h := newHarness(t, pmOnlyTOML)
	now := time.Now()
	seedFeature(h, 5, "Exports", now.Add(-time.Hour))
	seedFeature(h, 6, "Offline mode", now.Add(-time.Hour), "bees:planning")
	humanComment(h, 6, "should this sync in the background?", now.Add(-time.Minute))
	if err := h.store.SaveRole(config.RoleProductManager, state.RoleState{LastRun: now.Add(-2 * time.Minute)}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !h.sched.productManagerHasWork(ctx, snap) {
		t.Error("a person's comment on a planning issue did not wake the product manager")
	}
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	prompt := pmPrompt(t, h)
	planning := section(t, prompt, "## Planning with a person")
	for _, want := range []string{"#6: Offline mode", "should this sync in the background?", "Break nothing down from these"} {
		if !strings.Contains(planning, want) {
			t.Errorf("planning section missing %q:\n%s", want, planning)
		}
	}
	breakdown := section(t, prompt, "## Feature issues needing you")
	if strings.Contains(breakdown, "#6") {
		t.Errorf("a planning issue is presented for breakdown:\n%s", breakdown)
	}
	if !strings.Contains(breakdown, "#5: Exports") {
		t.Errorf("an ordinary feature is no longer presented for breakdown:\n%s", breakdown)
	}
}

// A planning issue nobody has commented on is not work: like a proposal, it
// waits for a person, and the session it would start has nothing to reply to.
func TestAnUntouchedPlanningIssueDoesNotWakeTheProductManager(t *testing.T) {
	h := newHarness(t, pmOnlyTOML)
	now := time.Now()
	seedFeature(h, 6, "Offline mode", now.Add(-time.Hour), "bees:planning")
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
		t.Error("an untouched planning issue woke the product manager before the interval elapsed")
	}
}

// A person ends planning by swapping bees:planning for bees:planned. The
// feature is then presented as agreed and to be broken down — once: the
// sub-issues the breakdown creates are what take it off the list, so the run
// after does not break it down again.
func TestAPlannedFeatureIsBrokenDownOnceAndNotAgain(t *testing.T) {
	now := time.Now()
	h := newHarnessAt(t, pmOnlyTOML, now)
	seedFeature(h, 6, "Offline mode", now.Add(-time.Hour), "bees:planned")
	// Nothing has been broken down from it yet, and a bee had the last word
	// in the planning conversation, so only the planned label presents it.
	h.gh.subIssues[6] = github.SubIssueSummary{}
	quietFeature(h, 6, now.Add(-30*time.Minute))

	runPass(t, h)

	agreed := section(t, lastPMPrompt(t, h), "## Agreed with a person")
	for _, want := range []string{"#6: Offline mode", "## Decisions", "settled"} {
		if !strings.Contains(agreed, want) {
			t.Errorf("agreed section missing %q:\n%s", want, agreed)
		}
	}

	// The session broke it into work items, so GitHub now reports sub-issues.
	// Nothing about the agreed feature wakes the product manager — an agreed
	// issue waits for it rather than starting it — so the next run is the one
	// product_manager_interval brings round.
	h.gh.subIssues[6] = github.SubIssueSummary{Total: 2}
	h.clock.advance(h.cfg.Scheduler.ProductManagerInterval.Duration + time.Minute)
	forcePoll(h)
	runPass(t, h)
	if n := len(h.sessions(config.RoleProductManager)); n != 2 {
		t.Fatalf("product manager sessions: %d, want 2", n)
	}
	again := section(t, lastPMPrompt(t, h), "## Agreed with a person")
	if strings.Contains(again, "#6") {
		t.Errorf("a feature that has been broken down is presented for breakdown again:\n%s", again)
	}
	if !strings.Contains(again, "_None waiting._") {
		t.Errorf("agreed section is not empty:\n%s", again)
	}
}

// bees:planning and bees:planned are neither state nor size labels: an issue
// in planning keeps whatever state it has, and classify buckets it exactly as
// it did before the labels existed.
func TestClassifyIgnoresThePlanningLabels(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	now := time.Now()
	lbl := func(names ...string) []github.Label {
		var out []github.Label
		for _, n := range names {
			out = append(out, github.Label{Name: n})
		}
		return out
	}
	issues := []github.Issue{
		{Number: 1, Title: "work item", CreatedAt: now, Labels: lbl("bees", "bees:ready", "bees:planning")},
		{Number: 2, Title: "feature", CreatedAt: now, Labels: lbl("bees", "bees:feature", "bees:planning")},
		{Number: 3, Title: "feedback", CreatedAt: now, Labels: lbl("bees", "bees:feedback", "bees:planned")},
		{Number: 4, Title: "no state", CreatedAt: now, Labels: lbl("bees", "bees:planned")},
	}
	snap := h.sched.classify(issues, nil)
	if got := len(snap.byState["ready"]); got != 1 || snap.byState["ready"][0].Number != 1 {
		t.Errorf("a planning work item left the ready bucket: %v", snap.byState["ready"])
	}
	if got := len(snap.features); got != 1 || snap.features[0].Number != 2 {
		t.Errorf("features: %v", snap.features)
	}
	if got := len(snap.feedback); got != 1 || snap.feedback[0].Number != 3 {
		t.Errorf("feedback: %v", snap.feedback)
	}
	if got := len(snap.byState[""]); got != 1 || snap.byState[""][0].Number != 4 {
		t.Errorf("a planned issue with no state label left the no-state bucket: %v", snap.byState[""])
	}
	if len(snap.proposals) != 0 {
		t.Errorf("proposals: %v", snap.proposals)
	}
}
