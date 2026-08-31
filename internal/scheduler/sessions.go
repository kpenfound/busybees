package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/text"
)

// sessionSpec describes one session to run for a role.
type sessionSpec struct {
	role    string
	name    string
	workDir string
	branch  string
	data    prompts.Data
	env     map[string]string
	// task selects a task template other than the role's default.
	task string
	// useFallback runs this attempt with the role's fallback model as its
	// primary model (scheduler.retry_with_fallback).
	useFallback bool
	// worker, when set, is updated with the attempt number so `bees status`
	// shows that a session is being retried.
	worker *state.Worker
}

// runSession resolves the role, renders prompts and runs claude.
func (s *Scheduler) runSession(ctx context.Context, spec sessionSpec) (*session.Result, error) {
	role, err := s.cfg.Role(spec.role)
	if err != nil {
		return nil, err
	}
	// A copy: the configured role keeps its own model. The developer can run
	// a different model per work item size; a retry with the fallback model
	// overrides whatever the size picked.
	if spec.role == config.RoleDeveloper && spec.data.Issue != nil {
		role.Model = role.ModelFor(s.sizeOf(spec.data.Issue.Labels))
	}
	fallback := spec.useFallback && role.FallbackModel != ""
	if fallback {
		role.Model = role.FallbackModel
	}
	if err := s.store.EnsureNotes(spec.role); err != nil {
		return nil, err
	}
	notes, err := s.store.ReadNotes(spec.role)
	if err != nil {
		return nil, err
	}
	sessionDir, err := s.runner.NewSessionDir(spec.name)
	if err != nil {
		return nil, err
	}

	d := spec.data
	d.Project = s.cfg.Project
	d.Filter = s.cfg.Filter
	d.Labels = s.labels
	d.AutoMerge = s.cfg.Merge().AutoMerge
	d.CommitFlags = s.cfg.CommitFlags()
	d.Notify = s.cfg.Mentions()
	d.MaxSize = s.cfg.MaxSize()
	d.WorkDir = spec.workDir
	d.Branch = spec.branch
	d.StateDir = s.store.Dir
	d.SessionDir = sessionDir
	d.NotesFile = s.store.NotesPath(spec.role)
	d.Notes = notes
	d.ConsolidateNotes, d.ConsolidateReason = s.consolidateNotes(spec.role, len(notes))
	if d.Issue != nil && (spec.role == config.RoleDeveloper || spec.role == config.RoleReviewer) {
		d.Size = s.sizeOf(d.Issue.Labels)
	}
	if d.MaxRounds == 0 {
		d.MaxRounds = s.cfg.Scheduler.MaxReviewRounds
	}
	if d.Issue != nil {
		// What a scheduler killed while working this issue left behind, for
		// the first session of the role it was killed in.
		d.Interrupted = s.interruptedFor(d.Issue.Number, spec.role)
	}

	// The project's own prompt files come from the worktree, so a branch's
	// bees/prompts/<role>.md applies to the session working on that branch.
	// A file bees cannot read must never take a session down: the ones that
	// did read are used, the rest are skipped with a warning, and `bees
	// doctor` is where a broken file fails loudly. The degraded operation is
	// keyed by role because each role reads a different set of files: one
	// role's session succeeding says nothing about another role's file, and
	// a shared name would let it clear the streak.
	project, perr := prompts.LoadProject(spec.workDir, spec.role)
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

	env := map[string]string{session.EnvNotesFile: d.NotesFile}
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
	// Record the session before it runs and clear it however it ends: a
	// record that outlives its session is what tells the next one that this
	// scheduler was killed while the session was working (interrupted.go).
	if d.Issue != nil {
		s.recordRunningSession(spec, d.Issue.Number, sessionDir)
		defer s.clearRunningSession(d.Issue.Number)
	}
	start := sessionEvent(EventSessionStarted, spec)
	start.Model, start.Fallback = role.Model, fallback
	start.Dir = sessionDir
	// Remembered under the same name the event carries, so a view that sees
	// the session start can name it back to KillSession (kill.go). The event
	// carries the directory too, for a view that only wants to read the
	// transcript in it; KillSession takes a name, so that stopping a session
	// is asking the scheduler about one of its own rather than handing it a
	// path to kill.
	s.recordLiveSession(spec.name, liveSession{role: spec.role, dir: sessionDir, issue: start.Issue, pr: start.PR})
	defer s.dropLiveSession(spec.name)
	s.publish(start)
	res, err := s.runner.Run(ctx, session.Request{
		Name:         spec.name,
		Role:         role,
		WorkDir:      spec.workDir,
		SystemPrompt: system,
		Prompt:       task,
		Env:          env,
		SessionDir:   sessionDir,
	})
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
	if res.Outcome.PR > 0 {
		ev.PR = res.Outcome.PR
	}
	return ev
}

