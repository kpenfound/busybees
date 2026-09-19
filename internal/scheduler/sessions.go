package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/ops"
	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/core/work"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/text"
)

// sessionSpec describes one session to run for a role.
type sessionSpec struct {
	work      work.Ref
	role      string
	name      string
	workspace vcs.Workspace
	branch    string
	data      prompts.Data
	env       map[string]string
	// task selects a task template other than the role's default.
	task string
	// fallbacks is how many steps down its profile's fallback chain this
	// attempt runs (scheduler.retry_with_fallback): the first retry on the
	// profile's fallback, the second on that one's own, and so on. 0 is
	// the profile itself.
	fallbacks int
	// resumeID continues the conversation of this role's previous session
	// on the same work item (its Result.ClaudeID), so the model keeps the
	// context it built there, and resumeAgent is the agent that gave the
	// id (Result.Agent): the session runs resumed only when it runs as that
	// agent, since a retry down the fallback chain can have left an id of
	// another agent's, which the profile's own agent would fail on. A retry
	// never carries it: an id the agent no longer knows is the one way a
	// resumed launch fails.
	resumeID    string
	resumeAgent string
	// worker, when set, is updated with the attempt number so `bees status`
	// shows that a session is being retried.
	worker *state.Worker
	// attempt numbers a best-of-N or mixture-of-experts attempt, 1 to N
	// (bestofn.go); 0 is any other session. An attempt runs
	// roles.developer.best_of_n_model and best_of_n_prompt where they are
	// set, and is told of no interrupted session: that report is about one
	// branch, and the attempts each work on their own.
	attempt int
	// moeExpert names the expert a mixture-of-experts attempt runs as, one
	// of roles.developer.moe_experts_by_size for the issue's size: the
	// attempt runs that expert's model and prompt instead of the best-of-N
	// ones. Empty for every other session, best-of-N attempts included.
	moeExpert string
	// assembler marks the session that makes the result of a fan-out
	// (bestofn.go): it runs roles.developer.assembler_model and
	// assembler_prompt where they are set.
	assembler bool
	// moeAssembler narrows assembler to the session that combines the work
	// of a mixture-of-experts fan-out, a different job from picking among
	// best-of-N's retries: it runs roles.developer.moe_assembler_model and
	// moe_assembler_prompt instead. Never set without assembler.
	moeAssembler bool
	// judge marks the reviewer's judge session, the one that posts what the
	// review found (review.go): it runs roles.reviewer.judge_profile where it
	// is set.
	judge bool
	// reviewActivity identifies the pipeline this judge replaces in live
	// views. Verification rounds leave it empty.
	reviewActivity string
}

