package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/workspace"
)

// Best of N. An issue whose size roles.developer.best_of_n_by_size sets to
// N > 1 gets N developer sessions for its first develop round instead of
// one, each on a branch and worktree of its own (attemptBranch), all
// running at once. Every attempt is a real developer session: it is
// recorded against the issue like any other, so the N of them spend one
// max_cost_per_issue budget together.
//
// Slots. The attempts are N concurrent agent processes, and max_developers
// is the only bound on those, so each attempt holds one of the pool's
// slots. All N are claimed at dispatch, all or none (claimSlots): a worker
// that took its own slot and then waited for N-1 more would hold what
// another fan-out is waiting for, and two of them on a pool smaller than
// their sum would wait for each other forever. N is clamped to
// max_developers first, because a pool can never supply more than it has.
// The worker gives the N-1 extra slots back the moment the attempts have
// finished, and runs the rest of the issue's life on the one slot every
// other worker has.
//
// The assembler. Once every attempt has ended, one more developer-role
// session runs on the issue's own branch (BranchFor) with the attempts
// listed in its task (task/developer_assemble.md): it reads them, decides
// what the result is — one attempt as it stands or a synthesis of several;
// the judgment is the session's, as a reviewer's verdict is — puts it on
// that branch and opens the pull request from it, exactly as a single
// developer session does. From the review stage on, nothing downstream can
// tell the two apart. An attempt whose session could not be run, or that
// pushed no commits, is listed as not a candidate, so an empty branch is
// never taken for a solution; when no attempt is a candidate there is
// nothing to assemble, and the issue is handed to a person instead.
//
// Cleanup is mechanical and lives here, not in the assembler's prompt: the
// attempt worktrees go when the attempts end, and every attempt branch is
// deleted, on the remote and in the main clone, once the fan-out is over —
// whatever the assembler came to, and when there was nothing to assemble —
// so a branch nobody will read again does not outlive the pull request.
// Deleting the head branch of a pull request an attempt opened closes it.
// The branches stay when the fan-out is going to be retried: on the
// account-wide session limit, which pauses the factory, and when reading
// what the attempts came to fails before the assembler runs, which fails
// the worker. A retry's attempts resume on their branches.

// attemptBranch is the branch attempt i (1-based) of a fan-out works on:
// the issue's branch with "-attempt-<i>" appended. An issue that does not
// fan out works on BranchFor's name, unchanged.
func (s *Scheduler) attemptBranch(issue, i int) string {
	return fmt.Sprintf("%s-attempt-%d", s.BranchFor(issue), i)
}

// attemptsFor is how many developer sessions the issue's next develop round
// runs, which is how many slots dispatch claims for it: best_of_n_by_size
// for the issue's size, clamped to max_developers, when that round is the
// first one; 1 for everything else. An issue whose branch already has an
// open pull request, or whose bookkeeping records a pull request or a
// later round, is a review loop being resumed, and a review round is one
// session however the size is configured.
//
// The clamp is applied silently here, where it is read on every dispatch of
// the issue; the worker logs it once, when the attempts start.
func (s *Scheduler) attemptsFor(issue github.Issue, snap *snapshot) int {
	role, err := s.cfg.Role(config.RoleDeveloper)
	if err != nil {
		return 1
	}
	n := role.BestOfN(s.sizeOf(issue.Labels))
	if n <= 1 || s.hasOpenPR(snap, issue) {
		return 1
	}
	if bk, err := s.store.Issue(issue.Number); err == nil && (bk.Round > 1 || bk.PR != 0) {
		return 1
	}
	return clampAttempts(n, s.cfg.Scheduler.MaxDevelopers)
}

// clampAttempts bounds a configured attempt count by the size of the slot
// pool: claiming more slots than exist would wait forever.
func clampAttempts(n, maxDevelopers int) int {
	if n > maxDevelopers {
		return maxDevelopers
	}
	return n
}

// claimSlots takes n slots from the pool without waiting, all of them or
// none: a partial claim would hold slots the pool cannot complete while
// another claim waits on them.
func (s *Scheduler) claimSlots(n int) bool {
	for i := 0; i < n; i++ {
		select {
		case <-s.slots:
		default:
			s.releaseSlots(i)
			return false
		}
	}
	return true
}

// releaseSlots gives n slots back to the pool.
func (s *Scheduler) releaseSlots(n int) {
	for i := 0; i < n; i++ {
		s.slots <- struct{}{}
	}
}

