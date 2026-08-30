// Package state manages the bees state directory: role notes, scheduler
// bookkeeping and the status file read by `bees status`.
//
// Layout of <state_dir>:
//
//	mail/<role>/*.json   local mailbox (see package mail)
//	notes/<role>.md      per-role notes, the roles' only long-term memory
//	sessions/<id>/       one directory per claude session (prompts, transcript, result)
//	issues/<n>.json      per-issue bookkeeping (review round, PR number)
//	qa.json              QA bookkeeping (last run)
//	product_manager.json product manager bookkeeping (last run)
//	status.json          live scheduler status
package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
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
- notes/     each role's notes file (their only memory between sessions)
- sessions/  prompts, transcripts and results of every Claude Code session
- issues/    per-issue bookkeeping (review rounds)
- status.json live scheduler status (` + "`bees status`" + `)

You can safely delete sessions/ to reclaim space. Editing notes/ by hand is a
good way to steer a role.
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

// EnsureNotes creates an empty notes file so sessions can edit it in place.
func (s *Store) EnsureNotes(role string) error {
	p := s.NotesPath(role)
	if _, err := os.Stat(p); err == nil {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte("# "+role+" notes\n\n"), 0o644)
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
	UpdatedAt   time.Time `json:"updated_at"`
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

// SaveIssue stores bookkeeping for an issue.
func (s *Store) SaveIssue(is IssueState) error {
	is.UpdatedAt = time.Now().UTC()
	return s.writeJSON(filepath.Join(s.Dir, "issues", strconv.Itoa(is.Number)+".json"), is)
}

// RoleState is bookkeeping for singleton roles.
type RoleState struct {
	LastRun time.Time `json:"last_run"`
	// LastCheck is when the scheduler last looked for work for the role
	// (used to rate-limit the QA merged-PR query).
	LastCheck time.Time `json:"last_check,omitempty"`
}

// Role loads bookkeeping for a singleton role.
func (s *Store) Role(role string) (RoleState, error) {
	var rs RoleState
	return rs, s.readJSON(filepath.Join(s.Dir, role+".json"), &rs)
}

// SaveRole stores bookkeeping for a singleton role.
func (s *Store) SaveRole(role string, rs RoleState) error {
	return s.writeJSON(filepath.Join(s.Dir, role+".json"), rs)
}

// Worker describes a running developer worker.
type Worker struct {
	Name  string    `json:"name"`
	Issue int       `json:"issue"`
	Stage string    `json:"stage"`
	Round int       `json:"round"`
	Since time.Time `json:"since"`
}

// Status is the live scheduler status.
type Status struct {
	UpdatedAt time.Time `json:"updated_at"`
	PID       int       `json:"pid"`
	LastPoll  time.Time `json:"last_poll"`
	// NextPoll is when the scheduler next polls GitHub. Between polls it
	// still runs local passes every scheduler.poll_interval.
	NextPoll time.Time `json:"next_poll,omitempty"`
	// InWorkHours is nil when scheduler.work_hours is not configured.
	InWorkHours *bool             `json:"in_work_hours,omitempty"`
	Workers     []Worker          `json:"workers"`
	Singletons  map[string]string `json:"singletons"` // role -> "idle"/"running"
	Queues      map[string]int    `json:"queues"`
	LastError   string            `json:"last_error,omitempty"`
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
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