// runSession resolves the role, renders prompts and runs the session.
func (s *Scheduler) runSession(ctx context.Context, spec sessionSpec) (_ *session.Result, resultErr error) {
	startedActivity := false
	defer func() {
		// Preparation can fail before session-started hands the row off.
		if spec.reviewActivity != "" && !startedActivity && resultErr != nil {
			ev := sessionEvent(EventReviewEnded, spec)
			ev.Session, ev.Err = "", resultErr.Error()
			s.publish(ev)
		}
	}()
	// One copy of the configuration for the whole start: a reload landing
	// between two reads here would hand the session half of each.
	cfg := s.config()
	role, err := cfg.Role(spec.role)
	if err != nil {
		return nil, err
	}
	// Select the whole profile before attempt, assembler and retry overrides.
	if spec.data.Issue != nil {
		role = role.ForSize(s.sizeOf(spec.data.Issue.Labels))
	} else if spec.data.PR != nil {
		role = role.ForSize(s.sizeOf(spec.data.PR.Labels))
	}
	switch {
	case spec.moeExpert != "":
		// The expert is already resolved against the size's model and the
		// developer's prompt (config.ResolvedRole.MoEExperts).
		if e, ok := expertNamed(role, s.sizeOf(spec.data.Issue.Labels), spec.moeExpert); ok {
			role.Model, role.Prompt = e.Model, e.Prompt
		}
	case spec.attempt > 0:
		if role.BestOfNModel != "" {
			role.Model = role.BestOfNModel
		}
		if role.BestOfNPrompt != "" {
			role.Prompt = role.BestOfNPrompt
		}
	}
	if spec.assembler {
		model, prompt := role.AssemblerModel, role.AssemblerPrompt
		if spec.moeAssembler {
			model, prompt = role.MoEAssemblerModel, role.MoEAssemblerPrompt
		}
		if model != "" {
			role.Model = model
		}
		if prompt != "" {
			role.Prompt = prompt
		}
	}
	if spec.judge {
		role = role.ForJudge()
	}
	// The profile the session runs, moved down its fallback chain as far as
	// the retry asks: its agent, model, effort and sandbox all come from
	// the profile the retry lands on.
	profile, fallback := ops.SelectProfile(session.ProfileForRole(role), spec.fallbacks)
	s.setWorkerSandbox(spec.worker, profile.Sandbox)
	resumeID := spec.resumeID
	if spec.resumeAgent != profile.Agent {
		resumeID = ""
	}
	if err := s.store.EnsureNotes(spec.role); err != nil {
		return nil, err
	}
	// The notes reach the session through notes_read, not the prompt; only
	// their size is needed here, for the consolidation decision. It is
	// measured through the configured backend, which is where the session
	// will read them from.
	notesSize, err := s.notes.Size(ctx, spec.role)
	if err != nil {
		return nil, err
	}
	sessionDir, err := s.runner.NewSessionDir(spec.name)
	if err != nil {
		return nil, err
	}
	if ref := sessionWork(spec); ref.Key != "" {
		err := session.WriteWork(sessionDir, ref)
		s.op("session-work-file", err, "could not record the session's work", "dir", sessionDir, "err", err)
	}

	d := spec.data
	d.Project = cfg.Project
	d.Filter = cfg.Filter
	d.Labels = s.labels
	d.AutoMerge = cfg.Merge().AutoMerge
	d.ReportFactoryErrors = cfg.Scheduler.ReportFactoryErrors
	d.FeatureProposals = cfg.Scheduler.Proposals()
	d.MinIssueSize = cfg.MinIssueSize()
	d.CommitFlags = cfg.CommitFlags()
	d.Notify = cfg.Mentions()
	d.MaxSize = cfg.MaxSize()
	d.WorkDir = spec.workspace.Directory()
	d.Branch = spec.branch
	d.StateDir = s.store.Dir
	d.SessionDir = sessionDir
	d.Sandbox = profile.Sandbox
	d.ConsolidateNotes, d.ConsolidateReason = s.consolidateNotes(spec.role, int(notesSize))
	if d.MaxRounds == 0 {
		d.MaxRounds = cfg.Scheduler.MaxReviewRounds
	}
	if sessionWork(spec).Key != "" && spec.attempt == 0 {
		// What a session that never finished on this issue left behind — a
		// scheduler dying while it worked, or a hard stop — for the first
		// session of the role it was interrupted in.
		d.Interrupted = s.interruptedFor(sessionWork(spec).Key, spec.role)
	}

	// The project's own prompt files come from the worktree, so a branch's
	// bees/prompts/<role>.md applies to the session working on that branch.
	// A file bees cannot read must never take a session down: the ones that
	// did read are used, the rest are skipped with a warning, and `bees
	// doctor` is where a broken file fails loudly. The degraded operation is
	// keyed by role because each role reads a different set of files: one
	// role's session succeeding says nothing about another role's file, and
	// a shared name would let it clear the streak.
	project, perr := prompts.LoadProject(spec.workspace.Directory(), spec.role)
	s.op("project-prompts/"+spec.role, perr, "project prompt file skipped", "role", spec.role, "err", perr)
	system, err := prompts.System(spec.role, d, role.Prompt, project...)
	if err != nil {
		return nil, err
	}
	taskName := spec.task
	if taskName == "" {
		taskName = spec.role
	}
	task, err := prompts.TaskNamed(spec.role, taskName, d)
	if err != nil {
		return nil, err
	}

	env := map[string]string{}
	if d.Issue != nil {
		env[session.EnvIssue] = strconv.Itoa(d.Issue.Number)
	}
	if d.PR != nil {
		env[session.EnvPR] = strconv.Itoa(d.PR.Number)
	}
	if spec.branch != "" {
		env[session.EnvBranch] = spec.branch
	}
	for k, v := range spec.env {
		env[k] = v
	}

	started := s.now()
	// The session's own context: cancelling the loop's ctx leaves it alone,
	// so an interrupted `bees run` finishes the session, and only HardStop —
	// which cancels this one — kills it (scheduler.go).
	sctx := s.sessionContext(ctx)
	// Record the session before it runs and clear it however it ends: a
	// record that outlives its session is what tells the next one that this
	// scheduler was killed while the session was working (interrupted.go).
	// A session HardStop killed is the one deliberate exception: its record
	// is kept, because the record *is* the crash report the next scheduler
	// resumes from — a stopped factory left the work exactly as a killed one
	// would have. A session that had already finished when the hard stop
	// landed wrote its result file, so the stale record it leaves is cleared
	// silently by the next worker (takeInterrupted).
	if sessionWork(spec).Key != "" {
		s.recordRunningSession(spec, sessionWork(spec), sessionDir)
		defer func() {
			if sctx.Err() != nil {
				return
			}
			s.clearRunningSession(sessionWork(spec))
		}()
	}
	start := sessionEvent(EventSessionStarted, spec)
	start.Model, start.Fallback, start.Sandbox = profile.Model, fallback, profile.Sandbox
	start.Dir = sessionDir
	// Remembered under the same name the event carries, so a view that sees
	// the session start can name it back to KillSession (kill.go). The event
	// carries the directory too, for a view that only wants to read the
	// transcript in it; KillSession takes a name, so that stopping a session
	// is asking the scheduler about one of its own rather than handing it a
	// path to kill.
	s.recordLiveSession(spec.name, liveSession{role: spec.role, dir: sessionDir, Work: start.Work.Clone()})
	defer s.dropLiveSession(spec.name)
	s.publish(start)
	startedActivity = true
	res, err := s.runner.Run(sctx, session.Request{
		Name:         spec.name,
		Profile:      profile,
		Workspace:    spec.workspace,
		SystemPrompt: system,
		Prompt:       task,
		Env:          env,
		SessionDir:   sessionDir,
		ResumeID:     resumeID,
	})
	// Whatever the session changed on GitHub through the MCP server — an
	// issue it triaged, a sub-issue it filed — goes into the cached poll
	// before the wake, so the local pass the wake runs classifies from the
	// fresh labels rather than from the ones the last poll saw. A session
	// that failed may have made its edits first, so this runs either way.
	touched := s.refreshTouched(ctx, sessionDir)
	// And whatever it posted on GitHub with `gh`, outside the tool that
	// appends the marker for it, is checked for one — on the pull request the
	// session reported opening as well as the ones it was given (markers.go).
	s.auditMarkers(ctx, spec, touched, openedPR(sessionDir), started)
	// A finished session is the factory's main local event: it may have
	// written mail to another role, and it is one step closer to freeing the
	// slot it holds. Wake the loop rather than let that wait for the next
	// tick — including when the session failed, which frees the slot too.
	s.signal()
	if err != nil {
		failed := sessionEvent(EventSessionEnded, spec)
		failed.Outcome, failed.Err = "failed", err.Error()
		s.publish(failed)
		return nil, err
	}
	// Whatever the session created must stay visible to the factory.
	s.adoptCreated(ctx, started)
	s.countSession(spec.role, d.ConsolidateNotes)
	s.record(spec, res)
	s.summarize(spec, res)
	s.publish(endEvent(spec, res))
	return res, nil
}

