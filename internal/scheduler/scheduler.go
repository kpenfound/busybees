// Package scheduler is the busybees orchestrator loop.
//
// It polls GitHub for visible issues and pull requests, keeps the workflow
// state labels consistent, and dispatches Claude Code sessions:
//
//   - a pool of developer workers (scheduler.max_developers). Each worker
//     owns one issue and runs a sequential developer -> reviewer -> developer
//     loop until the reviewer approves or the round limit is hit;
//   - three singleton roles: product manager, project manager and QA, each
//     running at most one session at a time when they have work.
//
// All GitHub state transitions are made by the scheduler (never by the
// sessions) except the ones role prompts explicitly delegate, such as the
// project manager moving an issue from triage to ready.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/text"
	"github.com/kpenfound/busybees/internal/workspace"
)

// Deps are the collaborators the scheduler needs.
type Deps struct {
	Config     *config.Config
	GitHub     *github.Client
	Mail       *mail.Box
	Runner     *session.Runner
	Workspaces *workspace.Manager
	Store      *state.Store
	Logger     *slog.Logger
	// Version and Revision are the build the binary was started from:
	// what `bees version` prints, and the untruncated commit behind it.
	// cmd/bees resolves them (the `-ldflags -X main.version` override lives
	// in package main) and the scheduler only records them, in the startup
	// log line and in status.json, so a running factory can be attributed
	// to a build — the role prompts are compiled in, so a merged prompt
	// change reaches no session until bees is rebuilt and restarted. Both
	// are empty when the caller supplies none, and then nothing is recorded.
	Version  string
	Revision string
	// Now overrides the clock (tests).
	Now func() time.Time
}

// Scheduler runs the factory.
type Scheduler struct {
	cfg    *config.Config
	labels config.Labels
	query  github.Query
	gh     *github.Client
	mail   *mail.Box
	runner *session.Runner
	ws     *workspace.Manager
	store  *state.Store
	log    *slog.Logger
	now    func() time.Time
	// version and revision describe the build this scheduler is running as
	// (Deps.Version, Deps.Revision).
	version  string
	revision string

	// Once makes Run perform a single poll/dispatch pass and wait for the
	// work it started.
	Once bool
	// OnlyRoles restricts which roles may run (nil = all enabled roles).
	OnlyRoles map[string]bool

	mu       sync.Mutex
	owned    map[int]*state.Worker
	running  map[string]bool
	backoff  map[string]time.Time
	lastPoll time.Time
	nextPoll time.Time
	lastErr  string
	// degraded holds the current failure streak of every named operation
	// that is failing right now; a success deletes its entry (degraded.go).
	degraded map[string]*opFailure
	queues   map[string]int
	waiting  map[int][]int
	// warnedCycles remembers the issues we already warned about, so a
	// dependency cycle is reported once per process rather than per poll.
	warnedCycles map[int]bool
	// readySizes counts the ready queue by size label ("xs", "s", ...);
	// issues with no size label are counted under "".
	readySizes map[string]int
	// priority lists the ready issues carrying bees:priority, so
	// `bees status` can show that the lever took effect.
	priority []int
	// needsHuman and approved are the detail behind two of the queue counts:
	// the issues the factory gave up on (with the reason it recorded when it
	// did) and the pull requests waiting for a person to merge. Both are
	// built from the same snapshot the counts are, so neither costs a
	// GitHub call of its own.
	needsHuman []state.Escalated
	approved   []state.ApprovedPR
	// dayPaused is true while the rolling 24h spend has reached
	// scheduler.max_cost_per_day, and daySpend is that spend. It is in
	// memory only: a factory restarted while the window sits between
	// scheduler.max_cost_per_day_resume_percent and the budget starts
	// dispatching again rather than waiting the pause out.
	dayPaused bool
	daySpend  float64
	// limitPausedUntil is when dispatch resumes after a session hit the
	// account-wide claude session limit; zero when none is in force. It is
	// in memory only, like dayPaused: after a restart the first session
	// re-learns the limit and re-pauses.
	limitPausedUntil time.Time
	// overBudget counts consecutive over-budget sessions per work item.
	overBudget map[string]int
	// live holds every session running right now, by the name the event
	// stream publishes, and killed the ones a person stopped through
	// KillSession (kill.go). Both are the live view's half of the picture:
	// a name is all a view has, and these are what turn one back into a
	// process to stop and a work item to hand over.
	live   map[string]liveSession
	killed map[string]bool
	// interrupted holds, per issue, the session a killed scheduler or a
	// hard stop left unfinished, until the worker that took the issue over
	// runs a session of the role it happened to (interrupted.go).
	interrupted map[int]*session.Interrupted
	// alive answers whether a pid is still running, and is how an
	// interrupted session is told from a running one. nil is procs.Alive;
	// tests replace it so no test has to kill a real process.
	alive func(int) bool
	// wake shortens the wait between two ticks: a purely local event (a
	// session finishing, mail the scheduler itself sent) signals it and the
	// loop runs a local pass at once instead of sitting out the rest of the
	// poll interval. Buffered with one slot, so a burst of signals coalesces
	// into a single pass.
	wake  chan struct{}
	wg    sync.WaitGroup
	slots chan struct{}
	// sessionCtx is the context every session runs under while Run is
	// running, and stopSessions cancels it. It is derived from Run's context
	// but not cancelled with it: cancelling the loop stops polling and
	// dispatch and lets the work in flight finish — every running session
	// (each still bounded by its role's timeout), and every issue a
	// developer worker already holds, through the stages it has left
	// (workIssue) — which is the cool-down an interrupt or the live view's
	// stop key asks for. HardStop is the other stop: it cancels this
	// context, which kills every running session's process group. Both are
	// nil outside Run — a session started another way (`bees exec`) runs
	// under its caller's context and dies with it, as it always has.
	sessionCtx   context.Context
	stopSessions context.CancelFunc
	// evMu guards subs, the event stream's subscribers (events.go). It is
	// its own lock: publishing must never contend with, or depend on, the
	// state mu protects.
	evMu sync.Mutex
	subs []chan Event
	// Issues and PRs from the last successful poll, reused by local passes.
	lastIssues []github.Issue
	lastPRs    []github.PR
	polled     bool
}

