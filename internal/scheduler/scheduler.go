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
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
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
	queues   map[string]int
	wg       sync.WaitGroup
	slots    chan struct{}
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
		cfg:     d.Config,
		labels:  d.Config.Labels(),
		query:   q,
		gh:      d.GitHub,
		mail:    d.Mail,
		runner:  d.Runner,
		ws:      d.Workspaces,
		store:   d.Store,
		log:     d.Logger,
		now:     d.Now,
		owned:   map[int]*state.Worker{},
		running: map[string]bool{},
		backoff: map[string]time.Time{},
		queues:  map[string]int{},
		slots:   make(chan struct{}, d.Config.Scheduler.MaxDevelopers),
	}
	for i := 0; i < d.Config.Scheduler.MaxDevelopers; i++ {
		s.slots <- struct{}{}
	}
	return s, nil
}

// Run executes the loop until ctx is cancelled (or, with Once, until one
// pass and the work it started have completed).
func (s *Scheduler) Run(ctx context.Context) error {
	if err := s.store.Init(); err != nil {
		return err
	}
	if err := s.ws.Prune(ctx); err != nil {
		s.log.Warn("worktree prune failed", "err", err)
	}
	s.log.Info("scheduler started", "repo", s.cfg.Project.Repo, "filter", s.describeQuery(),
		"max_developers", s.cfg.Scheduler.MaxDevelopers, "poll", s.cfg.Scheduler.PollInterval.Duration,
		"work_hours", s.cfg.Scheduler.WorkHours)
	for {
		full, err := s.tick(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			s.log.Error("poll failed", "err", err)
			s.setLastErr(err.Error())
			if isRateLimited(err) {
				s.log.Warn("GitHub rate limit hit; pausing polling", "for", s.cfg.Scheduler.RateLimitBackoff.Duration)
			}
		} else if full {
			s.setLastErr("")
		}
		s.writeStatus()
		if s.Once {
			break
		}
		select {
		case <-ctx.Done():
			goto drain
		case <-time.After(s.cfg.Scheduler.PollInterval.Duration):
		}
	}
drain:
	s.log.Info("waiting for running sessions to finish")
	s.wg.Wait()
	s.writeStatus()
	return nil
}

// rateLimitPhrases are the substrings that mark a message as "come back
// later": GitHub's rate-limit responses as surfaced by gh, and the API
// errors a claude session reports when it is throttled or the service is
// overloaded.
var rateLimitPhrases = []string{"rate limit", "abuse detection", "secondary rate", "overloaded", "usage limit"}

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
	issues     []github.Issue
	prs        []github.PR
	byState    map[string][]github.Issue
	feedback   []github.Issue // bees:feedback issues, for the product manager
	features   []github.Issue // bees:feature issues, owned by the product manager
	prByBranch map[string]github.PR
	prByNumber map[int]github.PR
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
	snap := &snapshot{issues: issues, prs: prs, byState: map[string][]github.Issue{}, prByBranch: map[string]github.PR{}, prByNumber: map[int]github.PR{}}
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
			continue
		}
		snap.byState[s.stateOf(i.Labels)] = append(snap.byState[s.stateOf(i.Labels)], i)
	}
	for st := range snap.byState {
		sort.Slice(snap.byState[st], func(a, b int) bool { return snap.byState[st][a].CreatedAt.Before(snap.byState[st][b].CreatedAt) })
	}
	for _, p := range prs {
		p.Labels = p.Labels[:len(p.Labels):len(p.Labels)]
		snap.prByBranch[p.HeadRefName] = p
		snap.prByNumber[p.Number] = p
	}
	return snap
}