// endEvent describes a finished session for the event stream: what it
// reported, what it cost and how long it took. A developer session that
// opened a pull request names it, exactly as the summary line does, so a
// view learns the number as soon as the session is over.
func endEvent(spec sessionSpec, res *session.Result) Event {
	ev := sessionEvent(EventSessionEnded, spec)
	ev.Outcome, ev.Note = outcomeOf(res)
	ev.Turns, ev.CostUSD, ev.Duration = res.NumTurns, res.CostUSD, res.Duration
	ev.CostKnown = res.CostKnown
	if ghwork.PR(res.Outcome.Work) > 0 {
		ev.Work = ghwork.WithPR(ev.Work, ghwork.PR(res.Outcome.Work))
	}
	return ev
}

// consolidateNotes decides whether the session about to run is also asked
// to consolidate its notes, and why. Developer workers run
// concurrently and share one role state file, so the read is locked.
func (s *Scheduler) consolidateNotes(role string, notesLen int) (bool, string) {
	cfg := s.config()
	every, maxBytes := cfg.Scheduler.NotesConsolidateEvery, cfg.Scheduler.NotesMaxBytes
	s.mu.Lock()
	rs, err := s.store.Role(role)
	s.mu.Unlock()
	if err != nil {
		// Notes bookkeeping must never cost the factory a session.
		s.log.Warn("could not read role bookkeeping", "role", role, "err", err)
		return false, ""
	}
	if !needsConsolidation(rs, notesLen, every, maxBytes) {
		return false, ""
	}
	return true, consolidateReason(notesLen, every, maxBytes)
}