// consolidateNotes decides whether the session about to run is also asked
// to consolidate its notes file, and why. Developer workers run
// concurrently and share one role state file, so the read is locked.
func (s *Scheduler) consolidateNotes(role string, notesLen int) (bool, string) {
	every, maxBytes := s.cfg.Scheduler.NotesConsolidateEvery, s.cfg.Scheduler.NotesMaxBytes
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
// notesLen-byte notes file is the one it will be shown) should also
// consolidate its notes: either enough sessions have run since the last
// pass, or the file has grown past maxBytes.
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
		return fmt.Sprintf("file is %s", byteSize(notesLen))
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
// cost the factory a session, so a failure only warns.
func (s *Scheduler) record(spec sessionSpec, res *session.Result) {
	status, _ := outcomeOf(res)
	e := state.LedgerEntry{
		Time:         s.now(),
		Role:         spec.role,
		Session:      spec.name,
		Turns:        res.NumTurns,
		CostUSD:      res.CostUSD,
		DurationMS:   res.Duration.Milliseconds(),
		Outcome:      status,
		ErrorSubtype: res.ErrorSubtype,
		TimedOut:     res.TimedOut,
	}
	if spec.data.Issue != nil {
		e.Issue = spec.data.Issue.Number
	}
	if spec.data.PR != nil {
		e.PR = spec.data.PR.Number
	}
	if res.Outcome.PR > 0 {
		e.PR = res.Outcome.PR
	}
	err := s.store.AppendLedger(e)
	s.op("ledger", err, "could not record the session in the ledger", "session", spec.name, "error", err)
	// The issue's running total is what scheduler.max_cost_per_issue is spent
	// against; every session run for the issue counts, retries included.
	s.recordIssueCost(e.Issue, e.CostUSD)
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
	dur     time.Duration
}

// summarize emits the one-line report of a finished session. It runs for
// every session, which is why it lives at the end of runSession.
func (s *Scheduler) summarize(spec sessionSpec, res *session.Result) {
	status, note := outcomeOf(res)
	sum := summary{
		role:    spec.role,
		outcome: status,
		note:    note,
		turns:   res.NumTurns,
		cost:    res.CostUSD,
		dur:     res.Duration,
	}
	if spec.data.Issue != nil {
		sum.issue = spec.data.Issue.Number
	}
	if spec.data.PR != nil {
		sum.pr = spec.data.PR.Number
	}
	if res.Outcome.PR > 0 {
		sum.pr = res.Outcome.PR
	}
	s.log.Info(formatSummary(sum), logging.SummaryKey, true,
		"role", sum.role, "issue", sum.issue, "pr", sum.pr, "outcome", sum.outcome,
		"turns", sum.turns, "cost_usd", sum.cost, "duration", sum.dur, "note", sum.note)
}

// formatSummary renders a session summary:
//
//	<mark> <role title> <subject> <phrase>[: "<note>"] (<turns>, $<cost>, <duration>)
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
	fmt.Fprintf(&b, " (%s, $%.2f, %s)", text.Count(sum.turns, "turn"), sum.cost, sum.dur.Round(time.Second))
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
		if (issue > 0 && m.Issue == issue) || (pr > 0 && m.PR == pr) {
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
// at or after t. Used to verify that a session which claims to have asked a
// question or requested changes actually sent the mail.
func (s *Scheduler) sentSince(role string, issue, pr int, t time.Time) bool {
	msgs, err := s.mail.List(mail.Filter{To: role})
	if err != nil {
		return false
	}
	for _, m := range msgs {
		if m.CreatedAt.Before(t.Add(-time.Second)) {
			continue
		}
		if (issue > 0 && m.Issue == issue) || (pr > 0 && m.PR == pr) || (issue == 0 && pr == 0) {
			return true
		}
	}
	return false
}

// failureKind classifies why a session did not produce a usable result.
type failureKind int

const (
	// failureNone is the zero value: the session produced a result.
	failureNone failureKind = iota
	// failureInfra is a failure of the machinery around the model — a
	// timeout, an API error, exhausted turns, a crashed claude. Retrying it
	// later is likely to work.
	failureInfra
	// failureBehavioural is the session itself: it ran and reported (even
	// `failed`), or chose not to report at all. Running it again would only
	// repeat the same decision.
	failureBehavioural
)

func (k failureKind) String() string {
	switch k {
	case failureNone:
		return "none"
	case failureInfra:
		return "infrastructure"
	case failureBehavioural:
		return "behavioural"
	}
	return "unknown"
}

// classifyFailure decides whether a finished session is worth retrying.
func classifyFailure(res *session.Result) failureKind {
	switch {
	case res.HasOutcome && res.Outcome.Status != "":
		// The session ran and said what happened; retrying changes nothing.
		return failureBehavioural
	case res.TimedOut:
		return failureInfra
	case res.IsError, res.ExitCode != 0:
		return failureInfra
	case rateLimitedText(res.ResultText):
		return failureInfra
	default:
		// Clean exit, no error, no outcome: the model chose not to report.
		return failureBehavioural
	}
}

// infraReason names an infrastructure failure for logs and escalations.
func infraReason(res *session.Result) string {
	switch {
	case res.TimedOut:
		return "timed out"
	case res.ErrorSubtype == "error_max_turns":
		return "ran out of turns"
	case rateLimitedText(res.ResultText):
		return "rate limited or overloaded"
	case res.ErrorSubtype != "":
		return "session error (" + res.ErrorSubtype + ")"
	case res.ExitCode != 0:
		return fmt.Sprintf("claude exited with code %d", res.ExitCode)
	default:
		return "unknown"
	}
}

// runSessionWithRetry runs a session and repeats it, up to
// scheduler.retries times, while it keeps failing for infrastructure
// reasons. The result of the last attempt is returned either way, except
// for one failure that is not the session's: a session that died on the
// account-wide claude session limit without reporting an outcome returns at
// once with errSessionLimited, spending no retry attempt, because every
// attempt and every other role would hit the same wall (see limits.go).
func (s *Scheduler) runSessionWithRetry(ctx context.Context, spec sessionSpec) (*session.Result, error) {
	policy := s.cfg.Retry()
	for attempt := 1; ; attempt++ {
		try := spec
		if attempt > 1 {
			// Its own name, so <state_dir>/sessions/ keeps both transcripts.
			try.name = fmt.Sprintf("%s-retry%d", spec.name, attempt-1)
			try.data.Retry = attempt - 1
			try.useFallback = policy.WithFallback
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
		if note, over := overSessionBudget(res, s.cfg.Scheduler.MaxCostPerSession); over {
			streak := s.overBudgetStreak(budgetKey(spec), true)
			s.log.Warn("session over its cost budget; treating it as failed",
				"role", spec.role, "session", try.name, "cost_usd", res.CostUSD,
				"max_cost_per_session", s.cfg.Scheduler.MaxCostPerSession, "consecutive", streak)
			if streak >= overBudgetEscalateAfter || attempt > policy.Retries {
				return failedResult(res, overBudgetNote(note, streak, spec.role)), nil
			}
			// One expensive session can be bad luck, so it is retried like
			// an infrastructure failure — with the role's fallback model
			// when scheduler.retry_with_fallback is on, which is usually the
			// cheaper one.
			if err := sleepCtx(ctx, policy.Delay); err != nil {
				return failedResult(res, note), err
			}
			continue
		}
		if s.cfg.Scheduler.MaxCostPerSession > 0 {
			s.overBudgetStreak(budgetKey(spec), false)
		}
		kind := classifyFailure(res)
		if kind != failureInfra || attempt > policy.Retries {
			return res, nil
		}
		s.log.Warn("session failed; retrying",
			"role", spec.role, "session", try.name, "attempt", attempt,
			"kind", kind.String(), "reason", infraReason(res), "in", policy.Delay)
		if err := sleepCtx(ctx, policy.Delay); err != nil {
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
	if classifyFailure(res) != failureInfra {
		return fmt.Sprintf("The %s session ended with `%s`: %s", roleTitle(role), status, note)
	}
	attempts := s.cfg.Retry().Retries + 1
	if attempts == 1 {
		return fmt.Sprintf("The %s session failed for infrastructure reasons (%s): %s", roleTitle(role), infraReason(res), note)
	}
	return fmt.Sprintf("The %s session failed %d times for infrastructure reasons (%s): %s", roleTitle(role), attempts, infraReason(res), note)
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