// setQueues records the queue sizes of a snapshot for `bees status`.
func (s *Scheduler) setQueues(snap *snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.queues = map[string]int{}
	for st, list := range snap.byState {
		s.queues[st] = len(list)
	}
	s.queues["feedback"] = len(snap.feedback)
	s.queues["features"] = len(snap.features)
	s.queues["open_prs"] = len(snap.prs)
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
	err := s.pass(ctx)
	wait := s.cfg.Scheduler.PollIntervalAt(now)
	if err != nil && isRateLimited(err) {
		// A rate limit wins over the work-hours window.
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
	if err := s.deliverHumanFeedback(ctx, snap); err != nil {
		s.log.Warn("human feedback", "err", err)
	}
	if err := s.reconcile(ctx, snap); err != nil {
		s.log.Warn("reconcile", "err", err)
	}
	s.dispatchDevelopers(ctx, snap, false)
	s.dispatchSingletons(ctx, snap, false)
	return nil
}

// localPass is a pass that makes no GitHub read calls of its own: it reuses
// the issue and PR lists from the last poll, so everything driven by the
// local mailbox (answered questions, review rounds) keeps moving at
// poll_interval even when GitHub is only polled every
// off_hours_poll_interval. It deliberately skips the human-feedback fetch and
// the product manager / QA has-work checks, all of which query GitHub. Until
// the first successful poll it does nothing.
func (s *Scheduler) localPass(ctx context.Context) {
	s.mu.Lock()
	issues, prs, ok := s.lastIssues, s.lastPRs, s.polled
	s.mu.Unlock()
	if !ok {
		return
	}
	snap := s.classify(issues, prs)
	s.setQueues(snap)
	if err := s.reconcile(ctx, snap); err != nil {
		s.log.Warn("reconcile", "err", err)
	}
	s.dispatchDevelopers(ctx, snap, true)
	s.dispatchSingletons(ctx, snap, true)
}

// reconcile applies label transitions that depend on local state:
//
//   - visible issues without a state label enter triage (and receive the
//     factory label if the filter does not already require it);
//   - blocked issues whose question has been answered move back to the
//     stage that asked (developer -> ready, project manager -> triage).
//     Mail from a human about the issue counts as an answer too.
func (s *Scheduler) reconcile(ctx context.Context, snap *snapshot) error {
	var errs []error
	for _, i := range snap.byState[""] {
		add := []string{s.labels.Triage}
		if !github.HasLabel(i.Labels, s.labels.Base) {
			add = append(add, s.labels.Base)
		}
		s.log.Info("new issue enters triage", "issue", i.Number, "title", i.Title)
		if err := s.gh.EditLabels(ctx, i.Number, add, nil); err != nil {
			errs = append(errs, err)
			continue
		}
		i.Labels = append(i.Labels, github.Label{Name: s.labels.Triage})
		snap.byState["triage"] = append(snap.byState["triage"], i)
	}
	for _, i := range snap.byState["blocked"] {
		if s.hasUnreadMail(config.RoleDeveloper, i.Number, 0) {
			s.log.Info("question answered, issue back to ready", "issue", i.Number)
			if err := s.setState(ctx, i.Number, s.labels.Ready); err != nil {
				errs = append(errs, err)
				continue
			}
			i.Labels = relabel(i.Labels, s.labels.Blocked, s.labels.Ready)
			snap.byState["ready"] = append(snap.byState["ready"], i)
		} else if s.hasUnreadMail(config.RoleProjectManager, i.Number, 0) {
			s.log.Info("question answered, issue back to triage", "issue", i.Number)
			if err := s.setState(ctx, i.Number, s.labels.Triage); err != nil {
				errs = append(errs, err)
				continue
			}
			i.Labels = relabel(i.Labels, s.labels.Blocked, s.labels.Triage)
			snap.byState["triage"] = append(snap.byState["triage"], i)
		}
	}
	return errors.Join(errs...)
}

func relabel(labels []github.Label, from, to string) []github.Label {
	out := make([]github.Label, 0, len(labels)+1)
	for _, l := range labels {
		if l.Name != from {
			out = append(out, l)
		}
	}
	return append(out, github.Label{Name: to})
}

// dispatchDevelopers hands issues to free developer workers. Issues that
// are already in progress or in review but not owned by a worker (for
// example after a restart) are resumed first, then ready issues oldest first.
//
// On a local pass (local) the snapshot comes from the last poll and can be
// stale: an issue a worker has since finished, a developer parked in
// bees:blocked or a human closed still carries its old state label. Before
// spending a session on such an issue the live issue is fetched once and the
// candidate dropped unless it is still open and in a dispatchable state.
func (s *Scheduler) dispatchDevelopers(ctx context.Context, snap *snapshot, local bool) {
	if !s.roleEnabled(config.RoleDeveloper) {
		return
	}
	var candidates []github.Issue
	candidates = append(candidates, snap.byState["in-progress"]...)
	candidates = append(candidates, snap.byState["review"]...)
	candidates = append(candidates, snap.byState["ready"]...)
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
		select {
		case <-s.slots:
		default:
			return // pool is full
		}
		if local {
			live, ok := s.liveCandidate(ctx, issue)
			if !ok {
				s.slots <- struct{}{}
				continue
			}
			issue = live
		}
		w := &state.Worker{Name: fmt.Sprintf("dev-%d", issue.Number), Issue: issue.Number, Stage: "starting", Since: s.now()}
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
			}()
			if err := s.workIssue(ctx, issue, w); err != nil && ctx.Err() == nil {
				s.log.Error("developer worker failed", "issue", issue.Number, "err", err)
				s.setBackoff(fmt.Sprintf("issue-%d", issue.Number), 5*s.cfg.Scheduler.PollInterval.Duration)
			}
		}(issue, w)
	}
}