// needsConsolidation reports whether the session starting now (the
// notesLen-byte notes are the ones it will read) should also consolidate
// its notes: either enough sessions have run since the last pass, or the
// notes have grown past maxBytes.
func needsConsolidation(rs state.RoleState, notesLen, every, maxBytes int) bool {
	if maxBytes > 0 && notesLen > maxBytes {
		return true
	}
	if every <= 0 {
		return false
	}
	return rs.Sessions+1-rs.LastConsolidated >= every
}

// consolidateReason names the trigger, for the prompt.
func consolidateReason(notesLen, every, maxBytes int) string {
	if maxBytes > 0 && notesLen > maxBytes {
		return fmt.Sprintf("notes are %s", byteSize(notesLen))
	}
	return "every " + text.Count(every, "session")
}

// byteSize renders a notes size the way a person would say it.
func byteSize(n int) string {
	if n < 1024 {
		return fmt.Sprintf("%d bytes", n)
	}
	return fmt.Sprintf("%d KB", n/1024)
}

// countSession records that one more session ran for the role, and that it
// was asked to consolidate its notes. The state is re-read under the lock so
// concurrent developer workers do not lose each other's counts.
func (s *Scheduler) countSession(role string, consolidated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rs, err := s.store.Role(role)
	if err != nil {
		s.log.Warn("could not read role bookkeeping", "role", role, "err", err)
		return
	}
	rs.Sessions++
	if consolidated {
		rs.LastConsolidated = rs.Sessions
	}
	if err := s.store.SaveRole(role, rs); err != nil {
		s.log.Warn("could not record the session for the role", "role", role, "err", err)
	}
}

// record appends the finished session to the ledger. Accounting must never
// cost the factory a session, so a failure only warns. A session whose
// agent reported no cost (codex, or one that died before its result event)
// is entered with its cost unknown rather than as a free one.
func (s *Scheduler) record(spec sessionSpec, res *session.Result) {
	status, _ := outcomeOf(res)
	e := state.LedgerEntry{
		Time:         s.now(),
		Role:         spec.role,
		Session:      spec.name,
		Turns:        res.NumTurns,
		CostUSD:      res.CostUSD,
		CostUnknown:  !res.CostKnown,
		DurationMS:   res.Duration.Milliseconds(),
		Outcome:      status,
		ErrorSubtype: res.ErrorSubtype,
		TimedOut:     res.TimedOut,
	}
	e.Work = sessionWork(spec)
	if ghwork.PR(res.Outcome.Work) > 0 {
		e.Work = ghwork.WithPR(e.Work, ghwork.PR(res.Outcome.Work))
	}
	err := s.store.AppendLedger(e)
	s.op("ledger", err, "could not record the session in the ledger", "session", spec.name, "error", err)
	// The issue's running total is what scheduler.max_cost_per_issue is spent
	// against; every session run for the issue counts, retries included.
	s.recordWorkCost(e.Work, e.CostUSD)
}

