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
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/workspace"
)

// seedApprovedPR sets up an approved issue 1 whose PR (fakePR) is visible
// from the start, with the given merge state, and pushes its branch to
// origin so a worker can reuse it.
func seedApprovedPR(t *testing.T, h *harness, mergeable, mergeState, sha string) {
	t.Helper()
	created := time.Now().Add(-time.Hour)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Done already", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:approved"}, {Name: "bees:size/s"}}, CreatedAt: created}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", URL: "https://x/pull/101",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:approved"}}, CreatedAt: created, UpdatedAt: created,
		Body: "Closes #1", Mergeable: mergeable, MergeStateStatus: mergeState, HeadSHA: sha}
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.clone, "seed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"checkout", "-q", "-b", "bees/issue-1"}, {"add", "."}, {"commit", "-q", "-m", "seed"}, {"push", "-q", "-u", "origin", "bees/issue-1"}, {"checkout", "-q", "main"}} {
		if _, err := workspace.Git(context.Background(), h.clone, args...); err != nil {
			t.Fatal(err)
		}
	}
}

// developerMail lists every message in the developer's inbox, oldest first.
func developerMail(t *testing.T, h *harness) []mail.Message {
	t.Helper()
	msgs, err := h.box.List(mail.Filter{To: config.RoleDeveloper})
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func TestConflictingPRGoesBackToTheDeveloper(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedApprovedPR(t, h, github.MergeableConflicting, "DIRTY", "abc123def456")

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// approved -> ready (conflict) -> in-progress -> review -> ... -> approved
	hist := strings.Join(h.gh.history[1], ",")
	if !strings.HasPrefix(hist, "bees:ready,bees:in-progress,bees:review") || !strings.HasSuffix(hist, "bees:approved") {
		t.Fatalf("history: %s", hist)
	}
	msgs := developerMail(t, h)
	var got *mail.Message
	for i := range msgs {
		if msgs[i].From == OrchestratorSender {
			got = &msgs[i]
		}
	}
	if got == nil {
		t.Fatalf("no mail from the orchestrator in the developer's inbox: %+v", msgs)
	}
	if got.Issue != 1 || got.PR != fakePR || got.Subject != "PR #101 conflicts with main" {
		t.Fatalf("mail: %+v", got)
	}
	for _, want := range []string{"branch `bees/issue-1`", "resolve the conflicts", "`pr-updated`", "https://x/pull/101"} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("mail body missing %q:\n%s", want, got.Body)
		}
	}
	dev := h.sessions(config.RoleDeveloper)
	if len(dev) == 0 {
		t.Fatal("no developer session ran")
	}
	prompt, _ := os.ReadFile(filepath.Join(dev[0], "prompt.md"))
	if !strings.Contains(string(prompt), "PR #101 conflicts with main") {
		t.Errorf("developer prompt missing the conflict mail:\n%s", prompt)
	}
	if bk, _ := h.store.Issue(1); bk.ConflictNotifiedSHA != "abc123def456" {
		t.Fatalf("ConflictNotifiedSHA not recorded: %+v", bk)
	}
	if !strings.Contains(h.logs.String(), "pull request needs updating; developer notified") {
		t.Fatalf("no log line about the notification:\n%s", h.logs.String())
	}
}

// checkOnce polls the fake GitHub and runs the conflict check once, the way
// a full pass does, without dispatching anything.
func checkOnce(t *testing.T, h *harness) {
	t.Helper()
	ctx := context.Background()
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.sched.checkPRs(ctx, snap); err != nil {
		t.Fatal(err)
	}
}