// New builds a scheduler.
func New(d Deps) (*Scheduler, error) {
	if d.Config == nil || d.GitHub == nil || d.Mail == nil || d.Runner == nil || d.Workspaces == nil || d.Store == nil {
		return nil, errors.New("scheduler: missing dependency")
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	f := d.Config.Filter
	q := github.Query{Assignee: f.Assignee, Milestone: f.Milestone}
	if f.LabelRequired() {
		q.Label = f.Label
	}
	s := &Scheduler{
		cfg:          d.Config,
		labels:       d.Config.Labels(),
		query:        q,
		gh:           d.GitHub,
		mail:         d.Mail,
		runner:       d.Runner,
		ws:           d.Workspaces,
		store:        d.Store,
		log:          d.Logger,
		now:          d.Now,
		version:      d.Version,
		revision:     d.Revision,
		owned:        map[int]*state.Worker{},
		running:      map[string]bool{},
		backoff:      map[string]time.Time{},
		degraded:     map[string]*opFailure{},
		queues:       map[string]int{},
		waiting:      map[int][]int{},
		warnedCycles: map[int]bool{},
		readySizes:   map[string]int{},
		live:         map[string]liveSession{},
		killed:       map[string]bool{},
		overBudget:   map[string]int{},
		interrupted:  map[int]*session.Interrupted{},
		wake:         make(chan struct{}, 1),
		slots:        make(chan struct{}, d.Config.Scheduler.MaxDevelopers),
	}
	for i := 0; i < d.Config.Scheduler.MaxDevelopers; i++ {
		s.slots <- struct{}{}
	}
	return s, nil
}

// Run executes the loop until ctx is cancelled (or, with Once, until one
// pass and the work it started have completed). Cancelling ctx stops
// nothing that is already running: the loop takes no new issue and starts no
// singleton, and waits for the work in flight — the running sessions, and
// the rest of the loop of every issue a developer worker holds — to finish.
// HardStop is what stops that too.
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.store.Init(); err != nil {
		return err
	}
	// The sessions' own context (see the field's comment): cancelling ctx
	// must leave running sessions alone, so they run under a context that
	// only HardStop — and the deferred cancel, after every session has
	// already finished — cancels.
	sessionCtx, stopSessions := context.WithCancel(context.WithoutCancel(ctx))
	s.mu.Lock()
	s.sessionCtx, s.stopSessions = sessionCtx, stopSessions
	s.mu.Unlock()
	defer func() {
		stopSessions()
		// Cleared so a session run outside a loop afterwards (tests drive
		// Run more than once) is not handed a cancelled context.
		s.mu.Lock()
		s.sessionCtx, s.stopSessions = nil, nil
		s.mu.Unlock()
	}()
	if err := s.ws.Prune(ctx); err != nil {
		s.log.Warn("worktree prune failed", "err", err)
	}
	if err := s.ensureLabels(ctx); err != nil {
		s.log.Warn("could not ensure labels", "err", capErrors(err))
	}
	s.log.Info("scheduler started", "repo", s.cfg.Project.Repo, "filter", s.describeQuery(),
		"max_developers", s.cfg.Scheduler.MaxDevelopers, "poll", s.cfg.Scheduler.PollInterval.Duration,
		"work_hours", s.cfg.Scheduler.WorkHours, "version", s.version)
	// review_assigned_prs reviews what the poll finds, and without an
	// assignee the poll finds only what carries the filter label: the
	// setting is not wrong, it just selects nothing a person did not label.
	// filter.assignee is optional, so this is a warning and not a refusal
	// to load.
	if s.cfg.Scheduler.ReviewAssignedPRs && s.cfg.Filter.Assignee == "" {
		s.log.Warn("scheduler.review_assigned_prs is on with no filter.assignee: only pull requests carrying the filter label are reviewed")
	}
	for {
		full, err := s.tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			s.opAs(s.log, slog.LevelError, "poll", err, "poll failed", "err", err)
			s.setLastErr(err.Error())
			if isRateLimited(err) {
				s.log.Warn("GitHub rate limit hit; pausing polling", "for", s.cfg.Scheduler.RateLimitBackoff.Duration)
			}
		} else if full {
			s.op("poll", nil, "")
			s.setLastErr("")
		}
		s.writeStatus()
		if full {
			// Published after writeStatus, never instead of it: the event
			// says a poll has just finished and status.json — already on
			// disk when a subscriber sees the event — says what it found.
			ev := Event{Kind: EventPoll}
			if err != nil {
				ev.Err = err.Error()
			}
			s.publish(ev)
		}
		if s.Once {
			break
		}
		if !s.waitForTick(ctx) {
			break
		}
	}
	if msg := stopNotice(s.liveCount(), s.heldIssues(), ctx.Err() != nil); msg != "" {
		s.log.Info(msg)
	}
	s.wg.Wait()
	s.writeStatus()
	return nil
}

// HardStop stops the sessions that are running right now, by cancelling the
// context they run under: each one's process group is killed exactly as a
// timeout kills it, and its directory — a transcript no result file closed,
// plus the marker written here — is what CheckInterrupted reports as an
// interrupted session, so the next `bees run` resumes the work through the
// ordinary crash-recovery path. It is the second half of stopping the
// factory: cancelling Run's context asks for the cool-down (finish what is
// running, start nothing new), and HardStop is the second interrupt — or the
// second press in the live view — for the person who will not wait it out.
// Outside Run it does nothing.
func (s *Scheduler) HardStop() {
	s.mu.Lock()
	stop := s.stopSessions
	dirs := make([]string, 0, len(s.live))
	for _, ls := range s.live {
		dirs = append(dirs, ls.dir)
	}
	s.mu.Unlock()
	if stop == nil {
		return
	}
	if len(dirs) > 0 {
		s.log.Warn("stopping " + text.Count(len(dirs), "running session") + " now")
	}
	// Marked before the kill, like KillSession: the next session for each
	// work item is told its predecessor was stopped on purpose rather than
	// left to guess that the machine crashed.
	for _, dir := range dirs {
		if err := session.MarkInterrupted(dir, "stopped with the factory: bees run was interrupted twice"); err != nil {
			s.log.Warn("could not mark the stopped session", "dir", dir, "err", err)
		}
	}
	stop()
}

// liveCount is how many sessions are running right now.
func (s *Scheduler) liveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.live)
}

// heldIssues is how many issues developer workers are carrying right now.
func (s *Scheduler) heldIssues() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.owned)
}

// stopNotice is the line Run logs on its way out, saying what the wait is
// for: the sessions running at that moment, counted, or — when the cool-down
// landed between two of a worker's stages, so there is no session to count —
// the issue that worker is still carrying. Silence there would leave the
// console saying nothing for as long as the worker takes. Empty when nothing
// is in flight, and on a tick or --once run the cool-down half is empty too:
// "again" would name an interrupt that never happened.
func stopNotice(sessions, held int, cooling bool) string {
	switch {
	case sessions > 0:
		msg := "waiting for " + text.Count(sessions, "running session") + " to finish"
		if cooling {
			msg += "; interrupt again to stop them now"
		}
		return msg
	case cooling && held > 0:
		return "waiting for the work in flight to finish; interrupt again to stop it now"
	}
	return ""
}

// sessionContext is the context a session about to start runs under: the
// session context Run derived, so the loop's cancellation cannot reach it —
// or, outside Run, the caller's own context.
func (s *Scheduler) sessionContext(ctx context.Context) context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessionCtx != nil {
		return s.sessionCtx
	}
	return ctx
}

// waitForTick sleeps until the next tick is due, running a local pass for
// every wake signalled in the meantime, and reports whether the loop should
// go on (false = the context was cancelled).
//
// A wake never becomes a full pass: it runs localPass directly, so no matter
// how many local events happen between two ticks the polling cadence is
// exactly what poll_interval and the work-hours window say. The timer is
// started once and is not restarted by a wake, for the same reason.
func (s *Scheduler) waitForTick(ctx context.Context) bool {
	timer := time.NewTimer(s.cfg.Scheduler.PollInterval.Duration)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return false
		case <-timer.C:
			return true
		case <-s.wake:
			s.localPass(ctx)
			s.writeStatus()
		}
	}
}

