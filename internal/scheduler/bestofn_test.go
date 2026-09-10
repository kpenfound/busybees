package scheduler

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/session"
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

// seedSized adds a ready issue of the given size, an hour old, whose pull
// request (200+n) appears once a developer session writes the fake gh's
// marker for the issue: the attempts write it, and the assembler's
// pr-opened is then located on the issue's own branch as any developer's.
func seedSized(h *harness, n int, size string) {
	seedReady(h, n, size, time.Now().Add(-time.Hour))
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
// issue's one running total. Then one assembler session runs on the issue's
// own branch, and what it pushes there goes to review as any developer's
// pull request does; every attempt branch and worktree is gone by then.
func TestBestOfNRunsOneAttemptPerSlot(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("FAKE_WAIT_FOR", release)
	t.Setenv("FAKE_COST", "1.0")
	seedSized(h, 1, "l")
	seedSized(h, 2, "s") // behind #1 in the queue: large-first
	events := h.sched.Subscribe()

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

	// One session per attempt, named for it, in no particular order, and
	// the assembler after all of them.
	got := h.sessionNames()
	slices.Sort(got)
	want := []string{"developer-issue-1-assemble", "developer-issue-1-attempt-1", "developer-issue-1-attempt-2", "developer-issue-1-attempt-3"}
	if !slices.Equal(got, want) {
		t.Errorf("sessions: got %v want %v", got, want)
	}
	if order := h.sessionOrder(); !strings.Contains(order[len(order)-1], "-assemble") {
		t.Errorf("the assembler did not run last: %v", order)
	}
	// The worker went through the fan-out and then the assembler.
	if stages := stagesOf(events, 1); !slices.Equal(stages, []string{"fan-out", "assembler"}) {
		t.Errorf("worker stages: got %v want [fan-out assembler]", stages)
	}
	// The assembler pushed the issue's own branch, with one attempt's work
	// on it; every attempt branch is gone from the remote and the clone.
	if got, want := remoteBranches(t, h), []string{"bees/issue-1", "main"}; !slices.Equal(got, want) {
		t.Errorf("remote branches: got %v want %v", got, want)
	}
	if got := localBranches(t, h, "bees/issue-1-attempt-*"); len(got) != 0 {
		t.Errorf("attempt branches left in the clone: %v", got)
	}
	if out, _ := workspace.Git(ctx, h.clone, "worktree", "list"); strings.Count(out, "\n") != 0 {
		t.Errorf("worktrees left behind:\n%s", out)
	}
	// The fake assembler takes one attempt whole, so the issue's branch
	// carries exactly that attempt's one commit.
	assembled := assembledFrom(t, h)
	log, err := workspace.Git(ctx, h.clone, "log", "--format=%s", "origin/main..origin/bees/issue-1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(log, "\n") != 0 || !strings.HasPrefix(log, "work ") {
		t.Errorf("origin/bees/issue-1 does not carry exactly the commit of %s:\n%s", assembled, log)
	}
	// The extra slots went back to the pool with the attempts, and the
	// worker's own with the worker.
	if got := freeSlots(h); got != 3 {
		t.Errorf("free slots after the fan-out: got %d want 3", got)
	}
	// Every attempt and the assembler were recorded against the issue.
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.Cost != 4.0 || bk.Sessions != 4 {
		t.Errorf("issue spend: $%.2f over %d sessions, want $4.00 over 4", bk.Cost, bk.Sessions)
	}
	// From the review stage on this is any developer's pull request: with
	// the reviewer disabled it is approved at once.
	if bk.PR != 201 {
		t.Errorf("issue #1 pull request: got %d want 201", bk.PR)
	}
	if got := h.stateOfIssue(1); got != "approved" {
		t.Errorf("issue #1 state: got %q want approved (comments: %v)", got, h.gh.comments[1])
	}
	if !strings.Contains(h.logs.String(), "best-of-N: running the assembler") {
		t.Errorf("the assembler is not logged:\n%s", h.logs.String())
	}
}

// stagesOf drains the worker stage events published for the issue so far.
func stagesOf(events <-chan Event, issue int) []string {
	var stages []string
	for {
		select {
		case ev := <-events:
			if ev.Kind == EventStage && ev.Issue == issue {
				stages = append(stages, ev.Stage)
			}
		default:
			return stages
		}
	}
}

// localBranches lists the branches of the main clone matching pattern.
func localBranches(t *testing.T, h *harness, pattern string) []string {
	t.Helper()
	out, err := workspace.Git(context.Background(), h.clone, "branch", "--list", "--format=%(refname:short)", pattern)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Fields(out)
}

// assembledFrom is the attempt branch the fake assembler took whole.
func assembledFrom(t *testing.T, h *harness) string {
	t.Helper()
	for _, dir := range h.sessions(config.RoleDeveloper) {
		if !strings.Contains(filepath.Base(dir), "-assemble") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, "assembled.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	t.Fatal("no assembler session ran")
	return ""
}

// assemblerPrompt is the task the assembler session was given.
func assemblerPrompt(t *testing.T, h *harness) string {
	t.Helper()
	for _, dir := range h.sessions(config.RoleDeveloper) {
		if strings.Contains(filepath.Base(dir), "-assemble") {
			b, err := os.ReadFile(filepath.Join(dir, "prompt.md"))
			if err != nil {
				t.Fatal(err)
			}
			return string(b)
		}
	}
	t.Fatal("no assembler session ran")
	return ""
}

// The assembler is told what each attempt came to, and an attempt that
// pushed nothing — here, one whose session reported `failed` without a
// commit — is listed as not a candidate rather than offered as a solution:
// the assembler takes a candidate, and the attempt's branch goes with the
// rest.
func TestBestOfNAssemblerIsToldTheAttempts(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	t.Setenv("FAKE_ATTEMPT_FAIL", "2")
	seedSized(h, 1, "l")
	runPass(t, h)
	h.sched.wg.Wait()

	if n := len(h.sessions(config.RoleDeveloper)); n != 4 {
		t.Fatalf("developer sessions: %v", h.sessionNames())
	}
	prompt := assemblerPrompt(t, h)
	for _, want := range []string{
		"assemble the result for issue #1 from 3 attempts",
		"- `bees/issue-1-attempt-1`: 1 commit on top of `main`, reported `pr-opened` (pull request #101)",
		"- `bees/issue-1-attempt-2`: **not a candidate** — pushed no commits, reported `failed`: attempt 2 could not build",
		"- `bees/issue-1-attempt-3`: 1 commit on top of `main`, reported `pr-opened` (pull request #101)",
		"git reset --hard origin/<branch>",
		"--base main --head bees/issue-1",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the assembler's task lacks %q:\n%s", want, prompt)
		}
	}
	if got := assembledFrom(t, h); got != "bees/issue-1-attempt-1" {
		t.Errorf("assembled from %s, want the first candidate", got)
	}
	if got, want := remoteBranches(t, h), []string{"bees/issue-1", "main"}; !slices.Equal(got, want) {
		t.Errorf("remote branches: got %v want %v", got, want)
	}
	if got := localBranches(t, h, "bees/issue-1-attempt-*"); len(got) != 0 {
		t.Errorf("attempt branches left in the clone: %v", got)
	}
	if got := h.stateOfIssue(1); got != "approved" {
		t.Errorf("issue #1 state: got %q want approved (comments: %v)", got, h.gh.comments[1])
	}
}

// A fan-out none of whose attempts pushed a commit has nothing to assemble:
// no assembler session runs, the issue is handed to a person with what each
// session reported, and the empty branches are deleted all the same.
func TestBestOfNWithNothingToAssembleEscalates(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	t.Setenv("FAKE_ATTEMPT_FAIL", "all")
	seedSized(h, 1, "l")
	runPass(t, h)
	h.sched.wg.Wait()

	got := h.sessionNames()
	slices.Sort(got)
	want := []string{"developer-issue-1-attempt-1", "developer-issue-1-attempt-2", "developer-issue-1-attempt-3"}
	if !slices.Equal(got, want) {
		t.Errorf("sessions: got %v want %v", got, want)
	}
	if got := h.stateOfIssue(1); got != "needs-human" {
		t.Errorf("issue #1 state: got %q want needs-human", got)
	}
	comments := strings.Join(h.gh.comments[1], "\n")
	if !strings.Contains(comments, "Best of N ran 3 developer attempts on this issue and none of them pushed a commit") {
		t.Errorf("the escalation does not say what happened:\n%s", comments)
	}
	for _, b := range want {
		if !strings.Contains(comments, "`bees/issue-1-"+strings.TrimPrefix(b, "developer-issue-1-")+"`: `failed`: attempt") {
			t.Errorf("the escalation does not list %s with its outcome:\n%s", b, comments)
		}
	}
	if got, want := remoteBranches(t, h), []string{"main"}; !slices.Equal(got, want) {
		t.Errorf("remote branches: got %v want %v", got, want)
	}
	if got := localBranches(t, h, "bees/issue-1-attempt-*"); len(got) != 0 {
		t.Errorf("attempt branches left in the clone: %v", got)
	}
	if got := freeSlots(h); got != 3 {
		t.Errorf("free slots after the fan-out: got %d want 3", got)
	}
}

// The attempt branches are deleted whatever the assembler came to: one that
// reports `failed` escalates the issue as any developer session's failure
// does, and leaves no branch behind.
func TestBestOfNCleansUpAfterAFailedAssembler(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	t.Setenv("FAKE_ASSEMBLE_FAIL", "1")
	seedSized(h, 1, "l")
	runPass(t, h)
	h.sched.wg.Wait()

	if n := len(h.sessions(config.RoleDeveloper)); n != 4 {
		t.Fatalf("developer sessions: %v", h.sessionNames())
	}
	if got := h.stateOfIssue(1); got != "needs-human" {
		t.Errorf("issue #1 state: got %q want needs-human", got)
	}
	if comments := strings.Join(h.gh.comments[1], "\n"); !strings.Contains(comments, "The developer session ended with `failed`: no attempt builds") {
		t.Errorf("the escalation does not carry the assembler's note:\n%s", comments)
	}
	if got, want := remoteBranches(t, h), []string{"main"}; !slices.Equal(got, want) {
		t.Errorf("remote branches: got %v want %v", got, want)
	}
	if got := localBranches(t, h, "bees/issue-1-attempt-*"); len(got) != 0 {
		t.Errorf("attempt branches left in the clone: %v", got)
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
	want := []string{"developer-issue-1-assemble", "developer-issue-1-attempt-1", "developer-issue-1-attempt-2"}
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
// the assembler assembler_model and assembler_prompt, and each the size's
// ordinary model and the developer's prompt when they are not; a session
// that is neither never sees any of them.
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
		if len(dirs) != 4 {
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
			"best_of_n_by_size = { l = 3 }\nbest_of_n_model = \"sonnet\"\nbest_of_n_prompt = \"solve it your own way\"\n"+
				"assembler_model = \"haiku\"\nassembler_prompt = \"take the best of them\"\n", 1)
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
		if len(dirs) != 5 {
			t.Fatalf("developer sessions: %v", dirs)
		}
		for _, dir := range dirs {
			name := filepath.Base(dir)
			args, prompt := argsOf(t, dir), systemPromptOf(t, dir)
			switch {
			case strings.Contains(name, "-attempt-"):
				if got := argValue(args, "--model"); got != "sonnet" {
					t.Errorf("%s --model: got %q want sonnet", name, got)
				}
				if !strings.Contains(prompt, "solve it your own way") || strings.Contains(prompt, "the developer's own prompt") || strings.Contains(prompt, "take the best of them") {
					t.Errorf("%s: the attempt's prompt must replace the developer's", name)
				}
			case strings.Contains(name, "-assemble"):
				if got := argValue(args, "--model"); got != "haiku" {
					t.Errorf("%s --model: got %q want haiku", name, got)
				}
				if !strings.Contains(prompt, "take the best of them") || strings.Contains(prompt, "the developer's own prompt") || strings.Contains(prompt, "solve it your own way") {
					t.Errorf("%s: the assembler's prompt must replace the developer's", name)
				}
			default:
				// #2, size s, is one session on model_by_size and the developer's prompt.
				if got := argValue(args, "--model"); got != "haiku" {
					t.Errorf("%s --model: got %q want haiku", name, got)
				}
				if strings.Contains(prompt, "solve it your own way") || strings.Contains(prompt, "take the best of them") || !strings.Contains(prompt, "the developer's own prompt") {
					t.Errorf("%s: a single session must keep the developer's prompt", name)
				}
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

// An interrupted session is reported to the next session of its role, and
// the attempts of a fan-out are not that session: the report is about one
// branch, and each attempt works on its own. None of them is told; the
// assembler, which reads every branch, is; and the record does not outlive
// the fan-out.
func TestBestOfNAttemptsAreNotToldOfAnInterruptedSession(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	seedSized(h, 1, "l")
	killedSession(t, h, 1, config.RoleDeveloper, "developer-issue-1-attempt-2", map[string]string{
		session.TranscriptFile:  twoTurns,
		session.InterruptedFile: "stopped by bees kill\n",
	})
	h.sched.alive = func(int) bool { return false }
	runPass(t, h)
	h.sched.wg.Wait()

	// The sessions that ran, which the killed one's directory is not.
	ran := h.sessionOrder()
	if len(ran) != 4 {
		t.Fatalf("sessions: %v", ran)
	}
	for _, name := range ran {
		b, err := os.ReadFile(filepath.Join(h.store.SessionsDir(), name, "prompt.md"))
		if err != nil {
			t.Fatal(err)
		}
		told := strings.Contains(flowedPrompt(string(b)), "ran for this issue before you was stopped")
		if told != strings.Contains(name, "-assemble") {
			t.Errorf("%s: told of the interrupted session: %v", name, told)
		}
	}
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.Session != nil {
		t.Errorf("the record of the interrupted session outlived the attempts: %+v", bk.Session)
	}
}

// A local pass claims the attempts' slots from a cached snapshot and reads
// the issue live before starting anything; an issue that has gone gives
// every slot it claimed back, not just one.
func TestBestOfNReleasesTheSlotsOfAVanishedCandidate(t *testing.T) {
	h := newHarnessAt(t, bestOfNTOML, time.Date(2026, 1, 1, 9, 0, 0, 0, time.UTC))
	seedSized(h, 1, "l")
	// The first pass polls but dispatches nothing: the pool is held.
	if !h.sched.claimSlots(3) {
		t.Fatal("could not hold the pool")
	}
	runPass(t, h)
	h.gh.mu.Lock()
	h.gh.issues[1].State = "CLOSED"
	h.gh.mu.Unlock()
	h.sched.releaseSlots(3)
	// The second, inside poll_interval, is a local pass over the cached
	// snapshot, in which #1 is still ready.
	runPass(t, h)
	h.sched.wg.Wait()

	if got := len(h.sessions(config.RoleDeveloper)); got != 0 {
		t.Errorf("%d developer sessions ran for a closed issue", got)
	}
	if got := freeSlots(h); got != 3 {
		t.Errorf("free slots after the vanished candidate: got %d want 3", got)
	}
}

// A worker whose round turns out not to be a fan-out gives the extra slots
// back before its one session runs, not when the worker ends: the pool
// serves the next issue while that session runs.
func TestBestOfNGivesTheSlotsBackBeforeASingleSession(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("FAKE_WAIT_FOR", release)
	seedReady(h, 1, "l", time.Now().Add(-time.Hour))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The pull request appears after the poll: dispatch claims three slots,
	// the worker finds one round to run.
	if err := os.WriteFile(h.gh.prMarker+"-issue-1", nil, 0o644); err != nil {
		t.Fatal(err)
	}
	h.sched.dispatchDevelopers(ctx, snap, false)
	waitFor(t, 30*time.Second, "the developer session to start", func() bool {
		return len(h.sessions(config.RoleDeveloper)) == 1
	})
	if got := freeSlots(h); got != 2 {
		t.Errorf("free slots during the single session: got %d want 2", got)
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, h, cancel, time.Minute)
	h.wantOrder("developer-issue-1-r1")
}

// An attempt whose session could not be run at all — no outcome, an error
// from the runner — is listed to the assembler as `failed` with the error
// as its note and is no candidate, whatever its branch holds; the count of
// commits still comes from the remote.
func TestAttemptDataReportsAnAttemptThatCouldNotRun(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	pushCommit(t, h.clone, "bees/issue-1-attempt-1")
	data, err := h.sched.attemptData(context.Background(), []attempt{
		{branch: "bees/issue-1-attempt-1", status: "pr-opened", pr: fakePR},
		{branch: "bees/issue-1-attempt-2", err: errors.New("claude: no such agent")},
	}, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []prompts.Attempt{
		{Branch: "bees/issue-1-attempt-1", Commits: 1, Outcome: "pr-opened", PR: fakePR, Candidate: true},
		{Branch: "bees/issue-1-attempt-2", Outcome: "failed", Note: "the session could not be run: claude: no such agent"},
	}
	if !slices.Equal(data, want) {
		t.Errorf("attemptData:\n got %+v\nwant %+v", data, want)
	}
}

// The account-wide session limit stopping the assembler itself, after every
// attempt already pushed, is not "nothing to assemble": the attempt
// branches are exactly what the retry the factory pauses for needs to read,
// so cleanup leaves them, and the issue is not escalated for it.
func TestBestOfNKeepsAttemptBranchesWhenTheAssemblerHitsTheLimit(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	t.Setenv("FAKE_ASSEMBLE_LIMIT", strconv.FormatInt(time.Now().Add(time.Hour).Unix(), 10))
	seedSized(h, 1, "l")
	runPass(t, h)
	h.sched.wg.Wait()

	if n := len(h.sessions(config.RoleDeveloper)); n != 4 {
		t.Fatalf("developer sessions: %v", h.sessionNames())
	}
	if got := h.stateOfIssue(1); got == "needs-human" {
		t.Error("issue #1 was escalated for the account's limit")
	}
	if comments := strings.Join(h.gh.comments[1], "\n"); comments != "" {
		t.Errorf("issue #1 was commented on for the account's limit:\n%s", comments)
	}
	if got, want := remoteBranches(t, h), []string{"bees/issue-1-attempt-1", "bees/issue-1-attempt-2", "bees/issue-1-attempt-3", "main"}; !slices.Equal(got, want) {
		t.Errorf("remote branches: got %v want %v", got, want)
	}
}

// pushCommit creates branch on the remote with one empty commit on top of
// main, without checking anything out in the clone.
func pushCommit(t *testing.T, clone, branch string) {
	t.Helper()
	ctx := context.Background()
	tree, err := workspace.Git(ctx, clone, "rev-parse", "main^{tree}")
	if err != nil {
		t.Fatal(err)
	}
	commit, err := workspace.Git(ctx, clone, "-c", "user.email=t@e", "-c", "user.name=t", "commit-tree", tree, "-p", "main", "-m", "work")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workspace.Git(ctx, clone, "push", "-q", "origin", commit+":refs/heads/"+branch); err != nil {
		t.Fatal(err)
	}
}

// Reading what the attempts came to can fail for reasons that are none of
// theirs — the fetch that precedes the commit counts hits a network blip,
// a lock, a full disk. The attempts' pushed work is the fan-out's result
// so far, and a retry of the worker starts from it: the branches stay,
// exactly as they do when the assembler hits the session limit. The
// clone's fetch URL is broken while the attempts are held, with pushes
// still routed to the real origin, so the fetch is the only thing that
// fails and a deletion would go through.
func TestBestOfNKeepsAttemptBranchesWhenReadingThemFails(t *testing.T) {
	h := newHarness(t, bestOfNTOML)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("FAKE_WAIT_FOR", release)
	seedSized(h, 1, "l")

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the three attempts to start", func() bool {
		return len(h.sessions(config.RoleDeveloper)) == 3
	})
	origin, err := workspace.Git(ctx, h.clone, "remote", "get-url", "origin")
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"remote", "set-url", "--push", "origin", origin},
		{"remote", "set-url", "origin", filepath.Join(t.TempDir(), "gone.git")},
	} {
		if _, err := workspace.Git(ctx, h.clone, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	waitWorkers(t, h, cancel, time.Minute)
	if _, err := workspace.Git(ctx, h.clone, "remote", "set-url", "origin", origin); err != nil {
		t.Fatal(err)
	}

	// The attempts pushed; no assembler ran on data that could not be read.
	if n := len(h.sessions(config.RoleDeveloper)); n != 3 {
		t.Fatalf("developer sessions: %v", h.sessionNames())
	}
	if !strings.Contains(h.logs.String(), "developer worker failed") {
		t.Errorf("the worker did not fail:\n%s", h.logs.String())
	}
	if got := h.stateOfIssue(1); got == "needs-human" {
		t.Error("issue #1 was escalated for a fetch that failed")
	}
	if got, want := remoteBranches(t, h), []string{"bees/issue-1-attempt-1", "bees/issue-1-attempt-2", "bees/issue-1-attempt-3", "main"}; !slices.Equal(got, want) {
		t.Errorf("remote branches: got %v want %v", got, want)
	}
}

// moeTOML fans a large issue out to three experts on a pool of three
// developers: one with a prompt and a model of its own, one with a prompt
// only, and one with neither. The developer is the only role that runs, and
// the large issue is first in the queue.
const moeTOML = `
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
best_of_n_model = "sonnet-4"
best_of_n_prompt = "solve it your own way"
moe_experts_by_size = { l = ["backend", "frontend", "tests"] }
[roles.developer.moe_experts.backend]
prompt = "solve it in the server code"
model = "sonnet"
[roles.developer.moe_experts.frontend]
prompt = "solve it in the interface"
[roles.developer.moe_experts.tests]
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

// attemptSession is the directory of the session that ran attempt i of
// issue 1, or the assembler's for i == 0.
func attemptSession(t *testing.T, h *harness, i int) string {
	t.Helper()
	suffix := "-assemble"
	if i > 0 {
		suffix = fmt.Sprintf("-attempt-%d", i)
	}
	for _, dir := range h.sessions(config.RoleDeveloper) {
		if strings.Contains(filepath.Base(dir), "developer-issue-1"+suffix+"-") {
			return dir
		}
	}
	t.Fatalf("no session for developer-issue-1%s among %v", suffix, h.sessionNames())
	return ""
}

// wantExpertSession asserts attempt i ran on the given model with the
// given prompt replacing the developer's, and none of the other experts'.
func wantExpertSession(t *testing.T, h *harness, i int, model, prompt string) {
	t.Helper()
	dir := attemptSession(t, h, i)
	name := filepath.Base(dir)
	if got := argValue(argsOf(t, dir), "--model"); got != model {
		t.Errorf("%s --model: got %q want %q", name, got, model)
	}
	b, err := os.ReadFile(filepath.Join(dir, "system-prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	system := string(b)
	if !strings.Contains(system, prompt) {
		t.Errorf("%s: the prompt %q is missing from the system prompt", name, prompt)
	}
	for _, other := range []string{"solve it in the server code", "solve it in the interface", "the developer's own prompt", "solve it your own way"} {
		if other != prompt && strings.Contains(system, other) {
			t.Errorf("%s: the system prompt carries %q as well as %q", name, other, prompt)
		}
	}
}

// A size moe_experts_by_size names experts for runs one developer session
// per expert at once, on the numbered attempt branches and worktrees and in
// the slots best-of-N uses, so the fan-out holds one slot per expert and a
// ready issue behind it waits. Attempt i runs expert i's own model and
// prompt; an expert with no model runs the size's, and one with neither
// runs the developer's own prompt. Their cost lands in the issue's one
// running total, the assembler runs after them on the issue's own branch,
// and every attempt branch and worktree is gone by the time the pull
// request goes to review.
func TestMoERunsOneSessionPerExpert(t *testing.T) {
	h := newHarness(t, moeTOML)
	release := filepath.Join(t.TempDir(), "release")
	t.Setenv("FAKE_WAIT_FOR", release)
	t.Setenv("FAKE_COST", "1.0")
	seedSized(h, 1, "l")
	seedSized(h, 2, "s") // behind #1 in the queue: large-first
	events := h.sched.Subscribe()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.pass(ctx); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 30*time.Second, "the three experts to start", func() bool {
		return len(h.sessions(config.RoleDeveloper)) == 3
	})
	// All three slots are the fan-out's while the experts run: #2 got none.
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

	// One session per expert, numbered by its place in the list, and the
	// assembler after all of them.
	got := h.sessionNames()
	slices.Sort(got)
	want := []string{"developer-issue-1-assemble", "developer-issue-1-attempt-1", "developer-issue-1-attempt-2", "developer-issue-1-attempt-3"}
	if !slices.Equal(got, want) {
		t.Errorf("sessions: got %v want %v", got, want)
	}
	if order := h.sessionOrder(); !strings.Contains(order[len(order)-1], "-assemble") {
		t.Errorf("the assembler did not run last: %v", order)
	}
	if stages := stagesOf(events, 1); !slices.Equal(stages, []string{"fan-out", "assembler"}) {
		t.Errorf("worker stages: got %v want [fan-out assembler]", stages)
	}
	// Each attempt is its expert: backend names both, frontend a prompt
	// only, tests neither. best_of_n_model and best_of_n_prompt are
	// best-of-N's and reach no expert.
	wantExpertSession(t, h, 1, "sonnet", "solve it in the server code")
	wantExpertSession(t, h, 2, "opus", "solve it in the interface")
	wantExpertSession(t, h, 3, "opus", "the developer's own prompt")
	// The assembler is the same session best-of-N runs, told every branch.
	prompt := assemblerPrompt(t, h)
	for i := 1; i <= 3; i++ {
		if !strings.Contains(prompt, fmt.Sprintf("`bees/issue-1-attempt-%d`", i)) {
			t.Errorf("the assembler was not told of attempt %d:\n%s", i, prompt)
		}
	}
	if got, want := remoteBranches(t, h), []string{"bees/issue-1", "main"}; !slices.Equal(got, want) {
		t.Errorf("remote branches: got %v want %v", got, want)
	}
	if got := localBranches(t, h, "bees/issue-1-attempt-*"); len(got) != 0 {
		t.Errorf("attempt branches left in the clone: %v", got)
	}
	if out, _ := workspace.Git(ctx, h.clone, "worktree", "list"); strings.Count(out, "\n") != 0 {
		t.Errorf("worktrees left behind:\n%s", out)
	}
	if got := freeSlots(h); got != 3 {
		t.Errorf("free slots after the fan-out: got %d want 3", got)
	}
	// Every expert and the assembler were recorded against the issue: one
	// budget for the round.
	bk, err := h.store.Issue(1)
	if err != nil {
		t.Fatal(err)
	}
	if bk.Cost != 4.0 || bk.Sessions != 4 {
		t.Errorf("issue spend: $%.2f over %d sessions, want $4.00 over 4", bk.Cost, bk.Sessions)
	}
	if bk.PR != 201 {
		t.Errorf("issue #1 pull request: got %d want 201", bk.PR)
	}
	if got := h.stateOfIssue(1); got != "approved" {
		t.Errorf("issue #1 state: got %q want approved (comments: %v)", got, h.gh.comments[1])
	}
	logs := h.logs.String()
	if !strings.Contains(logs, "mixture of experts: running the attempts") || strings.Contains(logs, "best-of-N: running the attempts") {
		t.Errorf("the round is not logged as a mixture of experts:\n%s", logs)
	}
}

// More experts than max_developers run as many as the pool holds, the first
// ones in the list: the clamp is best-of-N's, and it keeps the list's order
// so attempt i is still expert i.
func TestMoEClampsToMaxDevelopers(t *testing.T) {
	toml := strings.Replace(moeTOML, "max_developers = 3\n", "max_developers = 2\n", 1)
	h := newHarness(t, toml)
	seedSized(h, 1, "l")
	runPass(t, h)
	h.sched.wg.Wait()

	got := h.sessionNames()
	slices.Sort(got)
	want := []string{"developer-issue-1-assemble", "developer-issue-1-attempt-1", "developer-issue-1-attempt-2"}
	if !slices.Equal(got, want) {
		t.Errorf("sessions: got %v want %v", got, want)
	}
	wantExpertSession(t, h, 1, "sonnet", "solve it in the server code")
	wantExpertSession(t, h, 2, "opus", "solve it in the interface")
	if !strings.Contains(h.logs.String(), "mixture of experts clamped to max_developers") {
		t.Errorf("the clamp is not logged:\n%s", h.logs.String())
	}
	if got := freeSlots(h); got != 2 {
		t.Errorf("free slots after the fan-out: got %d want 2", got)
	}
}

// The attempts a mixture-of-experts round runs carry the expert each ran
// as, and attemptData hands that name to the assembler with the branch; a
// best-of-N attempt names none. Run directly, on a fan-out built by hand,
// so the wiring from the expert list to the assembler's data is what is
// under test.
func TestMoEAttemptsCarryTheirExpert(t *testing.T) {
	h := newHarness(t, moeTOML)
	seedSized(h, 1, "l")
	ctx := context.Background()
	f := fanOut{
		issue: *h.gh.issues[1], worker: &state.Worker{Name: "dev-1", Issue: 1}, attempts: 2,
		experts: []string{"backend", "frontend", "tests"}, base: "main", log: h.sched.log,
	}
	attempts, wss, err := h.sched.runAttempts(ctx, f)
	for _, ws := range wss {
		if rmErr := h.sched.ws.Remove(ctx, ws); rmErr != nil {
			t.Error(rmErr)
		}
	}
	if err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 || attempts[0].expert != "backend" || attempts[1].expert != "frontend" {
		t.Errorf("attempts: got %+v want backend, frontend", attempts)
	}
	data, err := h.sched.attemptData(ctx, attempts, "main")
	if err != nil {
		t.Fatal(err)
	}
	want := []prompts.Attempt{
		{Branch: "bees/issue-1-attempt-1", Commits: 1, Outcome: "pr-opened", PR: fakePR, Candidate: true, Expert: "backend"},
		{Branch: "bees/issue-1-attempt-2", Commits: 1, Outcome: "pr-opened", PR: fakePR, Candidate: true, Expert: "frontend"},
	}
	if !slices.Equal(data, want) {
		t.Errorf("attemptData:\n got %+v\nwant %+v", data, want)
	}
	// An attempt past the end of the list, and a best-of-N round, name no
	// expert.
	if got := f.expert(3); got != "tests" {
		t.Errorf("expert(3): got %q want tests", got)
	}
	if got := f.expert(4); got != "" {
		t.Errorf("expert(4): got %q want none", got)
	}
	if got := (fanOut{attempts: 3}).expert(1); got != "" {
		t.Errorf("a best-of-N round's expert(1): got %q want none", got)
	}
}

// A size best_of_n_by_size fans out, and a size neither table names, are
// untouched by the expert path: the best-of-N attempts run the best-of-N
// model and prompt with no expert named, the single session runs the
// developer's own, and dispatch claims the same slots it always has. The
// expert table is set for another size so the path is reachable.
func TestMoELeavesBestOfNAndSingleSessionsAlone(t *testing.T) {
	toml := strings.Replace(moeTOML, "moe_experts_by_size = { l = [\"backend\", \"frontend\", \"tests\"] }\n",
		"best_of_n_by_size = { l = 3 }\nmoe_experts_by_size = { m = [\"backend\", \"frontend\", \"tests\"] }\n", 1)
	h := newHarness(t, toml)
	seedSized(h, 1, "l")
	seedReady(h, 2, "s", time.Now())
	snap, err := h.sched.poll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		issue    int
		attempts int
	}{{1, 3}, {2, 1}} {
		issue := *h.gh.issues[tc.issue]
		if got := h.sched.expertsFor(issue); got != nil {
			t.Errorf("expertsFor(#%d): got %v want none", tc.issue, got)
		}
		if got := h.sched.attemptsFor(issue, snap); got != tc.attempts {
			t.Errorf("attemptsFor(#%d): got %d want %d", tc.issue, got, tc.attempts)
		}
	}
	// #1 takes the whole pool; #2 runs in the pass after it.
	runPass(t, h)
	h.sched.wg.Wait()
	forcePoll(h)
	runPass(t, h)
	h.sched.wg.Wait()
	got := h.sessionNames()
	slices.Sort(got)
	want := []string{"developer-issue-1-assemble", "developer-issue-1-attempt-1", "developer-issue-1-attempt-2", "developer-issue-1-attempt-3", "developer-issue-2-r1"}
	if !slices.Equal(got, want) {
		t.Fatalf("sessions: got %v want %v", got, want)
	}
	for i := 1; i <= 3; i++ {
		wantExpertSession(t, h, i, "sonnet-4", "solve it your own way")
	}
	for _, dir := range h.sessions(config.RoleDeveloper) {
		if !strings.Contains(filepath.Base(dir), "developer-issue-2-r1") {
			continue
		}
		if got := argValue(argsOf(t, dir), "--model"); got != "haiku" {
			t.Errorf("#2 --model: got %q want haiku", got)
		}
		b, _ := os.ReadFile(filepath.Join(dir, "system-prompt.md"))
		if system := string(b); !strings.Contains(system, "the developer's own prompt") || strings.Contains(system, "solve it in the") || strings.Contains(system, "solve it your own way") {
			t.Errorf("#2 must run the developer's own prompt:\n%s", system)
		}
	}
	if logs := h.logs.String(); strings.Contains(logs, "mixture of experts") || !strings.Contains(logs, "best-of-N: running the attempts") {
		t.Errorf("the best-of-N round is not logged as one:\n%s", logs)
	}
}