// fanOut is one first develop round run as N attempts.
type fanOut struct {
	issue     github.Issue
	worker    *state.Worker
	attempts  int
	base      string
	inbox     []mail.Message
	maxRounds int
	parent    *github.Parent
	log       *slog.Logger
	// ws is the worker's own worktree on the issue's branch, where the
	// assembler runs; release gives the extra slots back to the pool.
	ws      *workspace.Workspace
	release func()
}

// attempt is what one attempt of a fan-out came to.
type attempt struct {
	branch string
	// status and note are the session's outcome (outcomeOf); pr the pull
	// request it reported, if any.
	status, note string
	pr           int
	// err is set when the session could not be run at all, in which case
	// status and note are empty.
	err error
}

// runAttempts creates one worktree per attempt, runs the attempts at once
// and waits for all of them. It returns what each attempt came to, and the
// worktrees, which the caller removes. A worktree that cannot be created
// escalates the issue and returns an error, like the worker's own.
//
// One attempt failing to run does not stop the others: the error is
// reported once every attempt has ended, so the branches the rest pushed
// are complete when the assembler reads them. The one exception is the
// account-wide session limit (errSessionLimited), which every attempt
// would hit alike and which is returned as it is, so the caller pauses the
// factory rather than giving the issue up.
func (s *Scheduler) runAttempts(ctx context.Context, f fanOut) ([]attempt, []*workspace.Workspace, error) {
	if configured := s.configuredAttempts(f.issue); configured > f.attempts {
		f.log.Warn("best-of-N clamped to max_developers", "issue", f.issue.Number, "size", s.sizeOf(f.issue.Labels), "best_of_n", configured, "max_developers", s.cfg.Scheduler.MaxDevelopers, "attempts", f.attempts)
	}
	f.log.Info("best-of-N: running the attempts", "attempts", f.attempts, "mail", len(f.inbox))
	var wss []*workspace.Workspace
	for i := 1; i <= f.attempts; i++ {
		branch := s.attemptBranch(f.issue.Number, i)
		ws, err := s.ws.Branch(ctx, fmt.Sprintf("%s-attempt-%d", f.worker.Name, i), branch, f.base)
		if err != nil {
			_ = s.escalate(ctx, f.issue.Number, "Could not create a worktree for branch `"+branch+"`: "+err.Error())
			return nil, wss, fmt.Errorf("workspace: %w", err)
		}
		wss = append(wss, ws)
	}
	results := make([]attempt, f.attempts)
	var wg sync.WaitGroup
	for i := 1; i <= f.attempts; i++ {
		wg.Add(1)
		go func(i int, ws *workspace.Workspace) {
			defer wg.Done()
			a := attempt{branch: ws.Branch}
			// No worker: the attempts share one, and the retry and sandbox
			// marks N sessions would write on it would only overwrite each
			// other. The worker's stage names the fan-out instead.
			res, err := s.runSessionWithRetry(ctx, sessionSpec{
				role: config.RoleDeveloper, name: fmt.Sprintf("developer-issue-%d-attempt-%d", f.issue.Number, i),
				workDir: ws.RepoDir, branch: ws.Branch, attempt: i,
				data: prompts.Data{Issue: &f.issue, Inbox: f.inbox, Round: 1, MaxRounds: f.maxRounds, Parent: f.parent, BaseBranch: f.base},
			})
			if err != nil {
				a.err = err
			} else {
				a.status, a.note = outcomeOf(res)
				a.pr = res.Outcome.PR
			}
			results[i-1] = a
		}(i, wss[i-1])
	}
	wg.Wait()
	var errs []error
	for _, a := range results {
		if a.err != nil {
			errs = append(errs, a.err)
		}
	}
	for _, err := range errs {
		if errors.Is(err, errSessionLimited) {
			return results, wss, err
		}
	}
	if len(errs) > 0 {
		return results, wss, fmt.Errorf("attempt: %w", errs[0])
	}
	return results, wss, nil
}

// configuredAttempts is best_of_n_by_size for the issue's size, before the
// clamp; 1 when the developer role cannot be resolved.
func (s *Scheduler) configuredAttempts(issue github.Issue) int {
	role, err := s.cfg.Role(config.RoleDeveloper)
	if err != nil {
		return 1
	}
	return role.BestOfN(s.sizeOf(issue.Labels))
}