// liveCandidate re-reads an issue picked from a stale (cached) snapshot and
// reports whether a developer worker should still be started for it. It also
// refreshes the cached copy, so an issue that has moved on is not fetched
// again by the next local pass.
func (s *Scheduler) liveCandidate(ctx context.Context, issue github.Issue) (github.Issue, bool) {
	live, err := s.gh.GetIssue(ctx, issue.Number)
	if err != nil {
		s.log.Warn("live issue check failed, skipping", "issue", issue.Number, "err", err)
		return issue, false
	}
	s.cacheIssue(live)
	if live.State != "" && !strings.EqualFold(live.State, "open") {
		return live, false
	}
	switch s.stateOf(live.Labels) {
	case "ready", "in-progress", "review":
		return live, true
	default:
		return live, false
	}
}

// cacheIssue replaces an issue in the lists kept from the last poll.
func (s *Scheduler) cacheIssue(live github.Issue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.lastIssues {
		if s.lastIssues[i].Number == live.Number {
			s.lastIssues[i] = live
			return
		}
	}
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

// escalate marks an issue needs-human and leaves a comment for people.
// This is the only place the factory writes a GitHub comment.
func (s *Scheduler) escalate(ctx context.Context, number int, reason string) error {
	s.log.Warn("escalating to human", "issue", number, "reason", reason)
	if err := s.setState(ctx, number, s.labels.NeedsHuman); err != nil {
		return err
	}
	body := fmt.Sprintf("🐝 **busybees needs a human.**\n\n%s\n\nRemove the `%s` label and add `%s` (or `%s`) to hand it back to the factory.",
		reason, s.labels.NeedsHuman, s.labels.Ready, s.labels.Triage)
	return s.gh.Comment(ctx, number, body)
}

func (s *Scheduler) writeStatus() {
	s.mu.Lock()
	st := state.Status{LastPoll: s.lastPoll, NextPoll: s.nextPoll, Singletons: map[string]string{}, Queues: map[string]int{}, LastError: s.lastErr}
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
	s.mu.Unlock()
	if err := s.store.SaveStatus(st); err != nil {
		s.log.Warn("write status", "err", err)
	}
}

func (s *Scheduler) updateWorker(w *state.Worker, stage string, round int) {
	s.mu.Lock()
	w.Stage = stage
	w.Round = round
	w.Attempt = 1
	s.mu.Unlock()
	s.writeStatus()
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
