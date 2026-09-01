package scheduler

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// TestNeedsHumanHoldsAnIssueThatKeepsItsStateLabel: a person parks an issue
// by adding bees:needs-human to it from the GitHub issue list, which does
// not remove the state label underneath. The issue therefore carries two
// state labels, and the hold only works because bees:needs-human comes
// first in StateLabels() — every derivation of a state takes the first
// match. Last, as it was until #322, it lost to bees:ready and the factory
// went on dispatching the issue while the Needs human panel listed it.
//
// The second half is what makes the documented undo true: removing the
// label alone hands the issue straight back to bees:ready. Nothing tidies
// the pair up in between, which the label history asserts.
func TestNeedsHumanHoldsAnIssueThatKeepsItsStateLabel(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))
	h.gh.issues[1].Labels = append(h.gh.issues[1].Labels, github.Label{Name: "bees:needs-human"})

	runPass(t, h)

	if got := h.stateOfIssue(1); got != "needs-human" {
		t.Fatalf("state of the held issue: %q, want needs-human", got)
	}
	if got := h.sessions(config.RoleDeveloper); len(got) != 0 {
		t.Errorf("%d developer sessions ran for a held issue, want none: %v", len(got), got)
	}
	if got := h.gh.history[1]; len(got) != 0 {
		t.Errorf("the held issue was relabelled: %v", got)
	}

	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.NeedsHuman) != 1 || st.NeedsHuman[0].Issue != 1 {
		t.Errorf("needs_human: got %+v, want issue 1", st.NeedsHuman)
	}
	if n := st.Queues["ready"]; n != 0 {
		t.Errorf("ready queue counts %d, want 0: a held issue is not work", n)
	}
	if n := st.Queues["needs-human"]; n != 1 {
		t.Errorf("needs-human queue counts %d, want 1", n)
	}

	// A person lifts the hold by removing the one label, and nothing else.
	h.gh.mu.Lock()
	var kept []github.Label
	for _, l := range h.gh.issues[1].Labels {
		if l.Name != "bees:needs-human" {
			kept = append(kept, l)
		}
	}
	h.gh.issues[1].Labels = kept
	h.gh.mu.Unlock()

	if got := h.stateOfIssue(1); got != "ready" {
		t.Fatalf("state after the hold was lifted: %q, want ready", got)
	}
	forcePoll(h)
	snap, err := h.sched.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.byState["ready"]) != 1 || snap.byState["ready"][0].Number != 1 {
		t.Errorf("ready bucket: got %+v, want issue 1", snap.byState["ready"])
	}
	if got := snap.byState["needs-human"]; len(got) != 0 {
		t.Errorf("still escalated after the hold was lifted: %+v", got)
	}
}

// TestExecReviewerOnAHeldIssueStillReviews: `bees exec reviewer` forces the
// review stage by rewriting the issue's state label, and the local copy the
// worker reads has to be rewritten the same way. A held issue carries two
// state labels, so removing only the one stateOf names leaves the other
// behind: with bees:needs-human first the copy kept bees:in-progress,
// stateOf read that back out and a developer session ran instead of the
// review that was asked for.
func TestExecReviewerOnAHeldIssueStillReviews(t *testing.T) {
	h := newHarness(t, prereviewTOML)
	seedPreReviewIssue(t, h, "Review what is already pushed")
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:in-progress"},
		{Name: "bees:size/s"}, {Name: "bees:needs-human"}}
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	seedCounter(t, h, "review", 1) // approve straight away
	h.gh.checks = []checksResponse{{passingJSON, nil}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.RunRole(ctx, config.RoleReviewer, 1, 0); err != nil {
		t.Fatal(err)
	}

	h.wantOrder("reviewer-pr-101-r1")
	if n := len(h.sessions(config.RoleDeveloper)); n != 0 {
		t.Fatalf("%d developer sessions ran, want none: exec reviewer asked for a review", n)
	}
}
