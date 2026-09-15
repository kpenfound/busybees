// Package state manages the bees state directory: role notes, scheduler
// bookkeeping and the status file read by `bees status`.
//
// Layout of <state_dir>:
//
//	mail/<role>/*.json   local mailbox (see package mail)
//	feedback/<id>.json   drafts of factory-error reports written by
//	                     report_factory_error (see package feedback)
//	notes/<role>.md      per-role notes, the roles' only long-term memory
//	notes/archive/       notes files replaced by `bees notes reset`
//	sessions/<id>/       one directory per session (prompts, transcript, result)
//	reviews/<owner>/<name>/<pr>/<started>/
//	                     one review artifact per review the reviewer ran on a
//	                     pull request (see package review: brief.json,
//	                     angles/, findings.json)
//	issues/work-<hash>.json
//	                     per-work bookkeeping (review round, caller tags,
//	                     developer worker's stage, its running session and,
//	                     once the factory gives up, why it did)
//	<role>.json          per-role bookkeeping (last run, session counters);
//	                     every role has one, including developer and reviewer
//	status.json          live scheduler status
//	ledger.jsonl         one JSON line per finished session (`bees cost`),
//	                     trimmed to scheduler.retention_period, and always
//	                     keeping at least the last 24 hours
//	schema.json          runtime schema version, written after migration
//	bees.log             scheduler log (JSON, rotated: bees.log.1, bees.log.2)
package state

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/busybees/core/work"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/statemigrate"
)

// Store is a state directory.
type Store struct {
	Dir string

	// ledgerMu serialises AppendLedger with TrimLedger's rewrite, so a line
	// appended while the ledger is rewritten is never lost.
	ledgerMu sync.Mutex
}

// New returns a store rooted at dir.
func New(dir string) *Store { return &Store{Dir: dir} }

// Migrate ensures every access uses the current on-disk schema.
func (s *Store) Migrate() error { return statemigrate.Ensure(s.Dir) }

// MigrateExisting leaves a missing directory absent on read-only paths.
func (s *Store) MigrateExisting() error {
	if _, err := os.Stat(s.Dir); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return s.Migrate()
}