// signal asks the loop to run a local pass now rather than at the next tick.
// It is what makes the factory react to a purely local event — a session
// finishing (which frees a developer slot and may have written mail to
// another role) or mail the scheduler itself sent — instead of leaving the
// work to sit for up to a poll interval.
//
// Never blocks: the channel holds one slot and a signal that finds it full
// is dropped, because the pass it asks for has not run yet and will see
// everything the dropped signal would have.
//
// Mail written by another process (`bees mail send`, or the MCP server
// attached to a session) cannot signal an in-process channel. It does not
// need to: the session that sent it signals when it finishes, and the local
// pass that follows re-reads the mailbox from disk. Mail a person sends by
// hand while nothing is running still waits for the next tick.
func (s *Scheduler) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// drainWake drops a pending wake. It is called before a full pass, which
// does strictly more than the local pass the signal asked for.
func (s *Scheduler) drainWake() {
	select {
	case <-s.wake:
	default:
	}
}

// ensureLabels creates the workflow labels the repository does not have
// yet. A repository initialised by an older build is missing every label
// the factory has learned about since (`bees init` and `bees labels sync`
// create the set once), and a missing label makes every label edit that
// uses it fail. It runs once per process, before the first pass.
//
// Labels that already exist are left exactly as they are: a person who
// recoloured one keeps their choice. Only `bees labels sync` forces colour
// and description.
func (s *Scheduler) ensureLabels(ctx context.Context) error {
	have, err := s.gh.ListLabels(ctx)
	if err != nil {
		return err
	}
	// GitHub matches label names case-insensitively.
	exists := make(map[string]bool, len(have))
	for _, l := range have {
		exists[strings.ToLower(l.Name)] = true
	}
	var errs []error
	for _, spec := range s.labels.All() {
		if exists[strings.ToLower(spec.Name)] {
			continue
		}
		if err := s.gh.EnsureLabel(ctx, spec.Name, spec.Color, spec.Description); err != nil {
			errs = append(errs, err)
			continue
		}
		s.log.Info("created missing label", "label", spec.Name)
	}
	return errors.Join(errs...)
}

// maxLoggedErrs is how many of a pass's errors are named in one warning.
// One cause (a missing label, an expired token) fails the same call for
// every issue in a queue, and a joined wall of twenty identical messages is
// unreadable; the error returned to the caller still carries them all.
const maxLoggedErrs = 3

// capErrors renders err for a log message: an error joined from several
// (errors.Join) is cut to the first maxLoggedErrs, followed by the number
// of errors left out.
func capErrors(err error) string {
	if err == nil {
		return ""
	}
	joined, ok := err.(interface{ Unwrap() []error })
	if !ok {
		return err.Error()
	}
	errs := joined.Unwrap()
	if len(errs) <= maxLoggedErrs {
		return err.Error()
	}
	msgs := make([]string, 0, maxLoggedErrs+1)
	for _, e := range errs[:maxLoggedErrs] {
		msgs = append(msgs, e.Error())
	}
	msgs = append(msgs, fmt.Sprintf("+%d more", len(errs)-maxLoggedErrs))
	return strings.Join(msgs, "; ")
}

// rateLimitPhrases are the substrings that mark a message as "come back
// later": GitHub's rate-limit responses as surfaced by gh, and the API
// errors a claude session reports when it is throttled or the service is
// overloaded.
var rateLimitPhrases = []string{"rate limit", "abuse detection", "secondary rate", "overloaded", "usage limit", "session limit"}

