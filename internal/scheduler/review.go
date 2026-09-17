package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/review"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/workspace"
)

// The reviewer's review. A pull request the factory reviews — a developer's
// in the review loop (developer.go), or one a person asked about
// (reviewrequests.go) — goes through internal/review's pipeline, the one
// `bees review` runs: its context is gathered, a distiller session briefs
// the change and sizes it, one session per angle the size calls for looks
// for problems from that angle alone, and the judge merges what they found
// into one list. The brief and the angle sessions are internal/review's
// read-only CLIAgent sessions, exactly as `bees review`'s own are: no
// factory session runner, no MCP server, no tool that writes, runs or
// fetches anything. The judge is deterministic code (review.Judge).
//
// What comes after the judge is the factory's: one reviewer session through
// the ordinary session runner, the judge session, which has the built-in
// MCP tools and posts the judge's list on the pull request with
// submit_review, every finding and untriaged, and decides the verdict. It
// is the only session of a review that has factory tools.
//
// That is the first round of the review loop. A later round, after the
// developer answered a request for changes, runs no pipeline: the judge
// session is told the findings the first round's artifact holds and the
// commit that round read (verifyReview), and verifies them against the head
// as it stands. A requested review is always a full one.
//
// The pipeline uses the reviewer's size-resolved profile, then its named
// brief and angle overrides. Only execution settings reach the host adapter;
// its read-only policy always takes precedence over a profile's sandbox.
//
// The judge runs in the worker's checkout of the pull request's head;
// the brief and angles use an independent clone under the review's artifact
// directory (review.Checkout's Clone seam, in place of `bees review`'s
// container clone over the network), so that the diff can be written
// beside the files for them to read without a stray file landing in the
// worker's own worktree. The diff itself is read from that clone too
// (cloneOf points review.CheckoutBaseRef at the merge base of the base
// branch and the head, which the worker's full history has), so a pull
// request with more changed files than GitHub's diff allows is reviewed
// all the same, when the worker's checkout is the head GitHub names for
// it; a requested review that could not check out the head runs from the
// default branch and reads the diff through gh, limit and all. The clone
// is removed when the angles have run: nothing resumes a factory review.
// The artifact — the brief, each angle's run and the judge's list — is
// kept under the state directory's reviews/ for a person to read.

// reviewPipelineFailure is what a review that could not run is reported as:
// the reason, for an escalation comment or the requested-review log line.
type reviewPipelineFailure struct {
	err error
}

func (e reviewPipelineFailure) Error() string { return "the review could not run: " + e.err.Error() }
func (e reviewPipelineFailure) Unwrap() error { return e.err }

