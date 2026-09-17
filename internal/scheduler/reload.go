package scheduler

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
)

// config is the configuration in force: what every pass, dispatch and
// session start reads. A reload replaces it between two passes, so a
// function that reads several settings of one decision takes it once and
// reads them off the same copy.
func (s *Scheduler) config() *config.Config { return s.cfg.Load() }

// Reload hands the scheduler a bees.toml read again from disk. It takes
// effect at the start of the next pass — which a reload asks for at once,
// the way a finished session does — so what the next dispatch uses is
// next, and nothing already running changes: a session keeps the model,
// timeout and prompt it was started with, and a worker between two stages
// reads the new settings at its next decision.
//
// What the scheduler and its collaborators were built from cannot change
// this way (fixedKeys): a reload that changes one of those keys is refused
// with an error naming them and leaves the configuration in force where it
// was, exactly as CheckReload would have said.
func (s *Scheduler) Reload(next *config.Config) error {
	if err := s.CheckReload(next); err != nil {
		return err
	}
	s.mu.Lock()
	s.pending = next
	s.mu.Unlock()
	s.log.Info("configuration reload accepted; in force from the next pass", "path", next.Path)
	s.signal()
	return nil
}

// CheckReload says whether Reload would accept next, without applying it:
// the error names every key next changes that a running factory cannot,
// nil when it changes none. A caller reloading several projects at once
// checks every one before it applies any.
func (s *Scheduler) CheckReload(next *config.Config) error {
	if next == nil {
		return errors.New("reload: no configuration")
	}
	keys := changedFixedKeys(s.config(), next)
	if len(keys) == 0 {
		return nil
	}
	return fmt.Errorf("%s: %s cannot change while the factory runs; restart bees run to apply it", next.Path, strings.Join(keys, ", "))
}

// applyPending puts the configuration Reload accepted in force. It runs at
// the start of every pass, full or local, so a pass reads one configuration
// from beginning to end.
func (s *Scheduler) applyPending() {
	s.mu.Lock()
	next := s.pending
	s.pending = nil
	s.mu.Unlock()
	if next == nil {
		return
	}
	s.cfg.Store(next)
	s.log.Info("configuration reloaded", "path", next.Path)
}

// fixedKey is one bees.toml key a reload cannot change: the scheduler, or
// something it was built with — the state directory, the GitHub client, the
// session runner, the workspace manager, the developer slots — was built
// from its value and reads no configuration afterwards.
type fixedKey struct {
	key  string
	same func(a, b *config.Config) bool
}

// fixedKeys lists them. docs/cli.md lists the same keys beside the live
// view's r key, and a key added here is added there.
var fixedKeys = []fixedKey{
	{"project.repo", func(a, b *config.Config) bool { return a.Project.Repo == b.Project.Repo }},
	{"project.dir", func(a, b *config.Config) bool { return a.CloneDir() == b.CloneDir() }},
	{"project.remote", func(a, b *config.Config) bool { return a.Project.Remote == b.Project.Remote }},
	{"project.state_dir", func(a, b *config.Config) bool { return a.StateDir() == b.StateDir() }},
	{"filter.label", func(a, b *config.Config) bool { return a.Filter.Label == b.Filter.Label }},
	{"filter.require_label", func(a, b *config.Config) bool { return a.Filter.LabelRequired() == b.Filter.LabelRequired() }},
	{"filter.assignee", func(a, b *config.Config) bool { return a.Filter.Assignee == b.Filter.Assignee }},
	{"filter.milestone", func(a, b *config.Config) bool { return a.Filter.Milestone == b.Filter.Milestone }},
	{"filter.creator", func(a, b *config.Config) bool { return a.Filter.Creator == b.Filter.Creator }},
	{"[github]", func(a, b *config.Config) bool { return reflect.DeepEqual(a.GitHub, b.GitHub) }},
	{"[notes]", func(a, b *config.Config) bool { return reflect.DeepEqual(a.Notes, b.Notes) }},
	{"[logging]", func(a, b *config.Config) bool { return reflect.DeepEqual(a.Logging, b.Logging) }},
	{"scheduler.max_developers", func(a, b *config.Config) bool { return a.Scheduler.MaxDevelopers == b.Scheduler.MaxDevelopers }},
	{"scheduler.workspace_root", func(a, b *config.Config) bool { return a.Scheduler.WorkspaceRoot == b.Scheduler.WorkspaceRoot }},
	{"scheduler.keep_workspaces", func(a, b *config.Config) bool { return a.Scheduler.KeepWorkspaces == b.Scheduler.KeepWorkspaces }},
	{"global.skills_refresh", func(a, b *config.Config) bool { return a.SkillsRefreshPolicy() == b.SkillsRefreshPolicy() }},
}

// changedFixedKeys names the fixed keys whose value differs between the
// configuration in force and next, in the order fixedKeys lists them.
func changedFixedKeys(current, next *config.Config) []string {
	var keys []string
	for _, k := range fixedKeys {
		if !k.same(current, next) {
			keys = append(keys, k.key)
		}
	}
	return keys
}
