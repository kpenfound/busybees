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
	waiting  map[int][]int
	// warnedCycles remembers the issues we already warned about, so a
	// dependency cycle is reported once per process rather than per poll.
	warnedCycles map[int]bool
	// readySizes counts the ready queue by size label ("xs", "s", ...);
	// issues with no size label are counted under "".
	readySizes map[string]int
	wg         sync.WaitGroup
	slots      chan struct{}
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
		owned:        map[int]*state.Worker{},
		running:      map[string]bool{},
		backoff:      map[string]time.Time{},
		queues:       map[string]int{},
		waiting:      map[int][]int{},
		warnedCycles: map[int]bool{},
		readySizes:   map[string]int{},
		slots:        make(chan struct{}, d.Config.Scheduler.MaxDevelopers),
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
	if err := s.ensureLabels(ctx); err != nil {
		s.log.Warn("could not ensure labels", "err", capErrors(err))
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
	s.mu.Lock()
	defer s.mu.Unlock()
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
	s.queues["open_prs"] = len(snap.prs)
	s.readySizes = map[string]int{}
	for _, i := range snap.byState["ready"] {
		s.readySizes[s.sizeOf(i.Labels)]++
	}
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

// sortReady orders the ready queue in place as scheduler.dispatch_order asks:
// smallest or largest size first, ties broken by age (oldest first), which is
// also the "oldest" order poll already produced. sizeOf reads an issue's size
// label, so the helper is independent of a scheduler.
func sortReady(issues []github.Issue, order string, sizeOf func([]github.Label) string) {
	if order != config.DispatchSmallFirst && order != config.DispatchLargeFirst {
		return // "oldest" (and any unset value): poll's order stands
	}
	sort.SliceStable(issues, func(a, b int) bool {
		ra, rb := sizeRank(sizeOf(issues[a].Labels)), sizeRank(sizeOf(issues[b].Labels))
		if ra != rb {
			if order == config.DispatchLargeFirst {
				return ra > rb
			}
			return ra < rb
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
	if err := s.deliverHumanFeedback(ctx, snap); err != nil {
		s.log.Warn("human feedback", "err", err)
	}
	if err := s.checkPRs(ctx, snap); err != nil {
		s.log.Warn("check PRs", "err", err)
	}
	if err := s.reconcile(ctx, snap); err != nil {
		s.log.Warn("reconcile", "err", capErrors(err))
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
		s.log.Warn("reconcile", "err", capErrors(err))
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
//     Mail from a human about the issue counts as an answer too;
//   - ready issues without a size get the default one, and ready issues
//     sized above roles.developer.max_size go back to triage to be split.
//     The sizing runs last so that an issue unblocked by the loop above is
//     sized in the same pass instead of the next one.
//
// Every label edit is also written back to the cached poll (cacheIssue), or
// the local passes in between two polls would classify the issue from its
// stale labels and repeat the edit on every one of them.
func (s *Scheduler) reconcile(ctx context.Context, snap *snapshot) error {
	var errs []error
	var unlabelled []github.Issue
	for _, i := range snap.byState[""] {
		add := []string{s.labels.Triage}
		if !github.HasLabel(i.Labels, s.labels.Base) {
			add = append(add, s.labels.Base)
		}
		s.log.Info("new issue enters triage", "issue", i.Number, "title", i.Title)
		if err := s.gh.EditLabels(ctx, i.Number, add, nil); err != nil {
			errs = append(errs, err)
			unlabelled = append(unlabelled, i)
			continue
		}
		for _, l := range add {
			i.Labels = append(i.Labels, github.Label{Name: l})
		}
		snap.byState["triage"] = append(snap.byState["triage"], i)
		// Keep the cached poll in step, or every local pass in between two
		// polls asks GitHub to add the label again.
		s.cacheIssue(i)
	}
	snap.byState[""] = unlabelled
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
	// The pass moved issues between buckets; recount so `bees status` shows
	// what GitHub now shows instead of the poll's stale counts.
	s.setQueues(snap)
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
// example after a restart) are resumed first and are never reordered: a
// worker picking its issue back up after a restart must not be starved.
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
	if !s.roleEnabled(config.RoleDeveloper) {
		return
	}
	var candidates []github.Issue
	candidates = append(candidates, snap.byState["in-progress"]...)
	candidates = append(candidates, snap.byState["review"]...)
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
	sortReady(ready, s.cfg.Scheduler.DispatchOrder, s.sizeOf)
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
		// The cap only holds back fresh work: a resumed in-progress or
		// review issue, or a ready one with an open PR, is already in
		// flight. Checked before a slot is taken, so a held issue does not
		// keep a free developer idle.
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
			live, ok := s.liveCandidate(ctx, issue)
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
			}()
			if err := s.workIssue(ctx, issue, w); err != nil && ctx.Err() == nil {
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
	body := fmt.Sprintf("🐝 **busybees needs a human.**\n\n%s\n\nRemove the `%s` label and add `%s` (or `%s`) to hand it back to the factory.",
		reason, s.labels.NeedsHuman, s.labels.Ready, s.labels.Triage)
	// The factory and the people it works for share one GitHub account, so a
	// comment notifies nobody by itself: mention scheduler.notify.
	if m := s.cfg.Mentions(); m != "" {
		body = m + "\n\n" + body
	}
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
