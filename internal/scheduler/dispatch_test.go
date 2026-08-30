package scheduler

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// testSizeOf reads a size label the way Scheduler.sizeOf does, without a
// scheduler.
func testSizeOf(labels []github.Label) string {
	for _, l := range labels {
		if s, ok := strings.CutPrefix(l.Name, "bees:size/"); ok {
			return s
		}
	}
	return ""
}

func TestSortReady(t *testing.T) {
	base := time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC)
	// Ages ascending with the number; 3 carries no size label at all and so
	// ranks as "m", tying with 5.
	sizes := map[int]string{1: "l", 2: "xs", 3: "", 4: "s", 5: "m", 6: "xs"}
	issues := func() []github.Issue {
		var out []github.Issue
		for n := 1; n <= 6; n++ {
			i := github.Issue{Number: n, CreatedAt: base.Add(time.Duration(n) * time.Hour)}
			if sizes[n] != "" {
				i.Labels = []github.Label{{Name: "bees:size/" + sizes[n]}}
			}
			out = append(out, i)
		}
		return out
	}

	for _, tc := range []struct {
		order string
		want  []int
	}{
		{config.DispatchSmallFirst, []int{2, 6, 4, 3, 5, 1}},
		{config.DispatchLargeFirst, []int{1, 3, 5, 4, 2, 6}},
		{config.DispatchOldest, []int{1, 2, 3, 4, 5, 6}},
		{"", []int{1, 2, 3, 4, 5, 6}},
	} {
		t.Run(tc.order, func(t *testing.T) {
			got := issues()
			sortReady(got, tc.order, testSizeOf)
			var nums []int
			for _, i := range got {
				nums = append(nums, i.Number)
			}
			if fmt.Sprint(nums) != fmt.Sprint(tc.want) {
				t.Fatalf("order %q: got %v, want %v", tc.order, nums, tc.want)
			}
		})
	}
}

func TestSizeRank(t *testing.T) {
	if sizeRank("xs") >= sizeRank("s") || sizeRank("s") >= sizeRank("m") || sizeRank("m") >= sizeRank("l") || sizeRank("l") >= sizeRank("xl") {
		t.Fatal("sizes are not ordered smallest first")
	}
	if sizeRank("") != sizeRank("m") || sizeRank("huge") != sizeRank("m") {
		t.Fatal("an unsized issue must rank as m")
	}
}