// summary is everything a session summary line needs.
type summary struct {
	role    string
	issue   int
	pr      int
	outcome string
	note    string
	turns   int
	cost    float64
	// costKnown says whether cost is what the session cost. A cost arrives
	// in the event that ends a session's stream alone, so a session that
	// died before emitting one has no cost rather than a cost of zero, and
	// the line says so instead of printing $0.00. A codex session never
	// reports one: it reports tokens rather than a price.
	costKnown bool
	dur       time.Duration
}

// summarize emits the one-line report of a finished session. It runs for
// every session, which is why it lives at the end of runSession.
func (s *Scheduler) summarize(spec sessionSpec, res *session.Result) {
	status, note := outcomeOf(res)
	sum := summary{
		role:      spec.role,
		outcome:   status,
		note:      note,
		turns:     res.NumTurns,
		cost:      res.CostUSD,
		costKnown: res.CostKnown,
		dur:       res.Duration,
	}
	if spec.data.Issue != nil {
		sum.issue = spec.data.Issue.Number
	}
	if spec.data.PR != nil {
		sum.pr = spec.data.PR.Number
	}
	if ghwork.PR(res.Outcome.Work) > 0 {
		sum.pr = ghwork.PR(res.Outcome.Work)
	}
	s.log.Info(formatSummary(sum), logging.SummaryKey, true,
		"role", sum.role, "issue", sum.issue, "pr", sum.pr, "outcome", sum.outcome,
		"turns", sum.turns, "cost_usd", sum.cost, "duration", sum.dur, "note", sum.note)
}

// formatSummary renders a session summary:
//
//	<mark> <role title> <subject> <phrase>[: "<note>"] (<turns>, $<cost>, <duration>)
//
// A session whose cost is not known — one killed before its agent
// reported it, and every codex session — reads "cost unknown" where the
// amount goes.
func formatSummary(sum summary) string {
	var b strings.Builder
	b.WriteString(summaryMark(sum.outcome))
	b.WriteString(" " + roleTitle(sum.role))
	if subject := summarySubject(sum); subject != "" {
		b.WriteString(" " + subject)
	}
	b.WriteString(" " + summaryPhrase(sum))
	if sum.note != "" {
		b.WriteString(`: "` + oneLine(sum.note, noteLimit) + `"`)
	}
	cost := "cost unknown"
	if sum.costKnown {
		cost = fmt.Sprintf("$%.2f", sum.cost)
	}
	fmt.Fprintf(&b, " (%s, %s, %s)", text.Count(sum.turns, "turn"), cost, sum.dur.Round(time.Second))
	return b.String()
}

// noteLimit is how much of a session note a summary line shows.
const noteLimit = 80

func summaryMark(outcome string) string {
	switch outcome {
	case OutcomeChangesRequested, OutcomeFailed:
		return "✗"
	default:
		return "✓"
	}
}

// summarySubject is what the session was about: the pull request for a
// reviewer, the issue for a developer, nothing for the singleton roles.
func summarySubject(sum summary) string {
	if sum.role == config.RoleReviewer && sum.pr > 0 {
		return fmt.Sprintf("PR #%d", sum.pr)
	}
	if sum.issue > 0 {
		return fmt.Sprintf("issue #%d", sum.issue)
	}
	return ""
}

func summaryPhrase(sum summary) string {
	switch sum.outcome {
	case OutcomePROpened:
		return fmt.Sprintf("→ PR #%d opened", sum.pr)
	case OutcomePRUpdated:
		return fmt.Sprintf("→ PR #%d updated", sum.pr)
	case OutcomeQuestion:
		return "asked the project manager"
	case OutcomeChangesRequested:
		return "changes requested"
	default:
		return sum.outcome
	}
}