// Init creates the directory layout.
func (s *Store) Init() error {
	if err := s.Migrate(); err != nil {
		return err
	}
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
- feedback/  drafts of factory-error reports a role recorded with
             report_factory_error, waiting to be filed upstream
- notes/     each role's notes file (their only memory between sessions, unless
             bees.toml's notes.backend says otherwise), with archive/ holding
             the ones ` + "`bees notes reset`" + ` replaced
- sessions/  prompts, transcripts and results of every session
- reviews/   the reviewer's review of each pull request: the brief, what each
             angle found and the judge's list
- schema.json runtime schema version, published after migration
- issues/    bookkeeping keyed by opaque work identity (review rounds, worker
             stage, and why the factory gave an issue up)
- status.json live scheduler status (` + "`bees status`" + `)
- ledger.jsonl one line per finished session: turns, cost and outcome (` + "`bees cost`" + `),
             kept for scheduler.retention_period, and always at least 24 hours
- bees.log    every scheduler log record as JSON, rotated at 10 MiB

You can safely delete sessions/ and reviews/ to reclaim space. Steering a role is a matter
of editing its notes file: ` + "`bees notes edit <role>`" + `, or by hand.
`

// MailDir returns the mailbox directory.
func (s *Store) MailDir() string { return filepath.Join(s.Dir, "mail") }

// FeedbackDir returns the directory holding factory-error drafts (see
// package feedback).
func (s *Store) FeedbackDir() string { return filepath.Join(s.Dir, "feedback") }

// SessionsDir returns the sessions directory.
func (s *Store) SessionsDir() string { return filepath.Join(s.Dir, "sessions") }

// ReviewsDir returns the directory the reviewer's review artifacts are
// written under (internal/review's ArtifactDir), one per review of a pull
// request.
func (s *Store) ReviewsDir() string { return filepath.Join(s.Dir, "reviews") }

// NotesPath returns the notes file for a role.
func (s *Store) NotesPath(role string) string {
	return filepath.Join(s.Dir, "notes", role+".md")
}

// ReadNotes returns a role's notes ("" when none exist yet).
func (s *Store) ReadNotes(role string) (string, error) {
	if err := s.MigrateExisting(); err != nil {
		return "", err
	}
	b, err := os.ReadFile(s.NotesPath(role))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return string(b), err
}

// EnsureNotes creates the notes file when there is none, so a role's first
// notes_read returns the section headings roles are asked to consolidate
// their notes into, and the structure exists from the first run.
func (s *Store) EnsureNotes(role string) error {
	if err := s.Migrate(); err != nil {
		return err
	}
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

// WorkState is per-work bookkeeping with busybees workflow metadata. Two
// writers share the file and every field below says which one owns it: the developer worker
// (scheduler.workIssue) holds one WorkState for the whole life of an issue
// and writes it back with SaveIssue, while the scheduler's polling path
// writes single fields through the owner methods on Store (SetIssueSession,
// AddIssueCost, SetHumanSeenAt, SetIssueHumanSeenAt, SetConflictNotifiedSHA,
// SetReviewedSHA, SetProposal, SetOpenChildren), each of which reads the file,
// changes its own fields and writes it back. SaveIssue carries every field it
// does not own over from the file, so a worker's copy — loaded when it
// started, and by then stale — cannot erase what the polling path recorded
// while it ran.
type WorkState struct {
	// Work identifies the subject and carries caller-defined routing tags.
	Work work.Ref `json:"work"`
	// Round is the review round, Branch the branch it works on and CheckFixRounds
	// how many reviewer-diagnoses/developer-fixes iterations failing
	// required checks have cost. They are the worker's own bookkeeping,
	// written through SaveIssue.
	Round          int    `json:"round"`
	Branch         string `json:"branch,omitempty"`
	CheckFixRounds int    `json:"check_fix_rounds,omitempty"`
	// WorkerStage is the stage the developer worker (scheduler.workIssue) was
	// in — "develop", "fan-out", "assembler", "prereview", "review",
	// "stack-wait" or "checks" — AfterDevelop the stage its next developer
	// session leads back to, and PreReviewDone
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
	// the pull request the Work tags name, so a record left for any other one — or
	// written before a number was known — is dropped too
	// (scheduler.resumeStage).
	//
	// These are the developer worker's stages, not roles.reviewer.stages,
	// which are sections of one reviewer session's prompt. Like the fields
	// above they belong to the worker and are written through SaveIssue.
	WorkerStage   string `json:"worker_stage,omitempty"`
	AfterDevelop  string `json:"after_develop,omitempty"`
	PreReviewDone bool   `json:"pre_review_done,omitempty"`
	// ReviewArtifact is the artifact directory of the full review the worker
	// ran on its pull request, and ReviewedHead the head commit that review
	// read. A later review round verifies that review's findings against the
	// head as it stands instead of reviewing the change again
	// (scheduler.verifyReview). They are the worker's, written through
	// SaveIssue, and belong to the pull request the artifact directory is
	// named for: one recorded for another pull request is not verified.
	ReviewArtifact string `json:"review_artifact,omitempty"`
	ReviewedHead   string `json:"reviewed_head,omitempty"`
	// Session is the session the scheduler last started for this issue,
	// recorded before it runs and cleared when it ends. A record left
	// behind is what says a session never finished — a scheduler dying
	// while it ran, or a hard stop. The directory it names holds a
	// transcript no result file ever closed, and the branch may carry the
	// partial work that session left. It sits with the worker's stage
	// rather than in a file of its own because both answer the same
	// question — where did the last attempt get to
	// (scheduler.takeInterrupted). SetIssueSession is its only writer:
	// SaveIssue carries it over from the file, like the cost totals, so a
	// worker holding an WorkState across several sessions cannot write
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
	// value means the factory has not seen the issue at all yet; the first
	// pass that sees it records the poll time and delivers nothing, so an
	// upgrade does not replay the whole comment history. Triage, ready and
	// the product manager's feature and feedback issues record the time on
	// every pass and deliver one thing only, a comment that @-mentions the
	// login the factory acts as (scheduler.deliverMention). That refresh is
	// also what leaves a clock behind for an issue blocked out of triage or
	// waiting in the ready queue.
	IssueHumanSeenAt time.Time `json:"issue_human_seen_at,omitempty"`
	// ConflictNotifiedSHA is the PR head commit the developer was last told
	// to bring up to date with the default branch; the same head is never
	// mailed about twice. It is owned by SetConflictNotifiedSHA
	// (scheduler.checkPRs) and carried over by SaveIssue for the same reason.
	ConflictNotifiedSHA string `json:"conflict_notified_sha,omitempty"`
	// ReviewedSHA is the head commit of the pull request the last assignment-
	// triggered review looked at; a new push earns a new review and a restart
	// does not re-review the same head. Owned by SetReviewedSHA
	// (scheduler.dispatchRequestedReviews) and carried over by SaveIssue.
	ReviewedSHA string `json:"reviewed_sha,omitempty"`
	// Cost is what every session run for this issue has cost so far, in USD,
	// and Sessions how many sessions that was. Both are owned by
	// AddIssueCost: SaveIssue carries them over from the file, so a caller
	// holding an WorkState across several sessions cannot write back a
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
	OpenChildren       []work.Key `json:"open_children,omitempty"`
	CompleteReportedAt time.Time  `json:"complete_reported_at,omitempty"`
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

// SessionRun is one session the scheduler started for a work item, including
// PR-only requested reviews: who ran it, what it was called and the directory
// holding its prompts, transcript and, once it ends, its result.
type SessionRun struct {
	Role      string    `json:"role"`
	Name      string    `json:"name"`
	Dir       string    `json:"dir"`
	StartedAt time.Time `json:"started_at"`
}

// Issue loads bookkeeping for an issue (zero value when none).
func (s *Store) Issue(n int) (WorkState, error) { return s.Work(ghwork.New(n, 0)) }

// Work loads bookkeeping by opaque key, retaining caller metadata on first use.
func (s *Store) Work(ref work.Ref) (WorkState, error) {
	is := WorkState{Work: ref.Clone()}
	if ref.Key == "" {
		return is, errors.New("work key is required")
	}
	err := s.readJSON(s.WorkPath(ref.Key), &is)
	if err == nil && is.Work.Key != ref.Key {
		return is, fmt.Errorf("bookkeeping key mismatch at %s", s.WorkPath(ref.Key))
	}
	return is, err
}

// WorkPath returns the collision-safe bookkeeping filename for a key.
func (s *Store) WorkPath(key work.Key) string {
	return filepath.Join(s.Dir, "issues", key.Filename())
}

// WorkKeys lists persisted work identities without interpreting their keys.
func (s *Store) WorkKeys() ([]work.Key, error) {
	if err := s.MigrateExisting(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(filepath.Join(s.Dir, "issues"))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var keys []work.Key
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var is WorkState
		if err := s.readJSON(filepath.Join(s.Dir, "issues", e.Name()), &is); err != nil {
			return nil, err
		}
		if is.Work.Key == "" || e.Name() != is.Work.Key.Filename() {
			return nil, fmt.Errorf("invalid work bookkeeping filename %s", e.Name())
		}
		keys = append(keys, is.Work.Key)
	}
	slices.Sort(keys)
	return keys, nil
}

// IssueNumbers returns every issue that has bookkeeping, smallest first.
func (s *Store) IssueNumbers() ([]int, error) {
	keys, err := s.WorkKeys()
	if err != nil {
		return nil, err
	}
	var out []int
	for _, key := range keys {
		is, err := s.Work(work.Ref{Key: key})
		if err != nil {
			return nil, err
		}
		if n := ghwork.Issue(is.Work); n > 0 {
			out = append(out, n)
		}
	}
	slices.Sort(out)
	return out, nil
}

// RemoveIssue deletes an issue's bookkeeping. An issue that has none is not
// an error.
func (s *Store) RemoveIssue(n int) error { return s.RemoveWork(ghwork.IssueKey(n)) }

func (s *Store) RemoveWork(key work.Key) error {
	if err := s.MigrateExisting(); err != nil {
		return err
	}
	err := os.Remove(s.WorkPath(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// SaveIssue saves the developer worker's bookkeeping for an issue: the review
// round, its pull request and branch, the check-fix rounds and the worker's
// stage. Every other field is taken from the file rather than from is,
// because a developer worker holds one WorkState for the whole life of an
// issue while the scheduler's polling path keeps writing to the same file:
// saving the worker's copy wholesale would write back the cost totals as they
// were when it started, resurrect a session record that has since been
// cleared, and undo the delivered-feedback, notified-head, proposal and
// feature-completeness bookkeeping the polling path recorded in the meantime.
// Each of those fields has an owner method that reads, changes and writes the
// file (AddIssueCost, SetIssueSession, SetHumanSeenAt,
// SetIssueHumanSeenAt, SetConflictNotifiedSHA, SetReviewedSHA, SetProposal,
// SetOpenChildren); this is the other half of that rule.
func (s *Store) SaveIssue(is WorkState) error { return s.SaveWork(is) }

// SaveWork saves worker-owned fields and preserves the other writers' fields.
func (s *Store) SaveWork(is WorkState) error {
	if is.Work.Key == "" {
		return errors.New("work key is required")
	}
	cur, err := s.Work(is.Work)
	if err != nil {
		return err
	}
	is.Cost, is.Sessions, is.Session = cur.Cost, cur.Sessions, cur.Session
	is.HumanSeenAt, is.ConflictNotifiedSHA = cur.HumanSeenAt, cur.ConflictNotifiedSHA
	is.ReviewedSHA = cur.ReviewedSHA
	is.IssueHumanSeenAt = cur.IssueHumanSeenAt
	is.Proposal, is.ProposalApprovedAt = cur.Proposal, cur.ProposalApprovedAt
	is.OpenChildren, is.CompleteReportedAt = cur.OpenChildren, cur.CompleteReportedAt
	is.Escalation, is.EscalatedAt = cur.Escalation, cur.EscalatedAt
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(s.WorkPath(is.Work.Key), is)
}

// SetIssueSession records the session running for an issue, or clears it with
// nil. It is the only writer of WorkState.Session: SaveIssue carries the
// field over from the file, so a worker's own saves can neither clear a
// record a session has just written nor write back one it has cleared.
func (s *Store) SetIssueSession(n int, run *SessionRun) error {
	return s.SetWorkSession(ghwork.New(n, 0), run)
}

func (s *Store) SetWorkSession(ref work.Ref, run *SessionRun) error {
	is, err := s.Work(ref)
	if err != nil {
		return err
	}
	is.Session = run
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(s.WorkPath(is.Work.Key), is)
}

// SetEscalation records why the factory gave an issue up to a person, and
// when. It is the only writer of WorkState.Escalation and EscalatedAt:
// SaveIssue carries them over from the file, so a developer worker still
// holding an WorkState for the issue it was escalated over cannot erase the
// one record of the reason.
func (s *Store) SetEscalation(n int, reason string, at time.Time) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Work, is.Escalation, is.EscalatedAt = ghwork.WithIssue(is.Work, n), reason, at
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(s.WorkPath(ghwork.IssueKey(n)), is)
}

// SetHumanSeenAt records how far the scheduler has read a pull request's
// human reviews and comments. It is the only writer of WorkState.HumanSeenAt:
// SaveIssue carries the field over from the file, so a developer worker
// holding an WorkState across several sessions cannot write back an older
// mark and have the same feedback delivered to it twice.
func (s *Store) SetHumanSeenAt(n int, t time.Time) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Work, is.HumanSeenAt = ghwork.WithIssue(is.Work, n), t
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(s.WorkPath(ghwork.IssueKey(n)), is)
}

// SetIssueHumanSeenAt records how far the scheduler has read an issue's own
// human comments. It is the only writer of WorkState.IssueHumanSeenAt, and
// deliberately separate from SetHumanSeenAt: the pull request and the issue
// are two comment streams, and advancing one clock past the other would drop
// the comments it has not delivered yet.
func (s *Store) SetIssueHumanSeenAt(n int, t time.Time) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Work, is.IssueHumanSeenAt = ghwork.WithIssue(is.Work, n), t
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(s.WorkPath(ghwork.IssueKey(n)), is)
}

// SetConflictNotifiedSHA records the pull request head the developer was told
// to bring up to date. It is the only writer of
// WorkState.ConflictNotifiedSHA: SaveIssue carries the field over from the
// file, so a developer worker's own saves cannot forget the head and have the
// scheduler mail about it again.
func (s *Store) SetConflictNotifiedSHA(n int, sha string) error {
	is, err := s.Issue(n)
	if err != nil {
		return err
	}
	is.Work, is.ConflictNotifiedSHA = ghwork.WithIssue(is.Work, n), sha
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(s.WorkPath(ghwork.IssueKey(n)), is)
}

// SetReviewedSHA records the pull request head an assignment-triggered review
// looked at. It is the only writer of WorkState.ReviewedSHA: SaveIssue
// carries the field over from the file, so a developer worker's own saves
// cannot forget the head and have the scheduler review the same head again.
func (s *Store) SetReviewedSHA(n int, sha string) error {
	return s.SetWorkReviewedSHA(ghwork.New(n, 0), sha)
}

func (s *Store) SetWorkReviewedSHA(ref work.Ref, sha string) error {
	is, err := s.Work(ref)
	if err != nil {
		return err
	}
	is.ReviewedSHA = sha
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(s.WorkPath(is.Work.Key), is)
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
	is.Work, is.Proposal = ghwork.WithIssue(is.Work, n), proposal
	if !approvedAt.IsZero() {
		is.ProposalApprovedAt = approvedAt
	}
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(s.WorkPath(ghwork.IssueKey(n)), is)
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
	changed := children != nil && !slices.Equal(is.OpenChildren, ghwork.Keys(children))
	if !changed && reportedAt.IsZero() {
		return nil
	}
	if changed {
		is.OpenChildren, is.CompleteReportedAt = ghwork.Keys(children), time.Time{}
	}
	if !reportedAt.IsZero() {
		is.CompleteReportedAt = reportedAt
	}
	is.Work = ghwork.WithIssue(is.Work, n)
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(s.WorkPath(ghwork.IssueKey(n)), is)
}

// AddIssueCost adds one finished session to an issue's running total and
// returns the totals after it. It is the only writer of the cost fields.
func (s *Store) AddIssueCost(number int, cost float64) (WorkState, error) {
	return s.AddWorkCost(ghwork.New(number, 0), cost)
}

func (s *Store) AddWorkCost(ref work.Ref, cost float64) (WorkState, error) {
	is, err := s.Work(ref)
	if err != nil {
		return is, err
	}
	is.Cost += cost
	is.Sessions++
	is.UpdatedAt = time.Now().UTC()
	return is, s.writeJSON(s.WorkPath(is.Work.Key), is)
}

// SetIssueCost replaces an issue's running totals, which is how a total is
// seeded from the ledger for an issue whose bookkeeping predates budgets.
func (s *Store) SetIssueCost(number int, cost float64, sessions int) (WorkState, error) {
	return s.SetWorkCost(ghwork.New(number, 0), cost, sessions)
}

func (s *Store) SetWorkCost(ref work.Ref, cost float64, sessions int) (WorkState, error) {
	is, err := s.Work(ref)
	if err != nil {
		return is, err
	}
	is.Cost, is.Sessions = cost, sessions
	is.UpdatedAt = time.Now().UTC()
	return is, s.writeJSON(s.WorkPath(is.Work.Key), is)
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
	Work work.Ref `json:"work"`
	Name string   `json:"name"`
	// Work identifies the issue, or the pull request for a requested review.
	// Size is the issue's size label ("xs".."xl"), recorded when the worker
	// starts. It is what scheduler.max_large_in_flight counts.
	Size  string `json:"size,omitempty"`
	Stage string `json:"stage"`
	Round int    `json:"round"`
	// Attempt is the 1-based attempt of the running session; > 1 means the
	// previous attempt failed for infrastructure reasons and was retried.
	Attempt int `json:"attempt,omitempty"`
	// Resumed marks a worker that took over from a session that never
	// finished — a scheduler dying while it ran, or a hard stop — rather
	// than starting fresh: its branch may carry work nobody reported.
	Resumed bool      `json:"resumed,omitempty"`
	Since   time.Time `json:"since"`
	// Sandbox is the mode the session running right now is boxed in, set
	// when that session starts. A worker's stages run different roles, and
	// a role's sandbox is its own, so this follows the session rather than
	// the worker. Empty until the worker's first session starts.
	Sandbox string `json:"sandbox,omitempty"`
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
	Priority []work.Key `json:"priority,omitempty"`
	// BudgetPaused is true while no new session is being dispatched because
	// the rolling 24h spend reached scheduler.max_cost_per_day and has not
	// yet fallen back to scheduler.max_cost_per_day_resume_percent of it, so
	// DaySpendUSD can be under DayBudgetUSD while this is still true.
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
	WaitingOnDeps map[work.Key][]work.Key `json:"waiting_on_deps,omitempty"`
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

// PauseNotice says why dispatch is paused, or "" when it is not. The claude
// session limit is reported before the daily budget: it is the harder stop,
// and it names the time it lifts because that is the only thing a person can
// do anything about. A LimitPausedUntil in the past is not a pause at all —
// nothing has looked at it since it lifted, and a budget pause behind it
// wins instead.
func (s Status) PauseNotice(now time.Time) string {
	switch {
	case s.LimitPausedUntil.After(now):
		return fmt.Sprintf("claude session limit until %s (in %s)",
			s.LimitPausedUntil.Local().Format("15:04"), ShortDur(s.LimitPausedUntil.Sub(now)))
	case s.BudgetPaused:
		return fmt.Sprintf("daily budget ($%.2f / $%.2f)", s.DaySpendUSD, s.DayBudgetUSD)
	default:
		return ""
	}
}

// BudgetNotice is the rolling 24-hour spend against scheduler.max_cost_per_day,
// or "" when no daily budget is configured. It is what a view shows while
// dispatch is running; PauseNotice is what it shows once the budget stopped it.
func (s Status) BudgetNotice() string {
	if s.DayBudgetUSD > 0 {
		return fmt.Sprintf("daily budget: $%.2f / $%.2f", s.DaySpendUSD, s.DayBudgetUSD)
	}
	return ""
}

// ShortDur renders a duration the way bees.toml writes one ("3h10m", "45s"):
// time.Duration.String() keeps a trailing "0m0s" that says nothing.
//
// The rounding to whole seconds happens first, so the rounded value picks the
// branch: 59.6s rounds up to a whole minute and must print "1m", not the
// "1m0s" that Duration.String() would give it.
func ShortDur(d time.Duration) string {
	d = d.Round(time.Second)
	if d < time.Minute {
		return d.String()
	}
	h, m := int(d/time.Hour), int(d/time.Minute)%60
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

// Escalated is one issue the factory handed to a person: which issue, what
// it is called, why the factory gave it up and when. The reason is what
// scheduler.escalate recorded (WorkState.Escalation); it is empty for an
// issue a person labelled by hand, and for one escalated before this state
// directory existed.
type Escalated struct {
	Work work.Ref `json:"work"`

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
	Work work.Ref `json:"work"`

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
	if err := s.MigrateExisting(); err != nil {
		return err
	}
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
	if err := s.Migrate(); err != nil {
		return err
	}
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
