// Package state manages the bees state directory: role notes, scheduler
// bookkeeping and the status file read by `bees status`.
//
// Layout of <state_dir>:
//
//	mail/<role>/*.json   local mailbox (see package mail)
//	notes/<role>.md      per-role notes, the roles' only long-term memory
//	notes/archive/       notes files replaced by `bees notes reset`
//	sessions/<id>/       one directory per claude session (prompts, transcript, result)
//	issues/<n>.json      per-issue bookkeeping (review round, PR number)
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
- issues/    per-issue bookkeeping (review rounds)
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

// IssueState is per-issue bookkeeping.
type IssueState struct {
	Number int    `json:"number"`
	Round  int    `json:"round"`
	PR     int    `json:"pr,omitempty"`
	Branch string `json:"branch,omitempty"`
	// CheckFixRounds counts reviewer-diagnoses/developer-fixes iterations
	// for failing required checks.
	CheckFixRounds int `json:"check_fix_rounds,omitempty"`
	// HumanSeenAt is the timestamp of the latest human PR activity already
	// delivered to the developer.
	HumanSeenAt time.Time `json:"human_seen_at,omitempty"`
	// ConflictNotifiedSHA is the PR head commit the developer was last told
	// to bring up to date with the default branch; the same head is never
	// mailed about twice.
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
	// feature it proposed may now be broken into work items.
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
	// being reported complete can be reported again.
	OpenChildren       []int     `json:"open_children,omitempty"`
	CompleteReportedAt time.Time `json:"complete_reported_at,omitempty"`
	UpdatedAt          time.Time `json:"updated_at"`
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

// SaveIssue stores bookkeeping for an issue. The running cost totals are not
// taken from is: they are owned by AddIssueCost and carried over from the
// file, because a developer worker holds one IssueState for the whole life of
// an issue and would otherwise write back the total as it was when it started.
func (s *Store) SaveIssue(is IssueState) error {
	if cur, err := s.Issue(is.Number); err == nil {
		is.Cost, is.Sessions = cur.Cost, cur.Sessions
	}
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(is.Number)+".json"), is)
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
	Attempt int       `json:"attempt,omitempty"`
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
	LastError     string        `json:"last_error,omitempty"`
	// Degraded lists the factory operations that are failing right now, one
	// entry per operation, sorted by name. Absent when the last pass was
	// clean.
	Degraded []OpFailure `json:"degraded,omitempty"`
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
	defer os.Remove(tmp)
	if _, err := f.Write(b); err != nil {
		f.Close()
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