// rateLimitedText reports whether a message names a rate limit or an
// overloaded service. Matching is case-insensitive.
func rateLimitedText(msg string) bool {
	msg = strings.ToLower(msg)
	for _, p := range rateLimitPhrases {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// isRateLimited recognises GitHub's rate-limit responses as surfaced by gh.
func isRateLimited(err error) bool { return rateLimitedText(err.Error()) }

func (s *Scheduler) describeQuery() string {
	var parts []string
	if s.query.Label != "" {
		parts = append(parts, "label="+s.query.Label)
	}
	if s.query.Assignee != "" {
		parts = append(parts, "assignee="+s.query.Assignee)
	}
	if s.query.Milestone != "" {
		parts = append(parts, "milestone="+s.query.Milestone)
	}
	return strings.Join(parts, ",")
}

// snapshot is one poll of GitHub, classified by workflow state.
type snapshot struct {
	issues   []github.Issue
	prs      []github.PR
	byState  map[string][]github.Issue
	feedback []github.Issue // bees:feedback issues, for the product manager
	features []github.Issue // bees:feature issues, owned by the product manager
	// proposals are the features a bee wrote and no person has approved
	// yet (bees:proposal). They are a subset of features, not a bucket of
	// their own: they still show in the product manager's feature table.
	proposals  []github.Issue
	prByBranch map[string]github.PR
	prByNumber map[int]github.PR
	byNumber   map[int]github.Issue
	// open holds every issue number in the snapshot: an issue that is
	// closed, or invisible to the filter, is not "open" for dependencies.
	open map[int]bool
	// waiting maps a ready issue to the blockers it declares that are still
	// open. A non-empty entry holds the issue back from dispatch.
	waiting map[int][]int
}

func (s *Scheduler) poll(ctx context.Context) (*snapshot, error) {
	issues, err := s.gh.ListOpenIssues(ctx, s.query)
	if err != nil {
		return nil, err
	}
	prs, err := s.gh.ListOpenPRs(ctx, s.query)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.lastPoll = s.now()
	s.lastIssues, s.lastPRs, s.polled = issues, prs, true
	s.mu.Unlock()
	snap := s.classify(issues, prs)
	s.setQueues(snap)
	return snap, nil
}

// classify buckets one poll's issues and PRs by workflow state. It does not
// touch GitHub and never mutates its arguments, so the lists cached from the
// last poll can be classified again on every local pass.
func (s *Scheduler) classify(issues []github.Issue, prs []github.PR) *snapshot {
	snap := &snapshot{issues: issues, prs: prs, byState: map[string][]github.Issue{},
		prByBranch: map[string]github.PR{}, prByNumber: map[int]github.PR{},
		byNumber: map[int]github.Issue{}, open: map[int]bool{}, waiting: map[int][]int{}}
	byNumber := snap.byNumber
	for _, i := range issues {
		snap.open[i.Number] = true
		byNumber[i.Number] = i
	}
	for _, i := range issues {
		// Clip the label slice so an append by a caller (reconcile) cannot
		// write into the cached issue's backing array.
		i.Labels = i.Labels[:len(i.Labels):len(i.Labels)]
		// Feedback and feature issues belong to the product manager and sit
		// outside the workflow state machine.
		if github.HasLabel(i.Labels, s.labels.Feedback) {
			snap.feedback = append(snap.feedback, i)
			continue
		}
		if github.HasLabel(i.Labels, s.labels.Feature) {
			snap.features = append(snap.features, i)
			if github.HasLabel(i.Labels, s.labels.Proposal) {
				snap.proposals = append(snap.proposals, i)
			}
			continue
		}
		snap.byState[s.stateOf(i.Labels)] = append(snap.byState[s.stateOf(i.Labels)], i)
	}
	for st := range snap.byState {
		sort.Slice(snap.byState[st], func(a, b int) bool { return snap.byState[st][a].CreatedAt.Before(snap.byState[st][b].CreatedAt) })
	}
	s.fillWaiting(snap, byNumber)
	for _, p := range prs {
		p.Labels = p.Labels[:len(p.Labels):len(p.Labels)]
		snap.prByBranch[p.HeadRefName] = p
		snap.prByNumber[p.Number] = p
	}
	return snap
}

// issueForPR returns the visible issue a factory PR closes, or false when
// the PR closes no issue the factory can see (it is not driving that PR).
func (snap *snapshot) issueForPR(pr github.PR) (github.Issue, bool) {
	for _, n := range pr.ClosingIssues() {
		if i, ok := snap.byNumber[n]; ok {
			return i, true
		}
	}
	return github.Issue{}, false
}

// hasOpenPR reports whether the snapshot holds an open pull request on the
// issue's branch: the issue was worked on before and is being resumed, not
// started.
func (s *Scheduler) hasOpenPR(snap *snapshot, issue github.Issue) bool {
	_, ok := snap.prByBranch[s.BranchFor(issue.Number)]
	return ok
}

// queueNoState is the name `bees status` gives the bucket of visible issues
// that carry no workflow state label yet. Internally they are keyed by the
// empty string; an anonymous row in the queues block reads like a rendering
// glitch rather than "these are waiting for the next reconcile".
const queueNoState = "no_state"

// setQueues records the queue sizes of a snapshot for `bees status`. Empty
// state buckets are left out, so a queue only shows up while it has issues.
func (s *Scheduler) setQueues(snap *snapshot) {
	// Both read the state directory, one file per issue, so they are built
	// before the lock rather than under it.
	needsHuman, approved := s.escalatedIssues(snap), s.approvedPRs(snap)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.needsHuman, s.approved = needsHuman, approved
	s.waiting = snap.waiting
	s.queues = map[string]int{}
	for st, list := range snap.byState {
		if len(list) == 0 {
			continue
		}
		if st == "" {
			st = queueNoState
		}
		s.queues[st] = len(list)
	}
	s.queues["feedback"] = len(snap.feedback)
	s.queues["features"] = len(snap.features)
	// Always recorded, even at zero: a proposal is a decision owed by a
	// person, and a row that only appears once there is one is a row nobody
	// learns to look for.
	s.queues["proposals"] = len(snap.proposals)
	s.queues["open_prs"] = len(snap.prs)
	s.readySizes = map[string]int{}
	s.priority = nil
	for _, i := range snap.byState["ready"] {
		s.readySizes[s.sizeOf(i.Labels)]++
		if s.hasPriority(i.Labels) {
			s.priority = append(s.priority, i.Number)
		}
	}
	sort.Ints(s.priority)
}

// escalatedIssues describes the issues the factory has given up on, first
// escalated first: which issue, what it is called and why it was escalated.
// The reason is read from the issue's own bookkeeping, where escalate
// recorded it — never from GitHub, so listing them costs the polling path
// nothing. An issue a person labelled by hand, and one escalated before this
// state directory existed, has no reason and no time; they come last, in
// issue order.
func (s *Scheduler) escalatedIssues(snap *snapshot) []state.Escalated {
	var out []state.Escalated
	for _, i := range snap.byState["needs-human"] {
		e := state.Escalated{Issue: i.Number, Title: i.Title}
		if bk, err := s.store.Issue(i.Number); err == nil {
			e.Reason, e.Since = bk.Escalation, bk.EscalatedAt
		}
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool {
		x, y := out[a], out[b]
		if x.Since.IsZero() != y.Since.IsZero() {
			return y.Since.IsZero()
		}
		if !x.Since.Equal(y.Since) {
			return x.Since.Before(y.Since)
		}
		return x.Issue < y.Issue
	})
	return out
}

// approvedPRs describes the pull requests the reviewer approved and left for
// a person to merge, oldest first. An approved issue whose pull request the
// poll no longer finds open is left out: it has already been merged or
// closed and the label has yet to catch up, so it is not something a person
// is being asked to merge.
func (s *Scheduler) approvedPRs(snap *snapshot) []state.ApprovedPR {
	var out []state.ApprovedPR
	for _, i := range snap.byState["approved"] {
		pr, ok := snap.prByBranch[s.BranchFor(i.Number)]
		if !ok {
			continue
		}
		out = append(out, state.ApprovedPR{PR: pr.Number, Issue: i.Number, Title: pr.Title, Since: pr.CreatedAt})
	}
	sort.Slice(out, func(a, b int) bool {
		if !out[a].Since.Equal(out[b].Since) {
			return out[a].Since.Before(out[b].Since)
		}
		return out[a].PR < out[b].PR
	})
	return out
}

// stateOf returns the workflow state name ("triage", "ready", ...) of an
// issue, or "" when it has no state label.
func (s *Scheduler) stateOf(labels []github.Label) string {
	for _, l := range s.labels.StateLabels() {
		if github.HasLabel(labels, l) {
			return strings.TrimPrefix(l, s.labels.Base+":")
		}
	}
	return ""
}

// sizeOf returns the size of an issue ("xs", "s", "m", "l", "xl"), or ""
// when it carries no size label. Sizes are orthogonal to the workflow
// state: an issue has at most one of each.
func (s *Scheduler) sizeOf(labels []github.Label) string {
	for _, l := range s.labels.SizeLabels() {
		if github.HasLabel(labels, l) {
			return strings.TrimPrefix(l, s.labels.Base+":size/")
		}
	}
	return ""
}

// hasPriority reports whether the issue carries bees:priority. The label is
// not a state or size label: people set it, nothing in the factory removes
// it, the project manager is the one role that may add it (to a work item
// that unblocks the factory itself), and dispatch is the only thing that
// reads it.
func (s *Scheduler) hasPriority(labels []github.Label) bool {
	return github.HasLabel(labels, s.labels.Priority)
}

// defaultSize is the size reconcile gives a ready issue that carries none;
// an unsized issue therefore ranks as "m" everywhere.
const defaultSize = "m"

// sizeLarge is the size the scheduler.max_large_in_flight cap counts.
const sizeLarge = "l"

// sizeRank orders the work item sizes, smallest first. An unknown or missing
// size ranks as defaultSize, which is the label reconcile gives it anyway.
func sizeRank(size string) int {
	if i := slices.Index(config.Sizes, size); i >= 0 {
		return i
	}
	return slices.Index(config.Sizes, defaultSize)
}

// observeProposals watches bees:proposal on the feature issues and records
// the moment a person removes it. Approval is a label edit and leaves no
// comment, so github.Issue.AwaitingBee never flips and the feature would
// otherwise never come back to the product manager: this timestamp is the
// only thing that brings it back (runProductManager, productManagerHasWork).
//
// It only ever observes the label. Adding or removing bees:proposal is a
// person's decision, or `bees issue create --feature`'s; the scheduler never
// touches it. A feature first seen without the label is an ordinary feature,
// so a restart that lost the state dir approves nothing.
func (s *Scheduler) observeProposals(snap *snapshot) []error {
	var errs []error
	for _, i := range snap.features {
		proposal := github.HasLabel(i.Labels, s.labels.Proposal)
		is, err := s.store.Issue(i.Number)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if is.Proposal == proposal {
			continue
		}
		var approvedAt time.Time
		if is.Proposal {
			s.log.Info("person approved a proposal", "issue", i.Number)
			approvedAt = s.now()
		}
		if err := s.store.SetProposal(i.Number, proposal, approvedAt); err != nil {
			errs = append(errs, err)
		}
	}
	return errs
}

// sortReady orders the ready queue in place: issues carrying bees:priority
// first, then whatever scheduler.dispatch_order asks for
// (smallest or largest size first; "oldest" leaves the sizes alone), ties
// broken by age (oldest first), which is the order poll produced. Priority is
// a separate axis from size, so a small issue never jumps a priority one.
// sizeOf and hasPriority read an issue's labels, so the helper is independent
// of a scheduler.
func sortReady(issues []github.Issue, order string, sizeOf func([]github.Label) string, hasPriority func([]github.Label) bool) {
	bySize := order == config.DispatchSmallFirst || order == config.DispatchLargeFirst
	sort.SliceStable(issues, func(a, b int) bool {
		if pa, pb := hasPriority(issues[a].Labels), hasPriority(issues[b].Labels); pa != pb {
			return pa
		}
		if bySize {
			ra, rb := sizeRank(sizeOf(issues[a].Labels)), sizeRank(sizeOf(issues[b].Labels))
			if ra != rb {
				if order == config.DispatchLargeFirst {
					return ra > rb
				}
				return ra < rb
			}
		}
		return issues[a].CreatedAt.Before(issues[b].CreatedAt)
	})
}

// tick runs one iteration of the loop: a full pass when a GitHub poll is due,
// a local pass otherwise. It reports whether the pass was a full one and the
// error of a failed poll.
func (s *Scheduler) tick(ctx context.Context) (bool, error) {
	now := s.now()
	s.mu.Lock()
	due := s.nextPoll.IsZero() || !now.Before(s.nextPoll)
	s.mu.Unlock()
	if !due {
		s.localPass(ctx)
		return false, nil
	}
	// A wake that is still pending asks for a local pass; the full pass
	// about to run supersedes it.
	s.drainWake()
	err := s.pass(ctx)
	wait := s.cfg.Scheduler.PollIntervalAt(now)
	if next := s.cfg.Scheduler.NextWorkHoursStart(now); !next.IsZero() && next.Before(now.Add(wait)) {
		// The window opens before the off-hours interval would elapse: poll
		// then, so the work day starts on time instead of up to an interval
		// late.
		wait = next.Sub(now)
	}
	if err != nil && isRateLimited(err) && s.cfg.Scheduler.RateLimitBackoff.Duration > wait {
		// A rate limit is a floor, never a speed-up: it wins over the
		// interval in force only when it is the longer of the two.
		wait = s.cfg.Scheduler.RateLimitBackoff.Duration
	}
	s.mu.Lock()
	s.nextPoll = now.Add(wait)
	s.mu.Unlock()
	return true, err
}

// pass polls GitHub and runs the whole reconcile/dispatch cycle.
func (s *Scheduler) pass(ctx context.Context) error {
	snap, err := s.poll(ctx)
	if err != nil {
		return err
	}
	// Issue comments are delivered before PR feedback: an approved issue
	// that gets both goes back to ready in deliverHumanFeedback, out of
	// every in-flight bucket, and the loop below reads those buckets.
	err = s.deliverHumanIssueComments(ctx, snap)
	s.op("human-issue-comments", err, "human issue comments", "err", err)
	err = s.deliverHumanFeedback(ctx, snap)
	s.op("human-feedback", err, "human feedback", "err", err)
	err = s.checkPRs(ctx, snap)
	s.op("check-prs", err, "check PRs", "err", err)
	err = s.reconcile(ctx, snap)
	s.op("reconcile", err, "reconcile", "err", capErrors(err))
	s.checkDayBudget()
	s.dispatchDevelopers(ctx, snap, false)
	// After the developers, so a ready issue is never starved by a review
	// request, and only here: a local pass would dispatch from a cached
	// pull request list that still carries a label removed on GitHub.
	s.dispatchRequestedReviews(ctx, snap)
	s.dispatchSingletons(ctx, snap, false)
	return nil
}

// localPass is a pass that makes no GitHub read calls of its own: it reuses
// the issue and PR lists from the last poll, so everything driven by the
// local mailbox (answered questions, review rounds) keeps moving when a
// session finishes (signal) and at poll_interval even when GitHub is only
// polled every off_hours_poll_interval. It deliberately skips the human-feedback fetch,
// and with it the interval and merged-PR has-work checks, all of which query
// GitHub: on a local pass a singleton starts only when it has unread mail.
// Until the first successful poll it does nothing.
func (s *Scheduler) localPass(ctx context.Context) {
	s.mu.Lock()
	issues, prs, ok := s.lastIssues, s.lastPRs, s.polled
	s.mu.Unlock()
	if !ok {
		return
	}
	snap := s.classify(issues, prs)
	s.setQueues(snap)
	err := s.reconcile(ctx, snap)
	s.op("reconcile", err, "reconcile", "err", capErrors(err))
	s.checkDayBudget()
	s.dispatchDevelopers(ctx, snap, true)
	s.dispatchSingletons(ctx, snap, true)
}

// reconcile applies label transitions that depend on local state:
//
//   - a visible issue with no state label and neither bees:feature nor
//     bees:feedback is a person handing the factory an idea, not a spec: it
//     gets bees:feedback and goes to the product manager, who decides what it
//     becomes (and it receives the factory label if the filter does not
//     already require it). A person who wants something built without that
//     hop labels the issue bees:triage or bees:ready themselves;
//   - blocked issues whose question has been answered move back to the
//     stage that asked (developer -> ready, project manager -> triage).
//     Mail from a human about the issue counts as an answer too;
//   - ready issues without a size get the default one, and ready issues
//     sized above roles.developer.max_size go back to triage to be split.
//     The sizing runs last so that an issue unblocked by the loop above is
//     sized in the same pass instead of the next one;
//   - a feature that has stopped being a proposal is remembered as approved.
//
// Every label edit is also written back to the cached poll (cacheIssue), or
// the local passes in between two polls would classify the issue from its
// stale labels and repeat the edit on every one of them.
func (s *Scheduler) reconcile(ctx context.Context, snap *snapshot) error {
	var errs []error
	var unrouted []github.Issue
	for _, i := range snap.byState[""] {
		add := []string{s.labels.Feedback}
		if !github.HasLabel(i.Labels, s.labels.Base) {
			add = append(add, s.labels.Base)
		}
		s.log.Info("new issue goes to the product manager", "issue", i.Number, "title", i.Title)
		if err := s.gh.EditLabels(ctx, i.Number, add, nil); err != nil {
			errs = append(errs, err)
			unrouted = append(unrouted, i)
			continue
		}
		for _, l := range add {
			i.Labels = append(i.Labels, github.Label{Name: l})
		}
		// bees:feedback is a kind, not a state: the issue leaves the state
		// machine altogether. Putting it in a byState bucket would hand it
		// to the project manager, which is exactly the hop this avoids.
		snap.feedback = append(snap.feedback, i)
		// Keep the cached poll in step, or every local pass in between two
		// polls asks GitHub to add the label again.
		s.cacheIssue(i)
	}
	snap.byState[""] = unrouted
	var stillBlocked []github.Issue
	for _, i := range snap.byState["blocked"] {
		if s.hasUnreadMail(config.RoleDeveloper, i.Number, 0) {
			s.log.Info("question answered, issue back to ready", "issue", i.Number)
			if err := s.setState(ctx, i.Number, s.labels.Ready); err != nil {
				errs = append(errs, err)
				stillBlocked = append(stillBlocked, i)
				continue
			}
			i.Labels = relabel(i.Labels, s.labels.Blocked, s.labels.Ready)
			snap.byState["ready"] = append(snap.byState["ready"], i)
			s.cacheIssue(i)
		} else if s.hasUnreadMail(config.RoleProjectManager, i.Number, 0) {
			s.log.Info("question answered, issue back to triage", "issue", i.Number)
			if err := s.setState(ctx, i.Number, s.labels.Triage); err != nil {
				errs = append(errs, err)
				stillBlocked = append(stillBlocked, i)
				continue
			}
			i.Labels = relabel(i.Labels, s.labels.Blocked, s.labels.Triage)
			snap.byState["triage"] = append(snap.byState["triage"], i)
			s.cacheIssue(i)
		} else {
			stillBlocked = append(stillBlocked, i)
		}
	}
	snap.byState["blocked"] = stillBlocked
	// A work item in ready without a size — typically one a human
	// fast-tracked past triage — gets the default size, so the developer
	// and reviewer prompts and `bees status` always have one.
	for idx, i := range snap.byState["ready"] {
		if s.sizeOf(i.Labels) != "" {
			continue
		}
		s.log.Info("ready issue without a size gets the default", "issue", i.Number, "size", "m")
		if err := s.gh.EditLabels(ctx, i.Number, []string{s.labels.SizeM}, nil); err != nil {
			errs = append(errs, err)
			continue
		}
		sized := append(i.Labels, github.Label{Name: s.labels.SizeM})
		snap.byState["ready"][idx].Labels = sized
		// Keep the cached poll in step, or every local pass in between two
		// polls asks GitHub to add the label again.
		i.Labels = sized
		s.cacheIssue(i)
	}
	// A ready issue bigger than roles.developer.max_size is not something a
	// developer can land in one pull request, so it never gets dispatched: it
	// goes back to triage and the project manager splits it on its next run.
	// The label move is the whole signal; comments on GitHub are for people.
	limit := sizeRank(s.cfg.MaxSize())
	ready := snap.byState["ready"][:0:0]
	for _, i := range snap.byState["ready"] {
		size := s.sizeOf(i.Labels)
		if sizeRank(size) <= limit {
			ready = append(ready, i)
			continue
		}
		s.log.Info("ready issue is too big for a developer, back to triage",
			"issue", i.Number, "size", size, "max_size", s.cfg.MaxSize())
		if err := s.gh.EditLabels(ctx, i.Number, []string{s.labels.Triage}, []string{s.labels.Ready}); err != nil {
			errs = append(errs, err)
			ready = append(ready, i)
			continue
		}
		i.Labels = relabel(i.Labels, s.labels.Ready, s.labels.Triage)
		snap.byState["triage"] = append(snap.byState["triage"], i)
		// Keep the cached poll in step, or every local pass in between two
		// polls asks GitHub to move the label again.
		s.cacheIssue(i)
	}
	snap.byState["ready"] = ready
	errs = append(errs, s.observeProposals(snap)...)
	// The pass moved issues between buckets; recount so `bees status` shows
	// what GitHub now shows instead of the poll's stale counts.
	s.setQueues(snap)
	return errors.Join(errs...)
}

// relabel replaces one label with another in a local copy of an issue's
// labels, so a pass that has just edited them on GitHub reads what GitHub now
// shows. from is a full label name ("bees:ready"), not the short state
// stateOf returns: given a short name nothing is removed and the copy carries
// both labels.
func relabel(labels []github.Label, from, to string) []github.Label {
	out := make([]github.Label, 0, len(labels)+1)
	for _, l := range labels {
		if l.Name != from {
			out = append(out, l)
		}
	}
	return append(out, github.Label{Name: to})
}

// setStateLocal rewrites a local copy of an issue's labels the way
// github.SetState has just rewritten GitHub's: every state label goes and
// to is added. relabel takes a single name, which is only enough while an
// issue carries one state label. A person holding an issue adds
// bees:needs-human on top of the state label underneath, so removing only
// the one stateOf names leaves the other behind, and stateOf reads that one
// back out of the copy.
func setStateLocal(labels []github.Label, states []string, to string) []github.Label {
	out := make([]github.Label, 0, len(labels)+1)
	for _, l := range labels {
		if !slices.Contains(states, l.Name) {
			out = append(out, l)
		}
	}
	return append(out, github.Label{Name: to})
}

// dispatchDevelopers hands issues to free developer workers. Issues that
// are already in progress or in review but not owned by a worker (for
// example after a restart) are resumed first and are never reordered: a
// worker picking its issue back up after a restart must not be starved. An
// approved issue joins them only when it is a resumption too — a worker died
// waiting out the post-approval checks, or in the developer round they sent
// back (resumableChecks); an approved issue nothing was working on is waiting
// for a person to merge it, not for a developer.
// Ready issues that already have an open pull request come next, oldest
// first — a PR that needs attention (human feedback, a conflict with the
// default branch) is finished before new work is started. The rest of the
// ready queue is ordered by scheduler.dispatch_order, and a bees:size/l
// issue waits while scheduler.max_large_in_flight of them are already being
// worked on. A ready issue whose declared blockers are still open is skipped
// without consuming a pool slot; resumptions are never held back.
//
// On a local pass (local) the snapshot comes from the last poll and can be
// stale: an issue a worker has since finished, a developer parked in
// bees:blocked or a human closed still carries its old state label. Before
// spending a session on such an issue the live issue is fetched once and the
// candidate dropped unless it is still open and in a dispatchable state.
func (s *Scheduler) dispatchDevelopers(ctx context.Context, snap *snapshot, local bool) {
	// ctx.Err() is the stop key: sessions run under their own context, so a
	// pass that is still finishing when the loop's context is cancelled
	// would otherwise start work the cool-down promised not to.
	if ctx.Err() != nil || !s.roleEnabled(config.RoleDeveloper) || s.limitPaused() || s.dayBudgetReached() {
		return
	}
	var candidates []github.Issue
	candidates = append(candidates, snap.byState["in-progress"]...)
	candidates = append(candidates, snap.byState["review"]...)
	for _, i := range snap.byState["approved"] {
		if s.resumableChecks(snap, i) {
			candidates = append(candidates, i)
		}
	}
	var ready []github.Issue
	// resumed marks the ready issues that are a pull request coming back
	// for more work rather than something new.
	resumed := map[int]bool{}
	for _, i := range snap.byState["ready"] {
		if blockers := snap.waiting[i.Number]; len(blockers) > 0 {
			s.log.Info("issue waiting on dependencies", "issue", i.Number, "blocked_by", blockers)
			continue
		}
		if s.hasOpenPR(snap, i) {
			resumed[i.Number] = true
			candidates = append(candidates, i)
			continue
		}
		ready = append(ready, i)
	}
	sortReady(ready, s.cfg.Scheduler.DispatchOrder, s.sizeOf, s.hasPriority)
	candidates = append(candidates, ready...)
	largeLimit := s.cfg.Scheduler.LargeInFlight()
	for _, issue := range candidates {
		s.mu.Lock()
		_, taken := s.owned[issue.Number]
		s.mu.Unlock()
		if taken {
			continue
		}
		if until, ok := s.backoffUntil(fmt.Sprintf("issue-%d", issue.Number)); ok && s.now().Before(until) {
			continue
		}
		size := s.sizeOf(issue.Labels)
		// The cap only holds back fresh work: a resumed in-progress,
		// review or post-approval-checks issue, or a ready one with an
		// open PR, is already in flight. Checked before a slot is taken,
		// so a held issue does not keep a free developer idle.
		if largeLimit > 0 && size == sizeLarge && s.stateOf(issue.Labels) == "ready" && !resumed[issue.Number] && s.largeInFlight() >= largeLimit {
			s.log.Info("large issue waits, cap reached", "issue", issue.Number, "max_large_in_flight", largeLimit)
			continue
		}
		select {
		case <-s.slots:
		default:
			return // pool is full
		}
		if local {
			live, ok := s.liveCandidate(ctx, issue, snap)
			if !ok {
				s.slots <- struct{}{}
				continue
			}
			issue = live
			size = s.sizeOf(issue.Labels)
		}
		w := &state.Worker{Name: fmt.Sprintf("dev-%d", issue.Number), Issue: issue.Number, Size: size, Stage: "starting", Since: s.now()}
		s.mu.Lock()
		s.owned[issue.Number] = w
		s.mu.Unlock()
		s.writeStatus()
		s.wg.Add(1)
		go func(issue github.Issue, w *state.Worker) {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				delete(s.owned, issue.Number)
				s.mu.Unlock()
				s.slots <- struct{}{}
				s.writeStatus()
				// The session that finished last signalled while this
				// worker still held the slot. Signal again now that it is
				// back in the pool, or the next ready issue would wait for
				// the tick after all.
				s.signal()
			}()
			// The worker runs under the sessions' context and keeps going
			// through a cool-down (workIssue), so a failure during one is a
			// real failure and earns its log line and its backoff. Only the
			// hard stop that cancels that context makes them noise.
			if err := s.workIssue(ctx, issue, w); err != nil && s.sessionContext(ctx).Err() == nil {
				s.log.Error("developer worker failed", "issue", issue.Number, "err", err)
				s.setBackoff(fmt.Sprintf("issue-%d", issue.Number), 5*s.cfg.Scheduler.PollInterval.Duration)
			}
		}(issue, w)
	}
}

// largeInFlight counts the bees:size/l issues developer workers own, which
// is what scheduler.max_large_in_flight caps.
func (s *Scheduler) largeInFlight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, w := range s.owned {
		if w.Size == sizeLarge {
			n++
		}
	}
	return n
}

