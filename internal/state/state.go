// Package state manages the bees state directory: role notes, scheduler
// bookkeeping and the status file read by `bees status`.
//
// Layout of <state_dir>:
//
//	mail/<role>/*.json   local mailbox (see package mail)
//	notes/<role>.md      per-role notes, the roles' only long-term memory
//	notes/archive/       notes files replaced by `bees notes reset`
//	sessions/<id>/       one directory per claude session (prompts, transcript, result)
//	issues/<n>.json      per-issue bookkeeping (review round, PR number, the
//	                     developer worker's stage, its running session and,
//	                     once the factory gives up, why it did)
//	<role>.json          per-role bookkeeping (last run, session counters);
//	                     every role has one, including developer and reviewer
//	status.json          live scheduler status
//	ledger.jsonl         one JSON line per finished session (`bees cost`)
//	bees.log             scheduler log (JSON, rotated: bees.log.1, bees.log.2)
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Store is a state directory.
type Store struct{ Dir string }

// New returns a store rooted at dir.
func New(dir string) *Store { return &Store{Dir: dir} }

// Init creates the directory layout.
func (s *Store) Init() error {
	for _, d := range []string{"", "mail", "notes", "sessions", "issues"} {
		if err := os.MkdirAll(filepath.Join(s.Dir, d), 0o755); err != nil {
			return err
		}
	}
	readme := filepath.Join(s.Dir, "README.md")
	if _, err := os.Stat(readme); os.IsNotExist(err) {
		_ = os.WriteFile(readme, []byte(readmeText), 0o644)
	}
	return nil
}

const readmeText = `# busybees state directory

This directory is managed by ` + "`bees`" + `. It holds:

- mail/      the local mailbox roles use to talk to each other
- notes/     each role's notes file (their only memory between sessions),
             with archive/ holding the ones ` + "`bees notes reset`" + ` replaced
- sessions/  prompts, transcripts and results of every Claude Code session
- issues/    per-issue bookkeeping (review rounds, the developer worker's stage,
             and why the factory gave an issue up)
- status.json live scheduler status (` + "`bees status`" + `)
- ledger.jsonl one line per finished session: turns, cost and outcome (` + "`bees cost`" + `)
- bees.log    every scheduler log record as JSON, rotated at 10 MiB

You can safely delete sessions/ to reclaim space. Steering a role is a matter
of editing its notes file: ` + "`bees notes edit <role>`" + `, or by hand.
`

// MailDir returns the mailbox directory.
func (s *Store) MailDir() string { return filepath.Join(s.Dir, "mail") }

// SessionsDir returns the sessions directory.
func (s *Store) SessionsDir() string { return filepath.Join(s.Dir, "sessions") }

// NotesPath returns the notes file for a role.
func (s *Store) NotesPath(role string) string {
	return filepath.Join(s.Dir, "notes", role+".md")
}

// ReadNotes returns a role's notes ("" when none exist yet).
func (s *Store) ReadNotes(role string) (string, error) {
	b, err := os.ReadFile(s.NotesPath(role))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// EnsureNotes creates the notes file so sessions can edit it in place. A
// fresh file already carries the section headings roles are asked to
// consolidate their notes into, so the structure exists from the first run.
func (s *Store) EnsureNotes(role string) error {
	p := s.NotesPath(role)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(NotesSkeleton(role)), 0o644)
}

// NotesSections are the headings every notes file is organised under.
// Anything that does not fit goes under a heading of the role's choosing.
var NotesSections = []string{"Project facts", "Conventions", "Decisions", "Open questions"}

// NotesSkeleton returns the contents of a fresh notes file for a role.
func NotesSkeleton(role string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s notes\n", role)
	for _, h := range NotesSections {
		fmt.Fprintf(&b, "\n## %s\n", h)
	}
	return b.String()
}