// rolesOffTOML disables every role but the developer. With the reviewer off a
// pull request is approved as soon as it is opened, so a dispatched issue's
// label history is bees:in-progress,bees:approved.
const rolesOffTOML = `
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

// seedReady adds a sized ready issue and the pull request the fake developer
// "opens" for it; the PR stays hidden until a developer session ran for the
// issue, so the issue counts as new work rather than a resumption.
func seedReady(h *harness, n int, size string, created time.Time) {
	seedIssue(h, n, "bees:ready", size, created)
	h.gh.hidden[200+n] = true
}

// seedIssue adds an issue in the given state and the pull request on its
// branch, visible from the start.
func seedIssue(h *harness, n int, state, size string, created time.Time) {
	labels := []github.Label{{Name: "bees"}, {Name: state}}
	if size != "" {
		labels = append(labels, github.Label{Name: "bees:size/" + size})
	}
	h.gh.issues[n] = &github.Issue{Number: n, Title: fmt.Sprintf("Issue %d", n), Body: "please", State: "OPEN", Labels: labels, CreatedAt: created}
	h.gh.prs[200+n] = &github.PR{Number: 200 + n, Title: fmt.Sprintf("Issue %d", n), State: "OPEN", Body: fmt.Sprintf("Closes #%d", n),
		HeadRefName: fmt.Sprintf("bees/issue-%d", n), BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
}

// dispatched lists the issues a developer worker picked up, smallest number
// first.
func dispatched(h *harness) []int {
	h.gh.mu.Lock()
	defer h.gh.mu.Unlock()
	var out []int
	for n, labels := range h.gh.history {
		for _, l := range labels {
			if l == "bees:in-progress" {
				out = append(out, n)
				break
			}
		}
	}
	sort.Ints(out)
	return out
}

func TestSmallFirstDispatchesTheSmallestIssues(t *testing.T) {
	h := newHarness(t, baseTOML+"dispatch_order = \"small-first\"\n"+rolesOffTOML)
	base := time.Now().Add(-24 * time.Hour)
	// The oldest issue is also the biggest: oldest-first would take it.
	seedReady(h, 1, "l", base)
	seedReady(h, 2, "m", base.Add(time.Hour))
	seedReady(h, 3, "xs", base.Add(2*time.Hour))
	seedReady(h, 4, "s", base.Add(3*time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dispatched(h); fmt.Sprint(got) != "[3 4]" {
		t.Fatalf("dispatched %v, want the two smallest issues [3 4]", got)
	}
}

func TestLargeFirstDispatchesTheBiggestIssues(t *testing.T) {
	h := newHarness(t, baseTOML+"dispatch_order = \"large-first\"\nmax_large_in_flight = 0\n"+rolesOffTOML)
	base := time.Now().Add(-24 * time.Hour)
	seedReady(h, 1, "xs", base)
	seedReady(h, 2, "l", base.Add(time.Hour))
	seedReady(h, 3, "s", base.Add(2*time.Hour))
	seedReady(h, 4, "l", base.Add(3*time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := dispatched(h); fmt.Sprint(got) != "[2 4]" {
		t.Fatalf("dispatched %v, want the two largest issues [2 4]", got)
	}
}

func TestMaxLargeInFlightHoldsBackTheSecondLargeIssue(t *testing.T) {
	h := newHarness(t, baseTOML+"dispatch_order = \"oldest\"\nmax_large_in_flight = 1\n"+rolesOffTOML)
	base := time.Now().Add(-24 * time.Hour)
	seedReady(h, 1, "l", base)
	seedReady(h, 2, "l", base.Add(time.Hour))
	seedReady(h, 3, "s", base.Add(2*time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Both slots are used, but the second one goes to the small issue behind
	// the capped one rather than idling.
	if got := dispatched(h); fmt.Sprint(got) != "[1 3]" {
		t.Fatalf("dispatched %v, want [1 3]: the second size/l issue is capped", got)
	}
	if !strings.Contains(h.logs.String(), "large issue waits, cap reached") {
		t.Fatalf("no log line about the cap:\n%s", h.logs.String())
	}
}

// A ready issue that already has a pull request — one that came back from
// approved for human feedback or a conflict — is a resumption: it goes ahead
// of new work whatever its size, and the large cap does not hold it back.
func TestReadyIssueWithAPullRequestIsDispatchedFirst(t *testing.T) {
	h := newHarness(t, baseTOML+"dispatch_order = \"small-first\"\nmax_large_in_flight = 1\n"+rolesOffTOML)
	base := time.Now().Add(-24 * time.Hour)
	// Both slots would go to the small fresh issues under small-first alone.
	seedReady(h, 1, "xs", base)
	seedReady(h, 2, "s", base.Add(time.Hour))
	// An in-progress large issue is resumed first and fills the large cap ...
	seedIssue(h, 3, "bees:in-progress", "l", base.Add(2*time.Hour))
	h.gh.hidden[203] = true
	// ... and a ready large issue with an open PR still goes before the fresh ones.
	seedIssue(h, 4, "bees:ready", "l", base.Add(3*time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// dispatched() would miss 3, which was in-progress already: look at
	// which developer sessions ran instead.
	var got []string
	for _, dir := range h.sessions(config.RoleDeveloper) {
		got = append(got, filepath.Base(dir))
	}
	if len(got) != 2 || !strings.Contains(got[0], "issue-3-") || !strings.Contains(got[1], "issue-4-") {
		t.Fatalf("developer sessions %v, want the resumptions 3 and 4 before any new work", got)
	}
	if strings.Contains(h.logs.String(), "large issue waits, cap reached") {
		t.Fatalf("the cap must not hold back an issue with an open PR:\n%s", h.logs.String())
	}
}

func TestOversizedReadyIssueGoesBackToTriage(t *testing.T) {
	// Default max_size ("l").
	h := newHarness(t, baseTOML+rolesOffTOML)
	base := time.Now().Add(-24 * time.Hour)
	seedReady(h, 1, "xl", base)
	seedReady(h, 2, "l", base.Add(time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.history[1], ","); got != "bees:triage" {
		t.Fatalf("issue 1 label history: %q, want bees:triage", got)
	}
	if github.HasLabel(h.gh.issues[1].Labels, "bees:ready") {
		t.Fatalf("issue 1 kept bees:ready: %v", h.gh.issues[1].Labels)
	}
	if !github.HasLabel(h.gh.issues[1].Labels, "bees:size/xl") {
		t.Fatalf("issue 1 lost its size: %v", h.gh.issues[1].Labels)
	}
	if len(h.gh.comments[1]) != 0 {
		t.Fatalf("the label move must not be commented on: %v", h.gh.comments[1])
	}
	// An issue exactly at max_size is still dispatched.
	if got := dispatched(h); fmt.Sprint(got) != "[2]" {
		t.Fatalf("dispatched %v, want only the size/l issue [2]", got)
	}
	if !strings.Contains(h.logs.String(), "ready issue is too big for a developer, back to triage") {
		t.Fatalf("no log line about the limit:\n%s", h.logs.String())
	}
}

func TestMaxSizeSendsAnythingBiggerBackToTriage(t *testing.T) {
	h := newHarness(t, baseTOML+`
[roles.developer]
enabled = false
max_size = "m"
`+rolesOffTOML)
	base := time.Now().Add(-24 * time.Hour)
	seedReady(h, 1, "l", base)
	seedReady(h, 2, "xl", base.Add(time.Hour))
	seedReady(h, 3, "m", base.Add(2*time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{1, 2} {
		if got := strings.Join(h.gh.history[n], ","); got != "bees:triage" {
			t.Fatalf("issue %d label history: %q, want bees:triage", n, got)
		}
	}
	if got := h.gh.history[3]; len(got) != 0 {
		t.Fatalf("issue 3 is within max_size and must be left alone: %v", got)
	}

	// A local pass works from the issues cached by the last poll: without
	// cacheIssue it would ask GitHub to move the labels again.
	h.sched.localPass(ctx)
	for _, n := range []int{1, 2} {
		if got := strings.Join(h.gh.history[n], ","); got != "bees:triage" {
			t.Fatalf("issue %d relabelled twice: %q", n, got)
		}
	}
}