// liveCandidate re-reads an issue picked from a stale (cached) snapshot and
// reports whether a developer worker should still be started for it. It also
// refreshes the cached copy, so an issue that has moved on is not fetched
// again by the next local pass. bees:approved is dispatchable only under the
// gate the candidate list itself applies (resumableChecks): every other
// approved issue is waiting for a person, not for a developer.
func (s *Scheduler) liveCandidate(ctx context.Context, issue github.Issue, snap *snapshot) (github.Issue, bool) {
	live, err := s.gh.GetIssue(ctx, issue.Number)
	if s.op("issue-get", err, "live issue check failed, skipping", "issue", issue.Number, "err", err) {
		return issue, false
	}
	s.cacheIssue(live)
	if live.State != "" && !strings.EqualFold(live.State, "open") {
		return live, false
	}
	switch s.stateOf(live.Labels) {
	case "ready", "in-progress", "review":
		return live, true
	case "approved":
		return live, s.resumableChecks(snap, live)
	default:
		return live, false
	}
}

// resumableChecks reports whether an approved issue is a developer worker's
// unfinished business rather than a pull request waiting for a person to
// merge it. approve() labels the issue bees:approved before the worker enters
// the checks stage, so a scheduler killed while waiting out
// roles.reviewer.checks_timeout leaves work in flight behind a label that
// nothing else dispatches. The stage the worker recorded is what tells the
// two apart, and it is read from the state directory: no GitHub call joins
// the polling path for it.
//
// The develop round that gate sends back (postApprovalFixRound) counts too.
// The stage is recorded before the develop stage relabels the issue
// bees:in-progress, so a worker killed — or a single failing `gh issue edit` —
// in between leaves that record behind the same bees:approved label: the same
// work in flight, one stage on.
func (s *Scheduler) resumableChecks(snap *snapshot, issue github.Issue) bool {
	if !s.hasOpenPR(snap, issue) {
		return false
	}
	bk, err := s.store.Issue(issue.Number)
	if err != nil {
		return false
	}
	return bk.WorkerStage == "checks" || postApprovalFixRound(bk)
}