// inbox returns unread mail for a role. When issue or pr is non-zero only
// messages about that item are returned; otherwise every unread message.
func (s *Scheduler) inbox(role string, issue, pr int) ([]mail.Message, error) {
	msgs, err := s.mail.List(mail.Filter{To: role, UnreadOnly: true})
	if err != nil {
		return nil, err
	}
	if issue == 0 && pr == 0 {
		return msgs, nil
	}
	var out []mail.Message
	for _, m := range msgs {
		if (issue > 0 && ghwork.Issue(m.Work) == issue) || (pr > 0 && ghwork.PR(m.Work) == pr) {
			out = append(out, m)
		}
	}
	return out, nil
}

func (s *Scheduler) hasUnreadMail(role string, issue, pr int) bool {
	msgs, err := s.inbox(role, issue, pr)
	return err == nil && len(msgs) > 0
}

// sentSince reports whether role received a message about issue/pr created
// at or after t, from anybody. Used to verify that a session which claims to
// have asked a question or requested changes actually sent the mail.
func (s *Scheduler) sentSince(role string, issue, pr int, t time.Time) bool {
	return s.sentSinceFrom("", role, issue, pr, t)
}

// sentSinceFrom is sentSince restricted to one sender, for a claim only that
// role's own mail can settle: a role two others write to would otherwise be
// verified by somebody else's message.
func (s *Scheduler) sentSinceFrom(from, role string, issue, pr int, t time.Time) bool {
	msgs, err := s.mail.List(mail.Filter{To: role, From: from})
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if m.CreatedAt.Before(t.Add(-time.Second)) {
			continue
		}
		if (issue > 0 && ghwork.Issue(m.Work) == issue) || (pr > 0 && ghwork.PR(m.Work) == pr) || (issue == 0 && pr == 0) {
			return true
		}
	}
	return false
}

// runSessionWithRetry runs a session and repeats it, up to
// scheduler.retries times, while it keeps failing for infrastructure
// reasons. The result of the last attempt is returned either way, except
// for one failure that is not the session's: a session that died on the
// account-wide claude session limit without reporting an outcome returns at
// once with errSessionLimited, spending no retry attempt, because every
// attempt and every other role would hit the same wall (see limits.go).
func (s *Scheduler) runSessionWithRetry(ctx context.Context, spec sessionSpec) (*session.Result, error) {
	policy := s.config().Retry()
	for attempt := 1; ; attempt++ {
		try := spec
		if attempt > 1 {
			// Its own name, so <state_dir>/sessions/ keeps both transcripts.
			try.name = fmt.Sprintf("%s-retry%d", spec.name, attempt-1)
			try.data.Retry = attempt - 1
			if policy.Decide(attempt-1, true).UseFallback {
				try.fallbacks = attempt - 1
			}
			// A resumed launch that failed may have failed on the resume
			// itself (an id claude no longer has): the retry starts fresh.
			try.resumeID = ""
		}
		s.setWorkerAttempt(spec.worker, attempt)
		res, err := s.runSession(ctx, try)
		if s.tookKill(try.name) {
			// A person stopped this session from the live view, and
			// KillSession has already handed the issue to them. Retrying it
			// would start the work again on an issue that is now theirs, and
			// escalating it a second time would say the factory gave up
			// where a person stepped in.
			return nil, errSessionKilled
		}
		if err != nil {
			return nil, err
		}
		// A blocking event is an honest report about the account even from
		// a session that finished, so the pause is recorded either way; but
		// a session that reported an outcome did its work, and the caller
		// must still read it.
		if s.recordSessionLimit(res) && !res.HasOutcome {
			return res, errSessionLimited
		}
		if note, over := overSessionBudget(res, s.config().Scheduler.MaxCostPerSession); over {
			streak := s.overBudgetStreak(budgetKey(spec), true)
			s.log.Warn("session over its cost budget; treating it as failed",
				"role", spec.role, "session", try.name, "cost_usd", res.CostUSD,
				"max_cost_per_session", s.config().Scheduler.MaxCostPerSession, "consecutive", streak)
			if streak >= overBudgetEscalateAfter || !policy.Decide(attempt, true).Retry {
				return failedResult(res, overBudgetNote(note, streak, spec.role)), nil
			}
			// One expensive session can be bad luck, so it is retried like
			// an infrastructure failure — on the profile's fallback when
			// scheduler.retry_with_fallback is on, which is usually the
			// cheaper one.
			if err := ops.Sleep(ctx, policy.Decide(attempt, true).Delay); err != nil {
				return failedResult(res, note), err
			}
			continue
		}
		if s.config().Scheduler.MaxCostPerSession > 0 {
			s.overBudgetStreak(budgetKey(spec), false)
		}
		kind := ops.ClassifyFailure(res)
		decision := policy.Decide(attempt, kind == ops.FailureInfra)
		if !decision.Retry {
			return res, nil
		}
		s.log.Warn("session failed; retrying",
			"role", spec.role, "session", try.name, "attempt", attempt,
			"kind", kind.String(), "reason", ops.InfraReason(res), "in", policy.Delay)
		if err := ops.Sleep(ctx, decision.Delay); err != nil {
			return res, err
		}
	}
}