// runReview runs the review pipeline on pr, whose head is checked out in
// dir, and returns what it found as the judge session is told it, with the
// artifact it was written to. name is the review's name in the ledger,
// issue the work item it is charged to (0 for a requested review), and
// round the review round. name also identifies its live activity until the
// judge session starts. size selects the reviewer profile for the work item.
//
// An error is a review that produced no findings list: the context that
// could not be gathered, a distiller that briefed nothing, every angle
// failing. One angle failing among others is not: it is named in
// Review.Skipped and the rest are reviewed. What the sessions cost is
// recorded either way, in the ledger and against the issue, so the budgets
// see a review that failed halfway as well as one that ran.
func (s *Scheduler) runReview(ctx context.Context, log *slog.Logger, pr github.PR, dir, name string, issue, round int, size string) (_ *prompts.Review, _ *review.Artifact, resultErr error) {
	activity := Event{Kind: EventReviewStarted, Activity: name, Role: config.RoleReviewer,
		Round: round, Started: s.now(), Phase: "brief", Work: ghwork.New(issue, pr.Number)}
	s.publish(activity)
	defer func() {
		activity.Kind, activity.Success = EventReviewEnded, resultErr == nil
		if resultErr != nil {
			activity.Err = resultErr.Error()
		}
		s.publish(activity)
	}()
	// Serialize counting and publication so concurrent completions cannot
	// publish counts out of order. An angle's terminal callback counts once.
	var progressMu sync.Mutex
	completed := map[string]bool{}

	role, err := s.cfg.Role(config.RoleReviewer)
	if err != nil {
		return nil, nil, err
	}
	role = role.ForSize(size)
	agent := s.reviewAgent(role)
	distiller := s.reviewAgent(role.ForBrief())
	angleAgents := map[string]*review.CLIAgent{}
	for angle := range role.AngleProfiles {
		angleAgents[angle] = s.reviewAgent(role.ForAngle(angle))
	}
	project, err := review.LoadProject(review.FindProject(dir))
	if err != nil {
		return nil, nil, reviewPipelineFailure{err}
	}
	ref := review.Ref{Repo: s.cfg.Project.Repo, Number: pr.Number}
	// The artifact directory is named by the second the review started, so
	// a second review of the pull request inside that second — a round
	// after a fast developer, or a clock a test holds still — would write
	// over the first: the stamp moves on until it names a directory that
	// is not there.
	storage := s.store.ReviewsDir()
	started := s.now()
	for {
		if _, err := os.Stat(review.ArtifactDir(storage, ref, started)); os.IsNotExist(err) {
			break
		}
		started = started.Add(time.Second)
	}
	checkout := &review.Checkout{Clone: cloneOf(dir, pr.HeadSHA, s.ws.RemoteName())}
	runner := &review.Runner{
		Pipeline:  &review.Pipeline{Client: s.gh, Project: project, Dir: dir, Checkout: checkout},
		Distiller: &review.Distiller{Agent: &reviewBriefAgent{CLIAgent: distiller, checkout: filepath.Join(review.ArtifactDir(storage, ref, started), review.CheckoutDir)}},
		Angles: &review.Angles{
			Agent: agent, Provider: agent.Provider, Model: agent.Model,
			Sized: role.Angles, Agents: angleAgents,
			Checkout: checkout,
		},
		Storage: storage,
		AnglesReady: func(angles []string) {
			activity.Kind, activity.Phase, activity.Total = EventReviewProgress, "angles", len(angles)
			s.publish(activity)
		},
		Progress: func(angle string, event review.AngleEvent) {
			if event != review.AngleFinished && event != review.AngleFailed {
				return
			}
			progressMu.Lock()
			defer progressMu.Unlock()
			if completed[angle] {
				return
			}
			completed[angle] = true
			activity.Completed = len(completed)
			s.publish(activity)
		},
		// A fixed start: the artifact directory is named by it, and it has
		// to be known here to remove the clone from it when the run stops
		// before it returns the artifact.
		Now: func() time.Time { return started },
		Log: &reviewLog{log: log},
	}
	artifactDir := review.ArtifactDir(storage, ref, started)
	log.Info("reviewing", "pr", pr.Number, "agent", agent.Provider, "model", agent.Model, "artifact", artifactDir)
	a, err := runner.Run(ctx, ref)
	if err == nil {
		err = ctx.Err()
	}
	if rmErr := os.RemoveAll(filepath.Join(artifactDir, review.CheckoutDir)); rmErr != nil {
		log.Warn("could not remove the review's clone", "dir", artifactDir, "err", rmErr)
	}
	s.recordReview(name, issue, pr.Number, artifactDir, started, a, err)
	if err != nil {
		return nil, nil, reviewPipelineFailure{err}
	}
	return reviewData(a), a, nil
}

// reviewAgent is the agent the brief and angle sessions run as: the
// selected profile's execution fields (sandbox ignored), held to the role's
// timeout and turn limit and given its environment, using the executables the
// role's own sessions are.
func (s *Scheduler) reviewAgent(role config.ResolvedRole) *review.CLIAgent {
	return &review.CLIAgent{
		Provider:      role.Agent,
		Model:         role.Model,
		FallbackModel: role.FallbackModel,
		Effort:        role.Effort,
		ClaudeBin:     s.runner.ClaudeBin,
		CodexBin:      s.runner.CodexBin,
		Timeout:       role.Timeout,
		MaxTurns:      role.MaxTurns,
		Env:           role.Env,
	}
}