// cacheIssue writes an issue into the lists kept from the last poll: the
// copy the poll took is replaced, and an issue the poll never saw is
// appended. Appending is what puts an issue a session created — in another
// process, through the MCP server (refreshTouched) — in front of the local
// pass that follows, instead of leaving it for the next poll to discover.
func (s *Scheduler) cacheIssue(live github.Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The list is replaced rather than written into. A pass takes its header
	// under this lock and then classifies it without holding the lock, and
	// refreshTouched runs on the goroutine of the session that ended, not on
	// the loop's: writing an element in place would be a write to a slice
	// another goroutine is reading.
	next := make([]github.Issue, len(s.lastIssues), len(s.lastIssues)+1)
	copy(next, s.lastIssues)
	for i := range next {
		if next[i].Number == live.Number {
			next[i] = live
			s.lastIssues = next
			return
		}
	}
	s.lastIssues = append(next, live)
}

// dispatchSingletons starts the product manager, project manager and QA
// when they have work and are not already running. With mailOnly (a local
// pass) a role only starts when it has unread mail: the other has-work checks
// query GitHub.
func (s *Scheduler) dispatchSingletons(ctx context.Context, snap *snapshot, mailOnly bool) {
	type job struct {
		role string
		want func() bool
		run  func(context.Context, *snapshot) error
	}
	jobs := []job{
		{config.RoleProjectManager, func() bool { return s.projectManagerHasWork(snap) }, s.runProjectManager},
		{config.RoleProductManager, func() bool { return s.productManagerHasWork(ctx, snap) }, s.runProductManager},
		{config.RoleQA, func() bool { return s.qaHasWork(ctx) }, s.runQA},
	}
	if mailOnly {
		for i := range jobs {
			role := jobs[i].role
			jobs[i].want = func() bool { return s.hasUnreadMail(role, 0, 0) }
		}
	}
	// The same three gates as dispatchDevelopers, ctx.Err() included: a
	// cancelled loop context means the factory is stopping and no singleton
	// may start.
	if ctx.Err() != nil || s.limitPaused() || s.dayBudgetReached() {
		return
	}
	for _, j := range jobs {
		if !s.roleEnabled(j.role) {
			continue
		}
		s.mu.Lock()
		busy := s.running[j.role]
		s.mu.Unlock()
		if busy {
			continue
		}
		if until, ok := s.backoffUntil(j.role); ok && s.now().Before(until) {
			continue
		}
		if !j.want() {
			continue
		}
		s.mu.Lock()
		s.running[j.role] = true
		s.mu.Unlock()
		s.writeStatus()
		s.wg.Add(1)
		go func(j job) {
			defer s.wg.Done()
			defer func() {
				s.mu.Lock()
				s.running[j.role] = false
				s.mu.Unlock()
				s.writeStatus()
			}()
			err := j.run(ctx, snap)
			if err != nil && ctx.Err() == nil {
				s.log.Error("singleton role failed", "role", j.role, "err", err)
				s.setBackoff(j.role, 5*s.cfg.Scheduler.PollInterval.Duration)
			} else {
				// Never re-run a singleton faster than the poll interval.
				s.setBackoff(j.role, s.cfg.Scheduler.PollInterval.Duration)
			}
		}(j)
	}
}