// overBudgetNote is what an over-budget session reports as its failure. A
// streak says more than a single session does: two in a row means the role's
// max_turns or timeout are the wrong shape for this work, not that one
// session went astray.
func overBudgetNote(note string, streak int, role string) string {
	if streak < overBudgetEscalateAfter {
		return note
	}
	return fmt.Sprintf("%s, and %d sessions in a row have. Raise the budget, or lower roles.%s.max_turns / timeout so a session cannot cost this much.",
		note, streak, role)
}

// sessionFailure renders the escalation text for a session that ended
// badly, naming the classification and, for infrastructure failures, how
// many attempts were made.
func (s *Scheduler) sessionFailure(role string, res *session.Result, status, note string) string {
	if ops.ClassifyFailure(res) != ops.FailureInfra {
		return fmt.Sprintf("The %s session ended with `%s`: %s", roleTitle(role), status, note)
	}
	attempts := s.config().Retry().Retries + 1
	if attempts == 1 {
		return fmt.Sprintf("The %s session failed for infrastructure reasons (%s): %s", roleTitle(role), ops.InfraReason(res), note)
	}
	return fmt.Sprintf("The %s session failed %d times for infrastructure reasons (%s): %s", roleTitle(role), attempts, ops.InfraReason(res), note)
}

// outcomeOf returns the session's reported status, or a synthetic one when
// the session failed or reported nothing.
func outcomeOf(res *session.Result) (status, note string) {
	if res.HasOutcome && res.Outcome.Status != "" {
		return res.Outcome.Status, res.Outcome.Note
	}
	if res.TimedOut {
		return "failed", "session timed out"
	}
	if res.IsError {
		return "failed", fmt.Sprintf("session error (%s): %s", res.ErrorSubtype, truncate(res.ResultText, 500))
	}
	return "failed", "session ended without reporting an outcome"
}

// truncate shortens s to at most n runes, appending "…" when it cut. It
// counts runes, not bytes, so the result is always valid UTF-8.
func truncate(s string, n int) string {
	count := 0
	for i := range s {
		count++
		if count > n {
			return s[:i] + "…"
		}
	}
	return s
}

// oneLine flattens every run of whitespace in s to a single space and then
// truncates it to n runes. Use it wherever the text becomes part of a log
// message that must stay on one line: in text format the console handler
// prints a summary record as its bare message, so a newline in the note
// would break the one-line-per-session contract.
func oneLine(s string, n int) string {
	return truncate(strings.Join(strings.Fields(s), " "), n)
}

func roleTitle(role string) string { return prompts.Title(role) }

// sessionWork translates prompt subjects once at the busybees boundary. A
// caller-provided reference retains its opaque key and all custom tags.
func sessionWork(spec sessionSpec) work.Ref {
	if spec.work.Key != "" {
		return spec.work.Clone()
	}
	var issue, pr int
	if spec.data.Issue != nil {
		issue = spec.data.Issue.Number
	}
	if spec.data.PR != nil {
		pr = spec.data.PR.Number
	}
	return ghwork.New(issue, pr)
}
