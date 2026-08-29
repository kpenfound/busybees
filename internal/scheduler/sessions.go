package scheduler

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/session"
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
}

// runSession resolves the role, renders prompts and runs claude.
func (s *Scheduler) runSession(ctx context.Context, spec sessionSpec) (*session.Result, error) {
	role, err := s.cfg.Role(spec.role)
	if err != nil {
		return nil, err
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
	d.WorkDir = spec.workDir
	d.Branch = spec.branch
	d.StateDir = s.store.Dir
	d.SessionDir = sessionDir
	d.NotesFile = s.store.NotesPath(spec.role)
	d.Notes = notes
	if d.MaxRounds == 0 {
		d.MaxRounds = s.cfg.Scheduler.MaxReviewRounds
	}

	system, err := prompts.System(spec.role, d, role.Prompt)
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
	res, err := s.runner.Run(ctx, session.Request{
		Name:         spec.name,
		Role:         role,
		WorkDir:      spec.workDir,
		SystemPrompt: system,
		Prompt:       task,
		Env:          env,
		SessionDir:   sessionDir,
	})
	if err != nil {
		return nil, err
	}
	// Whatever the session created must stay visible to the factory.
	s.adoptCreated(ctx, started)
	return res, nil
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

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

func roleTitle(role string) string { return prompts.Title(role) }

var _ = config.RoleDeveloper