func (s *Scheduler) roleEnabled(role string) bool {
	if s.OnlyRoles != nil && !s.OnlyRoles[role] {
		return false
	}
	r, err := s.cfg.Role(role)
	return err == nil && r.Enabled
}

func (s *Scheduler) backoffUntil(key string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.backoff[key]
	return t, ok
}

func (s *Scheduler) setBackoff(key string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.backoff[key] = s.now().Add(d)
}

func (s *Scheduler) setLastErr(msg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastErr = msg
}

// setState moves an issue to one workflow state label.
func (s *Scheduler) setState(ctx context.Context, number int, to string) error {
	issue, err := s.gh.GetIssue(ctx, number)
	if err != nil {
		return err
	}
	return s.gh.SetState(ctx, number, issue.Labels, to, s.labels.StateLabels())
}

// escalationNoteLimit is how much of the reason the escalation summary line
// shows; the record's note attribute and the GitHub comment keep it in full.
const escalationNoteLimit = 200

// escalate marks an issue needs-human and leaves a comment for people.
// This is the only place the factory writes a GitHub comment.
func (s *Scheduler) escalate(ctx context.Context, number int, reason string) error {
	s.log.Warn(fmt.Sprintf("⚠ issue #%d escalated to a human: %s", number, oneLine(reason, escalationNoteLimit)),
		logging.SummaryKey, true, "issue", number, "outcome", "escalated", "note", reason)
	if err := s.setState(ctx, number, s.labels.NeedsHuman); err != nil {
		return err
	}
	// The reason reaches a person in the comment below and the log line
	// above, and neither is readable from the poll. Record it where the
	// factory's own views can read it back — a person looking at what the
	// factory is stuck on wants the reason next to the issue, not a search
	// through the log. After the label, never before: a reason recorded for
	// an escalation that then failed to happen would be presented as the
	// factory's the next time anybody labelled that issue by hand. Best
	// effort — the escalation must not fail over its own bookkeeping.
	if err := s.store.SetEscalation(number, oneLine(reason, escalationNoteLimit), s.now()); err != nil {
		s.log.Warn("could not record the escalation reason", "issue", number, "err", err)
	}
	body := fmt.Sprintf("🐝 **busybees needs a human.**\n\n%s\n\nRemove the `%s` label and add `%s` (or `%s`) to hand it back to the factory.",
		reason, s.labels.NeedsHuman, s.labels.Ready, s.labels.Triage)
	// By default the factory and the people it works for share one GitHub
	// account, so a comment notifies nobody by itself: mention
	// scheduler.notify. ([github] gives the factory an account of its own,
	// but the mention is what makes the escalation reach somebody either way.)
	if m := s.cfg.Mentions(); m != "" {
		body = m + "\n\n" + body
	}
	return s.gh.Comment(ctx, number, body)
}