func TestConflictIsNotifiedOncePerHead(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	seedApprovedPR(t, h, github.MergeableConflicting, "DIRTY", "aaa")

	checkOnce(t, h)
	if n := len(developerMail(t, h)); n != 1 {
		t.Fatalf("after the first check: %d messages, want 1", n)
	}
	if github.HasLabel(h.gh.issues[1].Labels, "bees:approved") || !github.HasLabel(h.gh.issues[1].Labels, "bees:ready") {
		t.Fatalf("issue 1 labels: %v, want ready instead of approved", h.gh.issues[1].Labels)
	}
	if github.HasLabel(h.gh.prs[fakePR].Labels, "bees:approved") {
		t.Fatalf("PR kept bees:approved: %v", h.gh.prs[fakePR].Labels)
	}

	// Same head, still conflicting: the developer is not nagged.
	checkOnce(t, h)
	if n := len(developerMail(t, h)); n != 1 {
		t.Fatalf("after a second check with the same head: %d messages, want 1", n)
	}

	// The developer pushed (new head, issue back in review) but it still
	// conflicts: told again.
	h.gh.prs[fakePR].HeadSHA = "bbb"
	h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: "bees:review"}, {Name: "bees:size/s"}}
	checkOnce(t, h)
	if n := len(developerMail(t, h)); n != 2 {
		t.Fatalf("after a push that still conflicts: %d messages, want 2", n)
	}
	if bk, _ := h.store.Issue(1); bk.ConflictNotifiedSHA != "bbb" {
		t.Fatalf("ConflictNotifiedSHA: %q, want bbb", bk.ConflictNotifiedSHA)
	}
}

func TestPRCheckHonoursTheSettings(t *testing.T) {
	for _, tc := range []struct {
		name       string
		toml       string
		mergeable  string
		mergeState string
		state      string // the issue's state label
		want       int    // messages the developer receives
		subject    string
	}{
		{"conflict, fixing disabled", "pr_fix_conflicts = false\n", github.MergeableConflicting, "DIRTY", "bees:approved", 0, ""},
		{"behind, keep_updated off (default)", "", "MERGEABLE", github.MergeStateBehind, "bees:approved", 0, ""},
		{"behind, keep_updated on", "pr_keep_updated = true\n", "MERGEABLE", github.MergeStateBehind, "bees:approved", 1, "PR #101 is behind main"},
		{"conflict counts as a conflict, not as behind", "pr_fix_conflicts = false\npr_keep_updated = true\n", github.MergeableConflicting, "DIRTY", "bees:approved", 0, ""},
		{"not computed yet", "pr_keep_updated = true\n", "UNKNOWN", "UNKNOWN", "bees:approved", 0, ""},
		{"no merge state at all", "pr_keep_updated = true\n", "", "", "bees:approved", 0, ""},
		{"clean", "pr_keep_updated = true\n", "MERGEABLE", "CLEAN", "bees:approved", 0, ""},
		{"conflict in review", "", github.MergeableConflicting, "DIRTY", "bees:review", 1, "PR #101 conflicts with main"},
		{"conflict in progress: the developer is on it", "", github.MergeableConflicting, "DIRTY", "bees:in-progress", 0, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, strings.Replace(devOnlyTOML, "[scheduler]\n", "[scheduler]\n"+tc.toml, 1))
			seedApprovedPR(t, h, tc.mergeable, tc.mergeState, "abc")
			h.gh.issues[1].Labels = []github.Label{{Name: "bees"}, {Name: tc.state}, {Name: "bees:size/s"}}
			checkOnce(t, h)
			msgs := developerMail(t, h)
			if len(msgs) != tc.want {
				t.Fatalf("%d messages, want %d: %+v", len(msgs), tc.want, msgs)
			}
			if tc.want > 0 && msgs[0].Subject != tc.subject {
				t.Fatalf("subject %q, want %q", msgs[0].Subject, tc.subject)
			}
			// Only an approved issue that was notified moves; review stays
			// with its worker and everything else is untouched.
			moved := len(h.gh.history[1]) > 0
			if wantMove := tc.want > 0 && tc.state == "bees:approved"; moved != wantMove {
				t.Fatalf("label history %v, want moved=%v", h.gh.history[1], wantMove)
			}
		})
	}
}