// reviewData is what the judge session is told about a review: the brief's
// size and summary, the angles that ran and the ones that reviewed nothing,
// the judge's list rendered, and where the artifact is.
func reviewData(a *review.Artifact) *prompts.Review {
	r := &prompts.Review{Artifact: a.Dir}
	if a.Brief != nil {
		r.Size, r.Summary = a.Brief.Size, a.Brief.Summary
	}
	for _, run := range a.Runs {
		r.Angles = append(r.Angles, run.Angle)
	}
	if a.Findings != nil {
		r.Skipped = a.Findings.Skipped
		r.Count = len(a.Findings.Items)
		r.Findings = review.RenderFindings(a.Findings.Items)
	}
	return r
}

// verifyReview is what a later round of the review loop tells the judge
// session in place of a new review: the findings of the full review recorded
// in the issue's bookkeeping, to be checked against the head as it stands,
// and the commit that review read. ok is false when there is nothing to
// verify — no review recorded, one recorded for another pull request, or an
// artifact that can no longer be read — and the round runs the full review.
func (s *Scheduler) verifyReview(log *slog.Logger, bk state.WorkState, pr int) (*prompts.Review, bool) {
	if bk.ReviewArtifact == "" {
		return nil, false
	}
	ref := review.Ref{Repo: s.cfg.Project.Repo, Number: pr}
	if filepath.Dir(bk.ReviewArtifact) != filepath.Dir(review.ArtifactDir(s.store.ReviewsDir(), ref, time.Time{})) {
		return nil, false
	}
	a, err := review.ReadArtifact(bk.ReviewArtifact)
	if err != nil || a.Findings == nil {
		log.Warn("the previous review cannot be read; reviewing the change again", "artifact", bk.ReviewArtifact, "err", err)
		return nil, false
	}
	r := reviewData(a)
	r.Verify, r.ReviewedHead = true, bk.ReviewedHead
	return r, true
}

// recordReview enters what the review's sessions cost in the ledger, as one
// entry under name, and charges it to the issue: the distiller's cost from
// the brief and each angle's from its run, which are what the CLIs
// reported. A session that reported no cost, or an angle that failed and
// so reported nothing, leaves the entry's cost unknown: what the entry
// holds is then only what the other sessions cost. A review that failed
// without a brief is entered as failed with its cost unknown too: the
// distiller may have run and spent before it failed, and nothing says
// whether it did.
func (s *Scheduler) recordReview(name string, issue, pr int, artifact string, started time.Time, a *review.Artifact, runErr error) {
	e := state.LedgerEntry{
		Time:       s.now(),
		Role:       config.RoleReviewer,
		Session:    name,
		DurationMS: s.now().Sub(started).Milliseconds(),
		Outcome:    "reviewed",
		Work:       ghwork.New(issue, pr),
	}
	if runErr != nil {
		e.Outcome = OutcomeFailed
	}
	brief, runs := readReviewCosts(artifact, a)
	if brief != nil {
		e.CostUSD += brief.CostUSD
		e.CostUnknown = brief.CostUnknown
	} else if runErr != nil {
		e.CostUnknown = true
	}
	for _, run := range runs {
		e.CostUSD += run.CostUSD
		e.Turns += run.Turns
		e.CostUnknown = e.CostUnknown || run.CostUnknown || run.Error != ""
	}
	err := s.store.AppendLedger(e)
	s.op("ledger", err, "could not record the review in the ledger", "session", name, "error", err)
	s.recordWorkCost(ghwork.New(issue, pr), e.CostUSD)
}

// readReviewCosts is the brief and the angle runs a review wrote: from the
// artifact when the run returned one, and read back from its directory
// when it did not, so a review that failed after its sessions ran still
// has what they cost recorded.
func readReviewCosts(artifact string, a *review.Artifact) (*review.Brief, []review.AngleRun) {
	if a != nil {
		return a.Brief, a.Runs
	}
	brief, _ := review.ReadBrief(artifact)
	runs, _ := review.ReadAngleRuns(artifact)
	return brief, runs
}