// IssueState is per-issue bookkeeping. Two writers share the file and every
// field below says which one owns it: the developer worker
// (scheduler.workIssue) holds one IssueState for the whole life of an issue
// and writes it back with SaveIssue, while the scheduler's polling path
// writes single fields through the owner methods on Store (SetIssueSession,
// AddIssueCost, SetHumanSeenAt, SetIssueHumanSeenAt, SetConflictNotifiedSHA,
// SetProposal, SetOpenChildren), each of which reads the file, changes its own
// fields and writes it back. SaveIssue carries every field it does not own
// over from the file, so a worker's copy — loaded when it started, and by then
// stale — cannot erase what the polling path recorded while it ran.
type IssueState struct {
	// Number is the issue these fields belong to and UpdatedAt when the file
	// was last written; every writer sets both.
	Number int `json:"number"`
	// Round is the review round the developer worker is on, PR the pull
	// request it opened, Branch the branch it works on and CheckFixRounds
	// how many reviewer-diagnoses/developer-fixes iterations failing
	// required checks have cost. They are the worker's own bookkeeping,
	// written through SaveIssue.
	Round          int    `json:"round"`
	PR             int    `json:"pr,omitempty"`
	Branch         string `json:"branch,omitempty"`
	CheckFixRounds int    `json:"check_fix_rounds,omitempty"`
	// WorkerStage is the stage the developer worker (scheduler.workIssue) was
	// in — "develop", "prereview", "review" or "checks" — AfterDevelop the
	// stage its next developer session leads back to, and PreReviewDone
	// whether the pre-review checks have already been read for this pull
	// request. They are the worker's loop state, written on every transition,
	// so a scheduler killed mid-flight comes back to the stage it was in
	// instead of re-deriving one from the issue's workflow label: a label says
	// an issue is in review, not whether its review has already happened.
	// The label stays the human-facing truth all the same — a remembered stage
	// it contradicts is dropped, and so are AfterDevelop and PreReviewDone
	// once the labels say the pull request they belong to is no longer being
	// reviewed. The exception is the developer round the post-approval checks
	// send back, which is recorded under bees:approved before the develop
	// stage can relabel the issue: it keeps the AfterDevelop that names the
	// gate it returns to, and only PreReviewDone goes. All of them belong to
	// the pull request PR names, so a record left for any other one — or
	// written before a number was known — is dropped too
	// (scheduler.resumeStage).
	//
	// These are the developer worker's stages, not roles.reviewer.stages,
	// which are sections of one reviewer session's prompt. Like the fields
	// above they belong to the worker and are written through SaveIssue.
	WorkerStage   string `json:"worker_stage,omitempty"`
	AfterDevelop  string `json:"after_develop,omitempty"`
	PreReviewDone bool   `json:"pre_review_done,omitempty"`
	// Session is the session the scheduler last started for this issue,
	// recorded before it runs and cleared when it ends. A record left
	// behind is what says a scheduler was killed while a session ran: the
	// directory it names holds a transcript no result file ever closed, and
	// the branch may carry the partial work that session left. It sits with
	// the worker's stage rather than in a file of its own because both
	// answer the same question — where did the last attempt get to
	// (scheduler.takeInterrupted). SetIssueSession is its only writer:
	// SaveIssue carries it over from the file, like the cost totals, so a
	// worker holding an IssueState across several sessions cannot write
	// back a record that has since been cleared.
	Session *SessionRun `json:"session,omitempty"`
	// HumanSeenAt is the timestamp of the latest human PR activity already
	// delivered to the developer. It is owned by SetHumanSeenAt
	// (scheduler.deliverHumanFeedback): delivering the same review twice is
	// exactly what it exists to prevent, so SaveIssue carries it over.
	HumanSeenAt time.Time `json:"human_seen_at,omitempty"`
	// IssueHumanSeenAt is the timestamp of the latest human *issue* comment
	// already delivered. It is separate from HumanSeenAt, which is the pull
	// request's: one stream's clock must not suppress the other's. It is
	// owned by SetIssueHumanSeenAt (scheduler.deliverHumanIssueComments) and
	// carried over by SaveIssue for the same reason HumanSeenAt is. A zero
	// value means the issue has not yet been seen in triage or in a flight
	// state; the first pass that sees it in one records the poll time and
	// delivers nothing, so an upgrade does not replay the whole comment
	// history. Triage records the time on every pass and never delivers,
	// which is what leaves a clock behind for an issue blocked out of it.
	IssueHumanSeenAt time.Time `json:"issue_human_seen_at,omitempty"`
	// ConflictNotifiedSHA is the PR head commit the developer was last told
	// to bring up to date with the default branch; the same head is never
	// mailed about twice. It is owned by SetConflictNotifiedSHA
	// (scheduler.checkPRs) and carried over by SaveIssue for the same reason.
	ConflictNotifiedSHA string `json:"conflict_notified_sha,omitempty"`
	// Cost is what every session run for this issue has cost so far, in USD,
	// and Sessions how many sessions that was. Both are owned by
	// AddIssueCost: SaveIssue carries them over from the file, so a caller
	// holding an IssueState across several sessions cannot write back a
	// stale total. scheduler.max_cost_per_issue is spent against them.
	Cost     float64 `json:"cost,omitempty"`
	Sessions int     `json:"sessions,omitempty"`
	// Proposal is whether the issue carried bees:proposal at the last
	// observation, and ProposalApprovedAt when the scheduler saw a person
	// remove it. Approval is a label edit and leaves no comment, so it is
	// remembered here: nothing else would tell the product manager that a
	// feature it proposed may now be broken into work items. Both are owned
	// by SetProposal (scheduler.observeProposals) and carried over by
	// SaveIssue.
	Proposal           bool      `json:"proposal,omitempty"`
	ProposalApprovedAt time.Time `json:"proposal_approved_at,omitempty"`
	// OpenChildren are the open sub-issues a feature had when the product
	// manager last ran, and CompleteReportedAt when the scheduler last told
	// it that all of them had closed. Together they are how the scheduler
	// notices a finished feature without asking GitHub on the polling path:
	// GitHub's sub-issue summary carries counts only, so the numbers are
	// remembered here and checked against the issues the poll still finds
	// open. An empty or incomplete lookup is never recorded over a remembered
	// one — no open children is the very state the check is about, and a
	// truncated set would look like children that closed — and a set that
	// changes clears the marker, so a feature that gains a sub-issue after
	// being reported complete can be reported again. Both are owned by
	// SetOpenChildren (scheduler.recordFeatureProgress), which is where those
	// rules live, and carried over by SaveIssue.
	OpenChildren       []int     `json:"open_children,omitempty"`
	CompleteReportedAt time.Time `json:"complete_reported_at,omitempty"`
	// Escalation is why the factory gave this issue up to a person and
	// EscalatedAt when it did. The bees:needs-human label says that it
	// happened; the reason is said once, in a GitHub comment and in the log,
	// and neither is readable from the poll — so it is recorded here, where a
	// view can show the person the factory is waiting for what it is waiting
	// about without asking GitHub for it. An issue a person labelled by hand,
	// or one escalated before this state directory existed, has neither.
	// Both are owned by SetEscalation (scheduler.escalate) and carried over
	// by SaveIssue.
	Escalation  string    `json:"escalation,omitempty"`
	EscalatedAt time.Time `json:"escalated_at,omitempty"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// SessionRun is one session the scheduler started for an issue: who ran it,
// what it was called and the directory holding its prompts, transcript and,
// once it ends, its result.
type SessionRun struct {
	Role      string    `json:"role"`
	Name      string    `json:"name"`
	Dir       string    `json:"dir"`
	StartedAt time.Time `json:"started_at"`
}

// Issue loads bookkeeping for an issue (zero value when none).
func (s *Store) Issue(n int) (IssueState, error) {
	var is IssueState
	err := s.readJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(n)+".json"), &is)
	if is.Number == 0 {
		is.Number = n
	}
	return is, err
}

// SaveIssue saves the developer worker's bookkeeping for an issue: the review
// round, its pull request and branch, the check-fix rounds and the worker's
// stage. Every other field is taken from the file rather than from is,
// because a developer worker holds one IssueState for the whole life of an
// issue while the scheduler's polling path keeps writing to the same file:
// saving the worker's copy wholesale would write back the cost totals as they
// were when it started, resurrect a session record that has since been
// cleared, and undo the delivered-feedback, notified-head, proposal and
// feature-completeness bookkeeping the polling path recorded in the meantime.
// Each of those fields has an owner method that reads, changes and writes the
// file (AddIssueCost, SetIssueSession, SetHumanSeenAt,
// SetIssueHumanSeenAt, SetConflictNotifiedSHA, SetProposal, SetOpenChildren);
// this is the other half of that rule.
func (s *Store) SaveIssue(is IssueState) error {
	if cur, err := s.Issue(is.Number); err == nil {
		is.Cost, is.Sessions, is.Session = cur.Cost, cur.Sessions, cur.Session
		is.HumanSeenAt, is.ConflictNotifiedSHA = cur.HumanSeenAt, cur.ConflictNotifiedSHA
		is.IssueHumanSeenAt = cur.IssueHumanSeenAt
		is.Proposal, is.ProposalApprovedAt = cur.Proposal, cur.ProposalApprovedAt
		is.OpenChildren, is.CompleteReportedAt = cur.OpenChildren, cur.CompleteReportedAt
		is.Escalation, is.EscalatedAt = cur.Escalation, cur.EscalatedAt
	}
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(is.Number)+".json"), is)
}

// SetIssueSession records the session running for an issue, or clears it with
// nil. It is the only writer of IssueState.Session: SaveIssue carries the
// field over from the file, so a worker's own saves can neither clear a
// record a session has just written nor write back one it has cleared.
func (s *Store) SetIssueSession(n int, run *SessionRun) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Number, is.Session = n, run
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(n)+".json"), is)
}

// SetEscalation records why the factory gave an issue up to a person, and
// when. It is the only writer of IssueState.Escalation and EscalatedAt:
// SaveIssue carries them over from the file, so a developer worker still
// holding an IssueState for the issue it was escalated over cannot erase the
// one record of the reason.
func (s *Store) SetEscalation(n int, reason string, at time.Time) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Number, is.Escalation, is.EscalatedAt = n, reason, at
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(n)+".json"), is)
}

// SetHumanSeenAt records how far the scheduler has read a pull request's
// human reviews and comments. It is the only writer of IssueState.HumanSeenAt:
// SaveIssue carries the field over from the file, so a developer worker
// holding an IssueState across several sessions cannot write back an older
// mark and have the same feedback delivered to it twice.
func (s *Store) SetHumanSeenAt(n int, t time.Time) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Number, is.HumanSeenAt = n, t
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(n)+".json"), is)
}

// SetIssueHumanSeenAt records how far the scheduler has read an issue's own
// human comments. It is the only writer of IssueState.IssueHumanSeenAt, and
// deliberately separate from SetHumanSeenAt: the pull request and the issue
// are two comment streams, and advancing one clock past the other would drop
// the comments it has not delivered yet.
func (s *Store) SetIssueHumanSeenAt(n int, t time.Time) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Number, is.IssueHumanSeenAt = n, t
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(n)+".json"), is)
}

// SetConflictNotifiedSHA records the pull request head the developer was told
// to bring up to date. It is the only writer of
// IssueState.ConflictNotifiedSHA: SaveIssue carries the field over from the
// file, so a developer worker's own saves cannot forget the head and have the
// scheduler mail about it again.
func (s *Store) SetConflictNotifiedSHA(n int, sha string) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Number, is.ConflictNotifiedSHA = n, sha
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(n)+".json"), is)
}

// SetProposal records whether a feature carries the proposal label, and with
// a non-zero approvedAt that a person has just removed it. A zero approvedAt
// leaves any approval already recorded alone: an approval is only ever
// observed once. The two fields are written together because the second is
// meaningless without the first, and SetProposal is their only writer —
// SaveIssue carries them over from the file, so a developer worker cannot
// forget an approval and leave the product manager unaware of it.
func (s *Store) SetProposal(n int, proposal bool, approvedAt time.Time) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Number, is.Proposal = n, proposal
	if !approvedAt.IsZero() {
		is.ProposalApprovedAt = approvedAt
	}
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(n)+".json"), is)
}

// SetOpenChildren records a feature's open sub-issues, and with a non-zero
// reportedAt that the scheduler has just presented the feature as complete.
// A nil children leaves the remembered set alone, which is how a caller says
// its lookup was empty or incomplete and must not be recorded; a set that
// differs from the remembered one clears the report marker, so a feature that
// gains a sub-issue can be reported complete again. Nothing is written when
// neither applies.
//
// The two fields are written together because neither means anything without
// the other, and SetOpenChildren is their only writer: SaveIssue carries them
// over from the file, so a developer worker cannot report a finished feature
// twice by writing back a set of children that has since been reported on.
func (s *Store) SetOpenChildren(n int, children []int, reportedAt time.Time) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	changed := children != nil && !slices.Equal(is.OpenChildren, children)
	if !changed && reportedAt.IsZero() {
		return nil
	}
	if changed {
		is.OpenChildren, is.CompleteReportedAt = children, time.Time{}
	}
	if !reportedAt.IsZero() {
		is.CompleteReportedAt = reportedAt
	}
	is.Number = n
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(n)+".json"), is)
}

// AddIssueCost adds one finished session to an issue's running total and
// returns the totals after it. It is the only writer of the cost fields.
func (s *Store) AddIssueCost(number int, cost float64) (IssueState, error) {
	is, err := s.Issue(number)
	if err != nil {
		return is, err
	}
	is.Cost += cost
	is.Sessions++
	is.UpdatedAt = time.Now().UTC()
	return is, s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(is.Number)+".json"), is)
}

// SetIssueCost replaces an issue's running totals, which is how a total is
// seeded from the ledger for an issue whose bookkeeping predates budgets.
func (s *Store) SetIssueCost(number int, cost float64, sessions int) (IssueState, error) {
	is, err := s.Issue(number)
	if err != nil {
		return is, err
	}
	is.Cost, is.Sessions = cost, sessions
	is.UpdatedAt = time.Now().UTC()
	return is, s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(is.Number)+".json"), is)
}

// RoleState is per-role bookkeeping. Singleton roles use it to remember
// when they last ran; every role uses the session counters to decide when it
// is next asked to consolidate its notes.
type RoleState struct {
	LastRun time.Time `json:"last_run"`
	// LastCheck is when the scheduler last looked for work for the role
	// (used to rate-limit the QA merged-PR query).
	LastCheck time.Time `json:"last_check,omitempty"`
	// Sessions counts every session run for the role, of whatever kind.
	Sessions int `json:"sessions,omitempty"`
	// LastConsolidated is the value of Sessions when the role was last
	// asked to consolidate its notes file.
	LastConsolidated int `json:"last_consolidated,omitempty"`
}

// Role loads bookkeeping for a role.
func (s *Store) Role(role string) (RoleState, error) {
	var rs RoleState
	return rs, s.readJSON(filepath.Join(s.Dir, role+".json"), &rs)
}

// SaveRole stores bookkeeping for a role.
func (s *Store) SaveRole(role string, rs RoleState) error {
	return s.writeJSON(filepath.Join(s.Dir, role+".json"), rs)
}

// Worker describes a running developer worker.
type Worker struct {
	Name  string `json:"name"`
	Issue int    `json:"issue"`
	// Size is the issue's size label ("xs".."xl"), recorded when the worker
	// starts. It is what scheduler.max_large_in_flight counts.
	Size  string `json:"size,omitempty"`
	Stage string `json:"stage"`
	Round int    `json:"round"`
	// Attempt is the 1-based attempt of the running session; > 1 means the
	// previous attempt failed for infrastructure reasons and was retried.
	Attempt int `json:"attempt,omitempty"`
	// Resumed marks a worker that took over from a session interrupted by a
	// scheduler that was killed, rather than starting fresh: its branch may
	// carry work nobody reported.
	Resumed bool      `json:"resumed,omitempty"`
	Since   time.Time `json:"since"`
}

// Status is the live scheduler status.
type Status struct {
	UpdatedAt time.Time `json:"updated_at"`
	PID       int       `json:"pid"`
	LastPoll  time.Time `json:"last_poll"`
	// NextPoll is when the scheduler next polls GitHub. Between polls it
	// still runs local passes: every scheduler.poll_interval, and whenever a
	// session finishes.
	NextPoll time.Time `json:"next_poll,omitempty"`
	// Version is the build the running scheduler was started from, as
	// `bees version` reports it ("v0.2.0", "dev (abc123def456 modified)",
	// or "dev"). Empty in a status.json written by a bees older than the
	// field, and in one written by a scheduler given no version.
	Version string `json:"version,omitempty"`
	// Revision is the untruncated commit that build came from, empty when
	// the binary carries no VCS stamps. Version truncates it for display;
	// this is the raw value, so it can be compared against the repository.
	//
	// Both matter because the role prompts are compiled into the binary: a
	// running factory serves the prompts of the build it was started from,
	// whatever has since been merged.
	Revision string `json:"revision,omitempty"`
	// InWorkHours is nil when scheduler.work_hours is not configured.
	InWorkHours *bool             `json:"in_work_hours,omitempty"`
	Workers     []Worker          `json:"workers"`
	Singletons  map[string]string `json:"singletons"` // role -> "idle"/"running"
	Queues      map[string]int    `json:"queues"`
	// ReadySizes counts the ready queue by size ("xs", "s", "m", "l",
	// "xl"); issues without a size label are counted under "".
	ReadySizes map[string]int `json:"ready_sizes,omitempty"`
	// Priority lists the ready issues carrying bees:priority, smallest
	// number first: the queue a person told the factory to build next.
	Priority []int `json:"priority,omitempty"`
	// BudgetPaused is true while the rolling 24h spend has reached
	// scheduler.max_cost_per_day and no new session is being dispatched.
	BudgetPaused bool `json:"budget_paused,omitempty"`
	// DaySpendUSD is that rolling 24h spend, and DayBudgetUSD the budget it
	// is measured against (0 when no daily budget is configured).
	DaySpendUSD  float64 `json:"day_spend_usd,omitempty"`
	DayBudgetUSD float64 `json:"day_budget_usd,omitempty"`
	// LimitPausedUntil is when dispatch resumes after a session hit the
	// account-wide claude session limit. Zero when no such pause is in
	// force; a time in the past is one nothing has looked at since it
	// lifted.
	LimitPausedUntil time.Time `json:"limit_paused_until,omitempty"`
	// WaitingOnDeps maps a ready issue to the blockers it declares that are
	// still open, so `bees status` can explain why it is not being built.
	WaitingOnDeps map[int][]int `json:"waiting_on_deps,omitempty"`
	// NeedsHuman lists the issues the factory has given up on and is waiting
	// for a person over, first escalated first. Queues counts them; this is
	// what they are, so a view can say which issue and why without asking
	// GitHub a second time.
	NeedsHuman []Escalated `json:"needs_human,omitempty"`
	// Approved lists the pull requests the reviewer approved and that are
	// waiting for a person to merge, oldest first. Like NeedsHuman it is the
	// detail behind a queue count, built from the poll the count came from.
	Approved  []ApprovedPR `json:"approved,omitempty"`
	LastError string       `json:"last_error,omitempty"`
	// Degraded lists the factory operations that are failing right now, one
	// entry per operation, sorted by name. Absent when the last pass was
	// clean.
	Degraded []OpFailure `json:"degraded,omitempty"`
}

// Escalated is one issue the factory handed to a person: which issue, what
// it is called, why the factory gave it up and when. The reason is what
// scheduler.escalate recorded (IssueState.Escalation); it is empty for an
// issue a person labelled by hand, and for one escalated before this state
// directory existed.
type Escalated struct {
	Issue  int    `json:"issue"`
	Title  string `json:"title"`
	Reason string `json:"reason,omitempty"`
	// Since is when the factory escalated the issue, zero when it did not
	// record one.
	Since time.Time `json:"since,omitempty"`
}

// ApprovedPR is one pull request the reviewer approved and left for a person
// to merge: the pull request, the issue it closes and when it was opened.
//
// Opened, not approved: the approval is a label edit and the poll carries no
// time for it, and the number a person waiting to merge is really being told
// is how long the change has been in flight.
type ApprovedPR struct {
	PR    int       `json:"pr"`
	Issue int       `json:"issue"`
	Title string    `json:"title"`
	Since time.Time `json:"since,omitempty"`
}

// OpFailure is the current failure streak of one named factory operation
// (an assignment, a label edit, the poll itself): how many times in a row it
// has failed, since when, and what it last said.
type OpFailure struct {
	Op    string `json:"op"`
	Count int    `json:"count"`
	// First and Last are the ends of the streak: the failure that started
	// it and the most recent one.
	First time.Time `json:"first"`
	Last  time.Time `json:"last"`
	// LastError is the most recent error, on one line and capped.
	LastError string `json:"last_error,omitempty"`
	// Escalated records that this streak already produced its one summary
	// line, so it is not repeated on every pass.
	Escalated bool `json:"escalated,omitempty"`
}

// SaveStatus writes status.json.
func (s *Store) SaveStatus(st Status) error {
	st.UpdatedAt = time.Now().UTC()
	st.PID = os.Getpid()
	return s.writeJSON(filepath.Join(s.Dir, "status.json"), st)
}

// LoadStatus reads status.json.
func (s *Store) LoadStatus() (Status, error) {
	var st Status
	return st, s.readJSON(filepath.Join(s.Dir, "status.json"), &st)
}

func (s *Store) readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

func (s *Store) writeJSON(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	// A temp name derived from the destination is shared by every writer of
	// that path: two goroutines saving the same issue would truncate and
	// write one file, and one could rename what the other is still writing,
	// leaving JSON no reader ever repairs. A unique temp file per call makes
	// concurrent saves last-write-wins instead.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	// A no-op after a successful rename; on every error path it is the
	// cleanup. Its own error is deliberately ignored.
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(b); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// os.CreateTemp creates the file 0600; state files are 0644.
	if err := os.Chmod(tmp, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