func (s *Scheduler) writeStatus() {
	s.mu.Lock()
	st := state.Status{LastPoll: s.lastPoll, NextPoll: s.nextPoll, Version: s.version, Revision: s.revision,
		Singletons: map[string]string{}, Queues: map[string]int{}, LastError: s.lastErr}
	if s.cfg.Scheduler.WorkHoursEnabled() {
		in := s.cfg.Scheduler.InWorkHours(s.now())
		st.InWorkHours = &in
	}
	for _, w := range s.owned {
		st.Workers = append(st.Workers, *w)
	}
	sort.Slice(st.Workers, func(i, j int) bool { return st.Workers[i].Issue < st.Workers[j].Issue })
	for _, r := range []string{config.RoleProductManager, config.RoleProjectManager, config.RoleQA} {
		if s.running[r] {
			st.Singletons[r] = "running"
		} else {
			st.Singletons[r] = "idle"
		}
	}
	for k, v := range s.queues {
		st.Queues[k] = v
	}
	if len(s.waiting) > 0 {
		st.WaitingOnDeps = make(map[int][]int, len(s.waiting))
		for k, v := range s.waiting {
			st.WaitingOnDeps[k] = append([]int(nil), v...)
		}
	}
	st.ReadySizes = map[string]int{}
	for k, v := range s.readySizes {
		st.ReadySizes[k] = v
	}
	st.Priority = append([]int(nil), s.priority...)
	st.NeedsHuman = append([]state.Escalated(nil), s.needsHuman...)
	st.Approved = append([]state.ApprovedPR(nil), s.approved...)
	st.Degraded = s.degradedLocked()
	s.budgetStatus(&st)
	s.limitStatus(&st)
	s.mu.Unlock()
	err := s.store.SaveStatus(st)
	s.op("write-status", err, "write status", "err", err)
}

func (s *Scheduler) updateWorker(w *state.Worker, stage string, round int) {
	s.mu.Lock()
	w.Stage = stage
	w.Round = round
	w.Attempt = 1
	issue := w.Issue
	s.mu.Unlock()
	s.writeStatus()
	s.publish(Event{Kind: EventStage, Role: config.RoleDeveloper, Issue: issue, Stage: stage, Round: round})
}

// setWorkerAttempt records which attempt of the current stage is running so
// `bees status` shows a retry.
func (s *Scheduler) setWorkerAttempt(w *state.Worker, attempt int) {
	if w == nil {
		return
	}
	s.mu.Lock()
	w.Attempt = attempt
	s.mu.Unlock()
	s.writeStatus()
}
