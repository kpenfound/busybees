package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/workspace"
)

// bestOfNTOML fans a large issue out to three attempts on a pool of three
// developers, with a prompt and a model of their own. The developer is the
// only role that runs, and the large issue is first in the queue.
const bestOfNTOML = `
version = 1
[project]
repo = "acme/widgets"
[scheduler]
poll_interval = "1s"
max_developers = 3
dispatch_order = "large-first"
[roles.developer]
model = "opus"
model_by_size = { s = "haiku" }
prompt = "the developer's own prompt"
best_of_n_by_size = { l = 3 }
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

// seedSized adds a ready issue of the given size with no pull request at
// all: a fan-out never looks one up, and the fake developer's markers must
// not conjure one on the issue's own branch.
func seedSized(h *harness, n int, size string) {
	h.gh.issues[n] = &github.Issue{Number: n, Title: fmt.Sprintf("Issue %d", n), Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/" + size}}, CreatedAt: time.Now().Add(-time.Hour)}
}

// remoteBranches lists the branches on the test origin, which is where every
// attempt pushes.
func remoteBranches(t *testing.T, h *harness) []string {
	t.Helper()
	out, err := workspace.Git(context.Background(), h.clone, "ls-remote", "--heads", "origin")
	if err != nil {
		t.Fatal(err)
	}
	var branches []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if _, ref, ok := strings.Cut(line, "\trefs/heads/"); ok {
			branches = append(branches, ref)
		}
	}
	slices.Sort(branches)
	return branches
}

// freeSlots is how many of the pool's slots nothing holds.
func freeSlots(h *harness) int { return len(h.sched.slots) }

// A size configured for N > 1 runs N developer sessions at once, each on
// its own worktree and branch and each in a slot of the pool, so the fan-out
// holds N slots and a ready issue behind it waits. Their cost lands in the
// issue's one running total. Nothing picks between them yet: the issue is
// handed to a person with the branches listed.
func TestBestOfNRunsOneAttemptPerSlot(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("FAKE_WAIT_FOR", release)
	t.Setenv("FAKE_COST", "1.0")
	seedSized(h, 1, "l")
	seedSized(h, 2, "s") // behind #1 in the queue: large-first

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the three attempts to start", func() bool {
		return len(h.sessions(config.RoleDeveloper)) == 3
	})
	// All three slots are the fan-out's while the attempts run: #2 got none.
	if got := freeSlots(h); got != 0 {
		t.Errorf("free slots during the fan-out: got %d want 0", got)
	}
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 3 {
		t.Errorf("a second pass dispatched into a full pool: %d developer sessions", n)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if len(st.Workers) != 1 || st.Workers[0].Issue != 1 || st.Workers[0].Stage != "fan-out" {
		t.Errorf("workers during the fan-out: %+v", st.Workers)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, h, cancel, time.Minute)

	// One session per attempt, named for it, in no particular order.
	got := h.sessionNames()
	slices.Sort(got)
	want := []string{"developer-issue-1-attempt-1", "developer-issue-1-attempt-2", "developer-issue-1-attempt-3"}
	if !slices.Equal(got, want) {
		t.Errorf("sessions: got %v want %v", got, want)
	}
	// Each pushed its own branch; the issue's own branch was never pushed.
	wantBranches := []string{"bees/issue-1-attempt-1", "bees/issue-1-attempt-2", "bees/issue-1-attempt-3", "main"}
	if got := remoteBranches(t, h); !slices.Equal(got, wantBranches) {
		t.Errorf("remote branches: got %v want %v", got, wantBranches)
	}
	// The extra slots went back to the pool with the attempts, and the
	// worker's own with the worker.
	if got := freeSlots(h); got != 3 {
		t.Errorf("free slots after the fan-out: got %d want 3", got)
	}
	// Every attempt was recorded against the issue.
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.Cost != 3.0 || bk.Sessions != 3 {
		t.Errorf("issue spend: $%.2f over %d sessions, want $3.00 over 3", bk.Cost, bk.Sessions)
	}
	// The issue is a person's now, with the branches to pick from.
	if got := h.stateOfIssue(1); got != "needs-human" {
		t.Errorf("issue #1 state: got %q want needs-human", got)
	}
	comments := strings.Join(h.gh.comments[1], "\n")
	for _, b := range wantBranches[:3] {
		if !strings.Contains(comments, "`"+b+"`: `pr-opened`, pull request #101") {
			t.Errorf("the escalation does not list %s with its outcome:\n%s", b, comments)
		}
	}
	if !strings.Contains(comments, "Best of N ran 3 developer attempts") {
		t.Errorf("the escalation does not say what happened:\n%s", comments)
	}
}

// A configured N larger than the pool is clamped to max_developers, with a
// log line saying so: claiming more slots than exist would wait forever.
func TestBestOfNClampsToMaxDevelopers(t *testing.T) {
	toml := strings.Replace(bestOfNTOML, "max_developers = 3\n", "max_developers = 2\n", 1)
	toml = strings.Replace(toml, "best_of_n_by_size = { l = 3 }\n", "best_of_n_by_size = { l = 5 }\n", 1)
	h := newHarness(t, toml)
	seedSized(h, 1, "l")
	runPass(t, h)
	h.sched.wg.Wait()

	got := h.sessionNames()
	slices.Sort(got)
	want := []string{"developer-issue-1-attempt-1", "developer-issue-1-attempt-2"}
	if !slices.Equal(got, want) {
		t.Errorf("sessions: got %v want %v", got, want)
	}
	if !strings.Contains(h.logs.String(), "best-of-N clamped to max_developers") {
		t.Errorf("the clamp is not logged:\n%s", h.logs.String())
	}
	if got := freeSlots(h); got != 2 {
		t.Errorf("free slots after the fan-out: got %d want 2", got)
	}
}

// A size with no best_of_n_by_size entry dispatches exactly as it always
// has, with the table set for another size: one session under its usual
// name, on the issue's own branch, in one slot.
func TestBestOfNLeavesOtherSizesAlone(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("FAKE_WAIT_FOR", release)
	seedReady(h, 1, "s", time.Now().Add(-time.Hour))

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the developer session to start", func() bool {
		return len(h.sessions(config.RoleDeveloper)) == 1
	})
	if got := freeSlots(h); got != 2 {
		t.Errorf("free slots during a single session: got %d want 2", got)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, h, cancel, time.Minute)

	h.wantOrder("developer-issue-1-r1")
	if got, want := remoteBranches(t, h), []string{"bees/issue-1", "main"}; !slices.Equal(got, want) {
		t.Errorf("remote branches: got %v want %v", got, want)
	}
	if got := h.stateOfIssue(1); got == "needs-human" {
		t.Errorf("a single session was handed to a person: %v", h.gh.comments[1])
	}
}

// An issue coming back for another round is one session however its size
// is configured: the fan-out is the first develop round only. Dispatch
// reads that off the poll and the issue's bookkeeping and claims one slot,
// so the round starts while the pool has just one; and a worker that finds
// a pull request dispatch did not see gives the extra slots back and runs
// one session all the same.
func TestBestOfNDoesNotFanOutAResumedIssue(t *testing.T) {
	old := time.Now().Add(-time.Hour)
	for _, tc := range []struct {
		name string
		seed func(t *testing.T, h *harness)
		// held is how many slots something else holds while the issue is
		// dispatched; afterPoll runs between the poll and the dispatch.
		held      int
		afterPoll func(t *testing.T, h *harness)
		want      string
	}{
		{
			name: "an open pull request the poll sees",
			seed: func(t *testing.T, h *harness) { seedIssue(h, 1, "bees:ready", "l", old) },
			held: 2, want: "developer-issue-1-r1",
		},
		{
			name: "a later round in the bookkeeping",
			seed: func(t *testing.T, h *harness) {
				seedReady(h, 1, "l", old)
				if err := h.store.SaveIssue(state.IssueState{Number: 1, Round: 2, PR: 201, Branch: "bees/issue-1"}); err != nil {
					t.Fatal(err)
				}
			},
			held: 2, want: "developer-issue-1-r2",
		},
		{
			name: "a pull request the poll missed",
			seed: func(t *testing.T, h *harness) { seedReady(h, 1, "l", old) },
			afterPoll: func(t *testing.T, h *harness) {
				if err := os.WriteFile(h.gh.prMarker+"-issue-1", nil, 0o644); err != nil {
					t.Fatal(err)
				}
			},
			want: "developer-issue-1-r1",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, bestOfNTOML)
			tc.seed(t, h)
			if !h.sched.claimSlots(tc.held) {
				t.Fatal("could not hold the slots")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			snap, err := h.sched.poll(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if tc.afterPoll != nil {
				tc.afterPoll(t, h)
			}
			h.sched.dispatchDevelopers(ctx, snap, false)
			waitWorkers(t, h, cancel, time.Minute)
			h.wantOrder(tc.want)
			h.sched.releaseSlots(tc.held)
			if got := freeSlots(h); got != 3 {
				t.Errorf("free slots after the round: got %d want 3", got)
			}
		})
	}
}

// An attempt runs best_of_n_model and best_of_n_prompt when they are set,
// and the size's ordinary model and the developer's prompt when they are
// not; a session that is not an attempt never sees them.
func TestBestOfNAttemptModelAndPrompt(t *testing.T) {
	systemPromptOf := func(t *testing.T, dir string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(dir, "system-prompt.md"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Run("unset: the size's model and the developer's prompt", func(t *testing.T) {
		h := newHarness(t, bestOfNTOML)
		seedSized(h, 1, "l")
		runPass(t, h)
		h.sched.wg.Wait()
		dirs := h.sessions(config.RoleDeveloper)
		if len(dirs) != 3 {
			t.Fatalf("developer sessions: %v", dirs)
		}
		for _, dir := range dirs {
			if got := argValue(argsOf(t, dir), "--model"); got != "opus" {
				t.Errorf("%s --model: got %q want opus", filepath.Base(dir), got)
			}
			if !strings.Contains(systemPromptOf(t, dir), "the developer's own prompt") {
				t.Errorf("%s: the developer's prompt is missing", filepath.Base(dir))
			}
		}
	})
	t.Run("set: the attempts' own", func(t *testing.T) {
		toml := strings.Replace(bestOfNTOML, "best_of_n_by_size = { l = 3 }\n",
			"best_of_n_by_size = { l = 3 }\nbest_of_n_model = \"sonnet\"\nbest_of_n_prompt = \"solve it your own way\"\n", 1)
		h := newHarness(t, toml)
		seedSized(h, 1, "l")
		seedReady(h, 2, "s", time.Now())
		// #1 takes the whole pool; #2 runs in the pass after it.
		runPass(t, h)
		h.sched.wg.Wait()
		forcePoll(h)
		runPass(t, h)
		h.sched.wg.Wait()
		dirs := h.sessions(config.RoleDeveloper)
		if len(dirs) != 4 {
			t.Fatalf("developer sessions: %v", dirs)
		}
		for _, dir := range dirs {
			name := filepath.Base(dir)
			args, prompt := argsOf(t, dir), systemPromptOf(t, dir)
			if strings.Contains(name, "-attempt-") {
				if got := argValue(args, "--model"); got != "sonnet" {
					t.Errorf("%s --model: got %q want sonnet", name, got)
				}
				if !strings.Contains(prompt, "solve it your own way") || strings.Contains(prompt, "the developer's own prompt") {
					t.Errorf("%s: the attempt's prompt must replace the developer's", name)
				}
				continue
			}
			// #2, size s, is one session on model_by_size and the developer's prompt.
			if got := argValue(args, "--model"); got != "haiku" {
				t.Errorf("%s --model: got %q want haiku", name, got)
			}
			if strings.Contains(prompt, "solve it your own way") || !strings.Contains(prompt, "the developer's own prompt") {
				t.Errorf("%s: a single session must keep the developer's prompt", name)
			}
		}
	})
}

// argValue is the value following a flag in a recorded command line.
func argValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// claimSlots takes all the slots it is asked for or none of them: a claim
// the pool cannot complete leaves the pool as it found it.
func TestClaimSlotsIsAllOrNothing(t *testing.T) {
	h := newHarness(t, bestOfNTOML) // a pool of three
	if h.sched.claimSlots(4) {
		t.Fatal("claimed four slots from a pool of three")
	}
	if got := freeSlots(h); got != 3 {
		t.Fatalf("a failed claim kept slots: %d free", got)
	}
	if !h.sched.claimSlots(3) {
		t.Fatal("could not claim the whole pool")
	}
	if got := freeSlots(h); got != 0 {
		t.Fatalf("free slots after claiming three: %d", got)
	}
	h.sched.releaseSlots(2)
	if got := freeSlots(h); got != 2 {
		t.Fatalf("free slots after releasing two: %d", got)
	}
	for n, max := range map[int]int{5: 2, 2: 2, 1: 3} {
		if got := clampAttempts(n, max); got != min(n, max) {
			t.Errorf("clampAttempts(%d, %d) = %d", n, max, got)
		}
	}
}
