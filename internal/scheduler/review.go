package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
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
// is the only session of a review that has a tool at all.
//
// That is the first round of the review loop. A later round, after the
// developer answered a request for changes, runs no pipeline: the judge
// session is told the findings the first round's artifact holds and the
// commit that round read (verifyReview), and verifies them against the head
// as it stands. A requested review is always a full one.
//
// The pipeline is configured by roles.reviewer (config.ResolvedRole): its
// agent and model run every session, angles says which angles each size
// runs, and brief_model, judge_model and angle_models replace the model for
// the distiller, the judge session and one angle each, falling back to
// model where they are unset. That fallback is applied here, the way the
// developer's best_of_n_model is in sessions.go; config leaves the fields
// empty on purpose.
//
// The sessions run in the worker's checkout of the pull request's head,
// and the angles in a local clone of it under the review's artifact
// directory (review.Checkout's Clone seam, in place of `bees review`'s
// container clone over the network), so that the diff can be written
// beside the files for them to read without a stray file landing in the
// worker's own worktree. The clone is removed when the angles have run:
// nothing resumes a factory review. The artifact — the brief, each angle's
// run and the judge's list — is kept under the state directory's reviews/
// for a person to read.

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
// issue the work item it is charged to (0 for a requested review).
//
// An error is a review that produced no findings list: the context that
// could not be gathered, a distiller that briefed nothing, every angle
// failing. One angle failing among others is not: it is named in
// Review.Skipped and the rest are reviewed. What the sessions cost is
// recorded either way, in the ledger and against the issue, so the budgets
// see a review that failed halfway as well as one that ran.
func (s *Scheduler) runReview(ctx context.Context, log *slog.Logger, pr github.PR, dir, name string, issue int) (*prompts.Review, *review.Artifact, error) {
	role, err := s.cfg.Role(config.RoleReviewer)
	if err != nil {
		return nil, nil, err
	}
	agent := s.reviewAgent(role)
	distiller := *agent
	if role.BriefModel != "" {
		distiller.Model = role.BriefModel
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
	runner := &review.Runner{
		Pipeline:  &review.Pipeline{Client: s.gh, Project: project, Dir: dir},
		Distiller: &review.Distiller{Agent: &distiller, Dir: dir},
		Angles: &review.Angles{
			Agent: agent, Provider: agent.Provider, Model: agent.Model,
			Sized: role.Angles, Models: role.AngleModels,
			Checkout: &review.Checkout{Clone: cloneOf(dir)},
			Dir:      dir,
		},
		Storage: storage,
		// A fixed start: the artifact directory is named by it, and it has
		// to be known here to remove the clone from it when the run stops
		// before it returns the artifact.
		Now: func() time.Time { return started },
		Log: &reviewLog{log: log},
	}
	artifactDir := review.ArtifactDir(storage, ref, started)
	log.Info("reviewing", "pr", pr.Number, "agent", agent.Provider, "model", agent.Model, "artifact", artifactDir)
	a, err := runner.Run(ctx, ref)
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
// reviewer role's agent and model, held to the role's timeout and turn
// limit and given its environment, and run as the same executables the
// role's own sessions are.
func (s *Scheduler) reviewAgent(role config.ResolvedRole) *review.CLIAgent {
	return &review.CLIAgent{
		Provider:  role.Agent,
		Model:     role.Model,
		ClaudeBin: s.runner.ClaudeBin,
		CodexBin:  s.runner.CodexBin,
		Timeout:   role.Timeout,
		MaxTurns:  role.MaxTurns,
		Env:       role.Env,
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
func (s *Scheduler) verifyReview(log *slog.Logger, bk state.IssueState, pr int) (*prompts.Review, bool) {
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
// reported. A review that failed before the brief cost nothing that can be
// known, and is entered with an outcome of failed and no cost.
func (s *Scheduler) recordReview(name string, issue, pr int, artifact string, started time.Time, a *review.Artifact, runErr error) {
	e := state.LedgerEntry{
		Time:       s.now(),
		Role:       config.RoleReviewer,
		Session:    name,
		Issue:      issue,
		PR:         pr,
		DurationMS: s.now().Sub(started).Milliseconds(),
		Outcome:    "reviewed",
	}
	if runErr != nil {
		e.Outcome = OutcomeFailed
	}
	brief, runs := readReviewCosts(artifact, a)
	if brief != nil {
		e.CostUSD += brief.CostUSD
	}
	for _, run := range runs {
		e.CostUSD += run.CostUSD
		e.Turns += run.Turns
	}
	err := s.store.AppendLedger(e)
	s.op("ledger", err, "could not record the review in the ledger", "session", name, "error", err)
	s.recordIssueCost(issue, e.CostUSD)
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

// cloneOf makes the checkout the angle sessions run in: a local clone of
// src, the worker's checkout of the pull request's head, at src's HEAD and
// sharing its objects, so it costs no fetch and little disk. The pull
// request's head is what the worker checked out — fetched and pulled by
// the review loop, a detached checkout of the head branch for a requested
// review — so the clone is the head as the worker has it, whichever
// reference it was asked for.
func cloneOf(src string) func(context.Context, review.Ref, string) error {
	return func(ctx context.Context, _ review.Ref, dir string) error {
		head, err := workspace.Git(ctx, src, "rev-parse", "HEAD")
		if err != nil {
			return err
		}
		if _, err := workspace.Git(ctx, src, "clone", "-q", "--shared", "--no-checkout", src, dir); err != nil {
			return err
		}
		_, err = workspace.Git(ctx, dir, "checkout", "-q", "--detach", strings.TrimSpace(head))
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