// cloneOf makes the checkout the diff, brief and angle sessions read:
// an independent local clone of src, the worker's checkout of the pull
// request's head, at src's HEAD. Objects are copied, never shared or
// hard-linked to the worker. The pull request's head is what the worker
// checked out — fetched and pulled by the review loop, a detached checkout
// of the head branch for a requested review — so the clone is the head as
// the worker has it, whichever reference it was asked for.
//
// The clone's review.CheckoutBaseRef is the merge base of the head and
// the base branch as the worker's remote has it (fetched with the rest at
// the start of the round), which is what GitHub diffs a pull request
// against: the worker's history is whole, so the merge base is there to
// find, and the diff read from the clone is the pull request's own. The
// remote is the one the workspace manager fetches, project.remote, under
// whose name the worker's remote-tracking branches live: on a project
// whose team repository is `upstream` there is no origin/<base> to find.
//
// The reference is set only when the worker's HEAD is headSHA, the commit
// GitHub says the pull request's head is: a requested review of a fork's
// pull request, or of a branch deleted since, runs from a checkout of the
// default branch instead (runRequestedReview), whose diff against its own
// merge base is nothing, and the diff of such a review is gh's to read. A
// base branch the remote does not have, or one the head shares no history
// with, leaves the reference unset the same way.
func cloneOf(src, headSHA, remote string) func(context.Context, review.Ref, string, string) error {
	return func(ctx context.Context, _ review.Ref, base, dir string) error {
		head, err := workspace.Git(ctx, src, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		head = strings.TrimSpace(head)
		if _, err := workspace.Git(ctx, src, "clone", "-q", "--no-hardlinks", "--no-checkout", src, dir); err != nil {
			return err
		}
		if _, err := workspace.Git(ctx, dir, "checkout", "-q", "--detach", head); err != nil {
			return err
		}
		if base == "" || (headSHA != "" && head != headSHA) {
			return nil
		}
		mergeBase, err := workspace.Git(ctx, src, "merge-base", "refs/remotes/"+remote+"/"+base, "HEAD")
		if err != nil {
			return nil
		}
		_, err = workspace.Git(ctx, dir, "update-ref", review.CheckoutBaseRef, strings.TrimSpace(mergeBase))
		return err
	}
}

// reviewLog is the runner's log as scheduler log lines: one record per
// line, at info level, under the worker's logger.
type reviewLog struct {
	log *slog.Logger
}

func (l *reviewLog) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			l.log.Info("review: " + line)
		}
	}
	return len(p), nil
}

// reviewStatesFor are the GitHub review states that confirm a reviewer's
// verdict: changes-requested is CHANGES_REQUESTED, and nothing else;
// approved is APPROVED or COMMENTED, because GitHub refuses an approval
// from a pull request's own author and the reviewer is told to comment in
// its place — the common case on a shared account, where the factory is
// every author. Any other status is confirmed by a review in any state:
// what the review loop's judge session posts is a comment review, and the
// loop asks only whether it posted one.
func reviewStatesFor(status string) []string {
	switch status {
	case OutcomeChangesRequested:
		return []string{"CHANGES_REQUESTED"}
	case OutcomeApproved:
		return []string{"APPROVED", "COMMENTED"}
	}
	return []string{"APPROVED", "COMMENTED", "CHANGES_REQUESTED"}
}

// escalationFor is the escalation comment for a review that could not run
// on a developer's pull request.
func (e reviewPipelineFailure) escalation(pr int) string {
	return fmt.Sprintf("The review of pull request #%d could not run: %v. The reviewer's brief and angle sessions are configured by `roles.reviewer` in bees.toml.", pr, e.err)
}

// The brief reads the same isolated clone as the angles. If gathering could not
// clone it, use the distiller's empty scratch directory and its gathered bundle.
type reviewBriefAgent struct {
	*review.CLIAgent
	checkout string
}

func (a *reviewBriefAgent) Run(ctx context.Context, req review.AgentRequest) (*review.AgentResult, error) {
	if st, err := os.Stat(a.checkout); err == nil && st.IsDir() {
		req.Dir = a.checkout
	}
	return a.CLIAgent.Run(ctx, req)
}