// assemble is the fan-out from start to finish: the attempts, then the
// assembler session on the worker's own worktree, then the cleanup. It
// returns the assembler's result and the time it started, for the worker to
// read exactly as it reads a single developer session's; a nil result with a
// nil error means the issue was handed to a person here and the worker is
// done. An attempt that could not be run stops nothing while another
// attempt is a candidate: it is logged, and the assembler is told.
func (s *Scheduler) assemble(ctx context.Context, f fanOut) (*session.Result, time.Time, error) {
	attempts, wss, err := s.runAttempts(ctx, f)
	// The attempts are over, whatever they came to: the pool gets its
	// slots back before anything else runs, and the worktrees go — the
	// branches are the attempts' work now.
	f.release()
	for _, aws := range wss {
		if rmErr := s.ws.Remove(context.WithoutCancel(ctx), aws); rmErr != nil {
			f.log.Warn("workspace cleanup failed", "branch", aws.Branch, "err", rmErr)
		}
	}
	if errors.Is(err, errSessionLimited) {
		return nil, time.Time{}, err
	}
	deleteBranches := func() {
		for _, a := range attempts {
			if delErr := s.ws.DeleteBranch(context.WithoutCancel(ctx), a.branch); delErr != nil {
				f.log.Warn("could not delete the attempt branch", "branch", a.branch, "err", delErr)
			}
		}
	}
	// Reading the attempts can fail for reasons that are none of theirs (a
	// fetch that hits the network, a lock). What they pushed is the
	// fan-out's work so far, and the worker's retry starts from it: the
	// branches stay, as they do for the session limit.
	data, dataErr := s.attemptData(ctx, attempts, f.base)
	if dataErr != nil {
		return nil, time.Time{}, dataErr
	}
	candidates := 0
	for _, a := range data {
		if a.Candidate {
			candidates++
		}
	}
	if candidates == 0 {
		deleteBranches()
		if err != nil {
			return nil, time.Time{}, err
		}
		return nil, time.Time{}, s.escalate(ctx, f.issue.Number, nothingToAssembleReason(data))
	}
	if err != nil {
		f.log.Warn("an attempt could not be run; assembling from the rest", "candidates", candidates, "err", err)
	}
	s.updateWorker(f.worker, "assembler", 1)
	f.log.Info("best-of-N: running the assembler", "candidates", candidates, "attempts", len(attempts))
	started := s.now()
	res, err := s.runSessionWithRetry(ctx, sessionSpec{
		role: config.RoleDeveloper, name: fmt.Sprintf("developer-issue-%d-assemble", f.issue.Number),
		workDir: f.ws.RepoDir, branch: f.ws.Branch, worker: f.worker, assembler: true, task: "developer_assemble",
		data: prompts.Data{Issue: &f.issue, Inbox: f.inbox, Round: 1, MaxRounds: f.maxRounds, Parent: f.parent, BaseBranch: f.base, Attempts: data},
	})
	// The assembler has ended, whatever it came to: what it pushed is on
	// the issue's branch, and the attempt branches have nothing left to
	// say. A session the limit stopped is the one that keeps them.
	if !errors.Is(err, errSessionLimited) {
		deleteBranches()
	}
	return res, started, err
}

// attemptData is what the assembler is told about each attempt: its branch,
// how many commits that branch carries beyond the base, and what its
// session reported. The main clone is fetched first, so the counts are the
// remote's and the assembler's worktree sees the same refs.
func (s *Scheduler) attemptData(ctx context.Context, attempts []attempt, base string) ([]prompts.Attempt, error) {
	if err := s.ws.Fetch(ctx); err != nil {
		return nil, fmt.Errorf("fetch: %w", err)
	}
	data := make([]prompts.Attempt, 0, len(attempts))
	for _, a := range attempts {
		n, err := s.ws.CommitsAhead(ctx, a.branch, base)
		if err != nil {
			return nil, fmt.Errorf("attempt %s: %w", a.branch, err)
		}
		d := prompts.Attempt{Branch: a.branch, Commits: n, Outcome: a.status, Note: a.note, PR: a.pr, Candidate: n > 0}
		if a.err != nil {
			d.Outcome, d.Note = OutcomeFailed, "the session could not be run: "+a.err.Error()
		}
		data = append(data, d)
	}
	return data, nil
}

// nothingToAssembleReason is the escalation for a fan-out none of whose
// attempts pushed a commit: what each session reported, so a person can see
// why. The branches are deleted by then, and named for the session log.
func nothingToAssembleReason(attempts []prompts.Attempt) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Best of N ran %d developer attempts on this issue and none of them pushed a commit, so there was nothing to assemble a pull request from.\n", len(attempts))
	for _, a := range attempts {
		fmt.Fprintf(&b, "\n- `%s`: `%s`", a.Branch, a.Outcome)
		if a.PR > 0 {
			fmt.Fprintf(&b, ", pull request #%d", a.PR)
		}
		if a.Note != "" {
			fmt.Fprintf(&b, ": %s", oneLine(a.Note, noteLimit))
		}
	}
	return b.String()
}
