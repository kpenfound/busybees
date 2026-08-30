// Package config loads and validates bees.toml, the single file that
// configures an entire busybees staff.
//
// The file has four top-level tables:
//
//	[project]   – repo, default branch, state directory, product description
//	[filter]    – which GitHub issues/PRs the factory can see (label, assignee, milestone)
//	[global]    – prompt/skills/mcp/model settings applied to every role
//	[scheduler] – concurrency, polling and review-loop limits
//	[roles.*]   – per-role overrides (product_manager, project_manager,
//	              developer, reviewer, qa)
//
// Role settings are resolved by merging [global] with [roles.<name>]:
// prompts are concatenated, skills are unioned, MCP servers are unioned with
// the role winning on name conflicts, and scalar values fall back to the
// global value and then to a built-in default.
package config

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
	// scheduler.timezone must resolve on hosts without a system zoneinfo
	// database (minimal containers), so embed one.
	_ "time/tzdata"

	"github.com/BurntSushi/toml"
)

// Role names. These are the keys used under [roles.*] in bees.toml.
const (
	RoleProductManager = "product_manager"
	RoleProjectManager = "project_manager"
	RoleDeveloper      = "developer"
	RoleReviewer       = "reviewer"
	RoleQA             = "qa"
)

// Roles lists every role in the order the factory thinks about them.
var Roles = []string{RoleProductManager, RoleProjectManager, RoleDeveloper, RoleReviewer, RoleQA}

// roleAliases maps short CLI-friendly names to canonical role names.
var roleAliases = map[string]string{
	"pm":              RoleProductManager,
	"product":         RoleProductManager,
	"product_manager": RoleProductManager,
	"product-manager": RoleProductManager,
	"pjm":             RoleProjectManager,
	"project":         RoleProjectManager,
	"project_manager": RoleProjectManager,
	"project-manager": RoleProjectManager,
	"dev":             RoleDeveloper,
	"developer":       RoleDeveloper,
	"review":          RoleReviewer,
	"reviewer":        RoleReviewer,
	"qa":              RoleQA,
	"tester":          RoleQA,
}

// CanonicalRole resolves a user-supplied role name or alias.
func CanonicalRole(name string) (string, error) {
	if r, ok := roleAliases[strings.ToLower(strings.TrimSpace(name))]; ok {
		return r, nil
	}
	return "", fmt.Errorf("unknown role %q (want one of %s)", name, strings.Join(Roles, ", "))
}

// Defaults used when neither the role nor [global] sets a value.
const (
	DefaultModel         = "opus"
	DefaultFallbackModel = "sonnet"
	DefaultMaxTurns      = 200
	DefaultTimeout       = 45 * time.Minute
	DefaultLabel         = "bees"
	DefaultRemote        = "origin"
	DefaultStateDir      = ".bees"
	DefaultBranchPrefix  = "bees/"
	DefaultPollInterval  = 5 * time.Minute
	// DefaultRetries and friends govern retrying a session that failed for
	// infrastructure reasons; see Config.Retry.
	DefaultRetries           = 1
	DefaultRetryDelay        = 10 * time.Minute
	DefaultRetryWithFallback = true
	// DefaultPRFixConflicts and DefaultPRKeepUpdated govern what the
	// scheduler does with an open pull request that conflicts with, or has
	// fallen behind, the default branch; see Scheduler.FixConflicts.
	DefaultPRFixConflicts = true
	DefaultPRKeepUpdated  = false
	// MaxRetries caps scheduler.retries.
	MaxRetries = 5
	// DefaultOffHoursPollInterval is the polling cadence outside
	// scheduler.work_hours (only used when work_hours is set).
	DefaultOffHoursPollInterval = time.Hour
	DefaultRateLimitWait        = 15 * time.Minute
	DefaultMaxDevelopers        = 1
	DefaultReviewRounds         = 3
	DefaultPMInterval           = time.Hour
	DefaultQAInterval           = 30 * time.Minute
	DefaultTriageBatch          = 5
	DefaultSkillsRefresh        = "24h"
	// DefaultDispatchOrder and friends govern which ready issue a free
	// developer worker takes next; see Scheduler.DispatchOrder.
	DefaultDispatchOrder    = DispatchSmallFirst
	DefaultMaxLargeInFlight = 1
	// DefaultMaxSize is the largest size a developer takes by default.
	DefaultMaxSize = "l"
	// DefaultNotesConsolidateEvery is how many sessions a role runs between
	// two consolidation passes over its notes file.
	DefaultNotesConsolidateEvery = 10
	// DefaultNotesMaxBytes is the notes size above which consolidation is
	// asked for early, without waiting for the session count.
	DefaultNotesMaxBytes = 32768
)

// Dispatch orders accepted by scheduler.dispatch_order.
const (
	DispatchSmallFirst = "small-first"
	DispatchOldest     = "oldest"
	DispatchLargeFirst = "large-first"
)

// DispatchOrders lists the accepted scheduler.dispatch_order values.
var DispatchOrders = []string{DispatchSmallFirst, DispatchOldest, DispatchLargeFirst}

// Sizes lists the work item sizes, smallest first. They mirror the
// bees:size/* labels (see Labels.SizeLabels).
var Sizes = []string{"xs", "s", "m", "l", "xl"}

// Duration is a time.Duration that unmarshals from TOML strings like "30m".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(text []byte) error {
	v, err := time.ParseDuration(string(text))
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

// MarshalText implements encoding.TextMarshaler.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// Config is the parsed bees.toml.
// CurrentVersion is the bees.toml format version this build writes and reads
// natively. Bump it (and add a migration) when a change to the schema cannot
// be read by older files as-is: renamed or removed keys, changed semantics.
// Adding optional keys is not a breaking change.
const CurrentVersion = 1

// migration rewrites the text of a bees.toml from one format version to the
// next. Migrations work on the text, not the decoded tree, so the user's
// comments (and the commented-out defaults) survive the rewrite.
type migration func(text string) (string, error)

// migrations[n] converts a version-n file to version n+1. Files without a
// version key are version 0. Load applies the steps in memory; Config.Rewrite
// (run by bees run/tick/exec/status and `bees config migrate`) writes the
// result back to disk.
var migrations = map[int]migration{
	0: addVersionKey,
}

type Config struct {
	// Version is the format version of the file (see CurrentVersion). After
	// Load it is always CurrentVersion; MigratedFrom tells whether the file on
	// disk is older.
	Version int `toml:"version"`

	Project   Project                 `toml:"project"`
	Filter    Filter                  `toml:"filter"`
	Global    RoleSettings            `toml:"global"`
	Scheduler Scheduler               `toml:"scheduler"`
	Roles     map[string]RoleSettings `toml:"roles"`

	// Path is the absolute path of the loaded bees.toml (not part of the file).
	Path string `toml:"-"`
	// MigratedFrom is the version of the file on disk when it is older than
	// CurrentVersion (0 = no version key), or -1 when no migration was needed.
	MigratedFrom int `toml:"-"`
	// migrated is the file text after migrations, written by Rewrite.
	migrated string
}

// Project holds settings that describe the software being built.
type Project struct {
	// Remote is the git remote the factory fetches from and pushes to. Default "origin".
	Remote string `toml:"remote" json:"remote"`
	// Repo is the GitHub repository in "owner/name" form. Derived from the
	// remote's URL when empty (see Config.Resolve).
	Repo string `toml:"repo" json:"repo"`
	// DefaultBranch is the branch developers branch from and QA tests.
	// Derived from the remote's HEAD when empty.
	DefaultBranch string `toml:"default_branch" json:"default_branch"`
	// StateDir is where mail, notes, logs and scheduler state live. Relative
	// paths are resolved against the directory containing bees.toml.
	StateDir string `toml:"state_dir" json:"state_dir"`
	// BranchPrefix is prepended to developer branches, e.g. "bees/issue-12".
	BranchPrefix string `toml:"branch_prefix" json:"branch_prefix"`
}

// Filter selects which GitHub issues and pull requests the factory can see.
// All configured criteria must match (they are ANDed).
type Filter struct {
	// Label is the factory's label. It is the base name of the workflow state
	// labels ("bees:ready", ...) and, when RequireLabel is true, the
	// visibility gate: only issues/PRs carrying it are visible. Default "bees".
	Label string `toml:"label" json:"label"`
	// RequireLabel can be set to false so that Assignee and/or Milestone
	// alone define visibility. The factory still applies Label to everything
	// it creates. Default true.
	RequireLabel *bool `toml:"require_label" json:"require_label"`
	// Assignee restricts visibility to issues/PRs assigned to this GitHub
	// login ("@me" resolves to the authenticated gh user). Everything the
	// factory creates is assigned to this user so it stays visible.
	Assignee string `toml:"assignee" json:"assignee"`
	// Milestone restricts visibility to issues/PRs in this milestone title.
	Milestone string `toml:"milestone" json:"milestone"`
}

// LabelRequired reports whether the label is part of the visibility gate.
func (f Filter) LabelRequired() bool { return f.RequireLabel == nil || *f.RequireLabel }

// BuiltinMCPServer is the name of the MCP server bees adds to every session
// (`bees mcp serve`). The name is reserved: bees.toml may not define it.
const BuiltinMCPServer = "bees"

// MCPServer configures one MCP server. Either Command (stdio) or URL (http/sse)
// must be set.
type MCPServer struct {
	Type    string            `toml:"type" json:"type"` // stdio (default when command set), http, sse
	Command string            `toml:"command" json:"command"`
	Args    []string          `toml:"args" json:"args"`
	Env     map[string]string `toml:"env" json:"env"`
	URL     string            `toml:"url" json:"url"`
	Headers map[string]string `toml:"headers" json:"headers"`
}

// RoleSettings are the settings that can be given globally or per role.
type RoleSettings struct {
	// Prompt is appended to the role's base prompt.
	Prompt string `toml:"prompt"`
	// PromptFile is a path (relative to bees.toml) whose contents are appended after Prompt.
	PromptFile string `toml:"prompt_file"`
	// Skills are git URLs of skill or plugin repositories. Optional "#sub/dir"
	// selects a directory inside the repo; optional "@ref" pins a branch/tag.
	Skills []string `toml:"skills"`
	// MCP servers keyed by name.
	MCP map[string]MCPServer `toml:"mcp"`
	// Model is the claude model alias or id. FallbackModel is used automatically
	// when Model has reached its usage limit.
	Model         string `toml:"model"`
	FallbackModel string `toml:"fallback_model"`
	// Effort is passed as --effort (low/medium/high/max) when set.
	Effort string `toml:"effort"`
	// MaxTurns caps agentic turns for a single session.
	MaxTurns int `toml:"max_turns"`
	// Timeout kills a session that runs longer than this.
	Timeout Duration `toml:"timeout"`
	// AllowedTools / DisallowedTools are passed straight through to claude.
	AllowedTools    []string `toml:"allowed_tools"`
	DisallowedTools []string `toml:"disallowed_tools"`
	// Enabled can be set to false on a role to take it out of the rotation.
	Enabled *bool `toml:"enabled"`
	// Shell is the shell claude's Bash tool uses in sessions (exported as
	// $SHELL), e.g. "/bin/bash". Default: the shell bees runs under.
	Shell string `toml:"shell"`
	// Env are environment variables exported into every session: to
	// claude, its Bash tool, MCP servers and git. $VAR references are
	// expanded from the bees process environment. Role entries override
	// global ones with the same name.
	Env map[string]string `toml:"env"`

	// The following key is only valid under [global].

	// SkillsRefresh is how stale a skill clone may get before it is pulled
	// when a session needs it: "never", "always" or a duration ("24h").
	SkillsRefresh string `toml:"skills_refresh"`

	// The following key is only valid under [roles.developer].

	// CommitFlags are extra flags the developer passes to every `git commit`,
	// for example "--gpg-sign --signoff". Appended to its system prompt.
	CommitFlags string `toml:"commit_flags"`
	// MaxSize is the largest work item size a developer takes ("xs".."xl").
	// A ready issue sized above it is sent back to triage to be split.
	MaxSize string `toml:"max_size"`
	// ModelBySize picks the model per work item size, keyed by "xs".."xl".
	// A size with no entry uses Model.
	ModelBySize map[string]string `toml:"model_by_size"`

	// The following keys are only valid under [roles.reviewer].

	// AutoMerge lets the reviewer merge a pull request it approved once the
	// required checks are green. Off by default: humans merge.
	AutoMerge *bool `toml:"auto_merge"`
	// MergeMethod is squash (default), merge or rebase.
	MergeMethod string `toml:"merge_method"`
	// ChecksWait is how long to wait after approval before polling required
	// checks, since some take a moment to report they have started. Default 1m.
	ChecksWait Duration `toml:"checks_wait"`
	// ChecksPollInterval is how often required checks are polled while
	// waiting for them. Default 2m.
	ChecksPollInterval Duration `toml:"checks_poll_interval"`
	// ChecksTimeout bounds how long to wait for required checks to finish
	// before escalating to a human. Default 30m.
	ChecksTimeout Duration `toml:"checks_timeout"`
	// MaxCheckFixRounds caps reviewer-diagnoses/developer-fixes iterations
	// when required checks fail. Default 2.
	MaxCheckFixRounds int `toml:"max_check_fix_rounds"`
}

// MergePolicy is the reviewer's auto-merge configuration.
type MergePolicy struct {
	AutoMerge          bool
	Method             string
	ChecksWait         time.Duration
	ChecksPollInterval time.Duration
	ChecksTimeout      time.Duration
	MaxCheckFixRounds  int
}

// Defaults for the merge policy.
const (
	DefaultMergeMethod       = "squash"
	DefaultChecksWait        = time.Minute
	DefaultChecksPoll        = 2 * time.Minute
	DefaultChecksTimeout     = 30 * time.Minute
	DefaultMaxCheckFixRounds = 2
)

// SkillsRefresh returns the skill refresh policy from [global]: always is
// true for "always", after is 0 for "never" and the parsed duration
// otherwise. Validate has already rejected anything else.
func (c *Config) SkillsRefresh() (always bool, after time.Duration) {
	always, after, _ = parseSkillsRefresh(c.Global.SkillsRefresh)
	return always, after
}

// SkillsRefreshPolicy returns the policy as it is written in bees.toml.
func (c *Config) SkillsRefreshPolicy() string {
	if strings.TrimSpace(c.Global.SkillsRefresh) == "" {
		return DefaultSkillsRefresh
	}
	return strings.TrimSpace(c.Global.SkillsRefresh)
}

func parseSkillsRefresh(v string) (always bool, after time.Duration, err error) {
	switch s := strings.TrimSpace(v); s {
	case "":
		return parseSkillsRefresh(DefaultSkillsRefresh)
	case "never":
		return false, 0, nil
	case "always":
		return true, 0, nil
	default:
		d, err := time.ParseDuration(s)
		if err != nil || d < 0 {
			return false, 0, fmt.Errorf("skills_refresh %q must be \"never\", \"always\" or a duration >= 0 such as \"24h\"", s)
		}
		return false, d, nil
	}
}

// CommitFlags returns the developer's extra git commit flags.
func (c *Config) CommitFlags() string { return strings.TrimSpace(c.Roles[RoleDeveloper].CommitFlags) }

// MaxSize returns the largest work item size the developer takes.
func (c *Config) MaxSize() string {
	return firstNonEmpty(strings.TrimSpace(c.Roles[RoleDeveloper].MaxSize), DefaultMaxSize)
}

// LargeInFlight returns scheduler.max_large_in_flight (0 = no cap).
func (s Scheduler) LargeInFlight() int {
	if s.MaxLargeInFlight == nil {
		return DefaultMaxLargeInFlight
	}
	return *s.MaxLargeInFlight
}

// FixConflicts returns scheduler.pr_fix_conflicts: whether an open pull
// request that conflicts with the default branch is handed back to the
// developer.
func (s Scheduler) FixConflicts() bool {
	if s.PRFixConflicts == nil {
		return DefaultPRFixConflicts
	}
	return *s.PRFixConflicts
}

// Merge returns the resolved merge policy from [roles.reviewer].
func (c *Config) Merge() MergePolicy {
	rs := c.Roles[RoleReviewer]
	p := MergePolicy{
		Method:             firstNonEmpty(rs.MergeMethod, DefaultMergeMethod),
		ChecksWait:         firstPositiveDur(rs.ChecksWait.Duration, DefaultChecksWait),
		ChecksPollInterval: firstPositiveDur(rs.ChecksPollInterval.Duration, DefaultChecksPoll),
		ChecksTimeout:      firstPositiveDur(rs.ChecksTimeout.Duration, DefaultChecksTimeout),
		MaxCheckFixRounds:  firstPositive(rs.MaxCheckFixRounds, DefaultMaxCheckFixRounds),
	}
	if rs.AutoMerge != nil {
		p.AutoMerge = *rs.AutoMerge
	}
	return p
}

// RetryPolicy is the resolved [scheduler] retry configuration.
type RetryPolicy struct {
	// Retries is the number of extra attempts a session gets after an
	// infrastructure failure. 0 means a session runs exactly once.
	Retries int
	// Delay is how long to wait before an attempt is repeated.
	Delay time.Duration
	// WithFallback runs a retry with the role's fallback model as primary.
	WithFallback bool
}

// Retry returns the resolved retry policy.
func (c *Config) Retry() RetryPolicy {
	p := RetryPolicy{Retries: DefaultRetries, Delay: DefaultRetryDelay, WithFallback: DefaultRetryWithFallback}
	if n := c.Scheduler.Retries; n != nil {
		p.Retries = *n
	}
	if d := c.Scheduler.RetryDelay; d != nil {
		p.Delay = d.Duration
	}
	if b := c.Scheduler.RetryWithFallback; b != nil {
		p.WithFallback = *b
	}
	return p
}

// Scheduler configures the orchestrator loop.
type Scheduler struct {
	// PollInterval is how often GitHub is polled for work. Each poll costs
	// two API calls (open issues, open PRs); everything else is gated on what
	// those lists report. Default 5m.
	PollInterval Duration `toml:"poll_interval" json:"poll_interval"`
	// RateLimitBackoff is how long to pause polling after GitHub reports a
	// rate limit. Default 15m.
	RateLimitBackoff Duration `toml:"rate_limit_backoff" json:"rate_limit_backoff"`
	// MaxDevelopers is the number of concurrent developer workers. Each worker
	// runs a sequential developer <-> reviewer loop for one issue at a time.
	MaxDevelopers int `toml:"max_developers" json:"max_developers"`
	// MaxReviewRounds caps developer/reviewer iterations before an issue is
	// escalated with the needs-human label.
	MaxReviewRounds int `toml:"max_review_rounds" json:"max_review_rounds"`
	// ProductManagerInterval is the minimum time between product manager runs
	// (mail in the PM inbox triggers an earlier run).
	ProductManagerInterval Duration `toml:"product_manager_interval" json:"product_manager_interval"`
	// QAInterval is the minimum time between QA runs. QA only runs when
	// something has been merged since its last run.
	QAInterval Duration `toml:"qa_interval" json:"qa_interval"`
	// TriageBatchSize is the maximum number of issues handed to the project
	// manager in one session.
	TriageBatchSize int `toml:"triage_batch_size" json:"triage_batch_size"`
	// NotesConsolidateEvery is how many sessions a role runs between two
	// passes in which it is also asked to consolidate its notes file.
	// Default 10.
	NotesConsolidateEvery int `toml:"notes_consolidate_every" json:"notes_consolidate_every"`
	// NotesMaxBytes asks for consolidation early, whatever the session
	// count, once a notes file grows past this size. Default 32768.
	NotesMaxBytes int `toml:"notes_max_bytes" json:"notes_max_bytes"`
	// DispatchOrder decides which ready issue a free developer worker takes
	// next: small-first (default), oldest or large-first.
	DispatchOrder string `toml:"dispatch_order" json:"dispatch_order"`
	// MaxLargeInFlight caps how many bees:size/l issues developer workers may
	// hold at once. 0 means no cap. Default 1.
	MaxLargeInFlight *int `toml:"max_large_in_flight" json:"max_large_in_flight"`
	// PRFixConflicts hands an open pull request that conflicts with the
	// default branch back to its developer: the developer is mailed, and an
	// approved issue goes back to ready ahead of new work. Default true.
	PRFixConflicts *bool `toml:"pr_fix_conflicts" json:"pr_fix_conflicts"`
	// PRKeepUpdated does the same when the pull request merely fell behind
	// the default branch without conflicting. Default false.
	PRKeepUpdated bool `toml:"pr_keep_updated" json:"pr_keep_updated"`
	// Retries is the number of extra attempts a session gets when it failed
	// for infrastructure reasons (timeout, API error, exhausted turns).
	// 0 disables retrying. Default 1.
	Retries *int `toml:"retries" json:"retries"`
	// RetryDelay is how long to wait before a retry. Default 10m.
	RetryDelay *Duration `toml:"retry_delay" json:"retry_delay"`
	// RetryWithFallback runs a retry with the role's fallback_model as its
	// primary model. Default true.
	RetryWithFallback *bool `toml:"retry_with_fallback" json:"retry_with_fallback"`
	// KeepWorkspaces leaves temp worktrees on disk after a session (debugging).
	KeepWorkspaces bool `toml:"keep_workspaces" json:"keep_workspaces"`
	// WorkspaceRoot overrides the temp dir used for worktrees.
	WorkspaceRoot string `toml:"workspace_root" json:"workspace_root"`
	// WorkHours is the daily window during which GitHub is polled every
	// PollInterval, as "HH:MM-HH:MM" on a 24-hour clock ("09:00-18:00").
	// Empty (the default) disables the feature: GitHub is polled every
	// PollInterval around the clock and the three keys below are ignored.
	// A window whose start is after its end wraps midnight and belongs to
	// the day its start falls on ("22:00-06:00" with work_days = ["fri"]
	// covers Friday 22:00 to Saturday 06:00).
	WorkHours string `toml:"work_hours" json:"work_hours"`
	// OffHoursPollInterval is how often GitHub is polled outside the work
	// hours window. Must be >= PollInterval. Default 1h.
	OffHoursPollInterval Duration `toml:"off_hours_poll_interval" json:"off_hours_poll_interval"`
	// WorkDays are the days the window applies to, as lowercase three-letter
	// names (mon, tue, wed, thu, fri, sat, sun). Default mon-fri.
	WorkDays []string `toml:"work_days" json:"work_days"`
	// Timezone is the IANA name the window is interpreted in
	// ("America/New_York"). Empty means the machine's local time.
	Timezone string `toml:"timezone" json:"timezone"`

	// Parsed form of the four keys above, filled in by Validate.
	whStart, whEnd int // minutes since midnight
	whDays         map[time.Weekday]bool
	whLoc          *time.Location
	whEnabled      bool
}

// weekdayNames maps the accepted work_days values to weekdays, in the order
// they are printed.
var weekdayNames = []struct {
	name string
	day  time.Weekday
}{
	{"mon", time.Monday}, {"tue", time.Tuesday}, {"wed", time.Wednesday},
	{"thu", time.Thursday}, {"fri", time.Friday}, {"sat", time.Saturday}, {"sun", time.Sunday},
}

// WorkHoursEnabled reports whether a work-hours window is configured.
func (s Scheduler) WorkHoursEnabled() bool { return s.whEnabled }

// InWorkHours reports whether t falls inside the configured window. It is
// always true when no window is configured.
func (s Scheduler) InWorkHours(t time.Time) bool {
	if !s.whEnabled {
		return true
	}
	t = t.In(s.whLoc)
	mins := t.Hour()*60 + t.Minute()
	if s.whStart < s.whEnd {
		return s.whDays[t.Weekday()] && mins >= s.whStart && mins < s.whEnd
	}
	// Overnight window: it belongs to the day its start falls on, so the
	// tail after midnight counts as the previous day.
	if mins >= s.whStart {
		return s.whDays[t.Weekday()]
	}
	return mins < s.whEnd && s.whDays[(t.Weekday()+6)%7]
}

// NextWorkHoursStart returns the earliest instant strictly after t at which
// the work-hours window opens. It is the zero time when no window is
// configured or when t is already inside one, and it skips days the window
// does not apply to (Friday evening asks about Monday morning). As in
// InWorkHours, an overnight window belongs to the day its start falls on.
func (s Scheduler) NextWorkHoursStart(t time.Time) time.Time {
	if !s.whEnabled || s.InWorkHours(t) {
		return time.Time{}
	}
	t = t.In(s.whLoc)
	y, m, d := t.Date()
	// Today, then every day of the week that follows: the window opens on
	// one of them unless no day is a work day, which Validate rejects.
	for i := 0; i <= 7; i++ {
		start := time.Date(y, m, d+i, s.whStart/60, s.whStart%60, 0, 0, s.whLoc)
		if s.whDays[start.Weekday()] && start.After(t) {
			return start
		}
	}
	return time.Time{}
}

// PollIntervalAt returns the GitHub polling interval that applies at t.
func (s Scheduler) PollIntervalAt(t time.Time) time.Duration {
	if !s.whEnabled || s.InWorkHours(t) {
		return s.PollInterval.Duration
	}
	return s.OffHoursPollInterval.Duration
}

// WorkHoursDescription renders the window for `bees status`, for example
// "09:00-18:00 mon-fri, America/New_York".
func (s Scheduler) WorkHoursDescription() string {
	if !s.whEnabled {
		return ""
	}
	return fmt.Sprintf("%s %s, %s", s.WorkHours, describeDays(s.whDays), s.whLoc)
}

// describeDays prints a day set as compact ranges: "mon-fri", "mon,wed,fri".
// A run of exactly two days is listed rather than hyphenated ("sat,sun").
func describeDays(days map[time.Weekday]bool) string {
	var parts []string
	for i := 0; i < len(weekdayNames); i++ {
		if !days[weekdayNames[i].day] {
			continue
		}
		j := i
		for j+1 < len(weekdayNames) && days[weekdayNames[j+1].day] {
			j++
		}
		switch j {
		case i:
			parts = append(parts, weekdayNames[i].name)
		case i + 1:
			parts = append(parts, weekdayNames[i].name, weekdayNames[j].name)
		default:
			parts = append(parts, weekdayNames[i].name+"-"+weekdayNames[j].name)
		}
		i = j
	}
	return strings.Join(parts, ",")
}

// parseWorkHours validates the work-hours keys and fills in the parsed
// fields. It returns one message per problem, each naming the key.
func (s *Scheduler) parseWorkHours() []string {
	s.whEnabled = false
	if s.WorkHours == "" {
		return nil
	}
	var errs []string
	start, end, err := parseWindow(s.WorkHours)
	if err != nil {
		errs = append(errs, "scheduler."+err.Error())
	} else {
		s.whStart, s.whEnd = start, end
	}
	days := map[time.Weekday]bool{}
	accepted := make([]string, 0, len(weekdayNames))
	for _, w := range weekdayNames {
		accepted = append(accepted, w.name)
	}
	for _, d := range s.WorkDays {
		found := false
		for _, w := range weekdayNames {
			if strings.ToLower(strings.TrimSpace(d)) == w.name {
				days[w.day] = true
				found = true
				break
			}
		}
		if !found {
			errs = append(errs, fmt.Sprintf("scheduler.work_days: unknown day %q (want one of %s)", d, strings.Join(accepted, " ")))
		}
	}
	if len(days) == 0 && len(errs) == 0 {
		errs = append(errs, fmt.Sprintf("scheduler.work_days must list at least one of %s", strings.Join(accepted, " ")))
	}
	loc := time.Local
	if s.Timezone != "" {
		if loc, err = time.LoadLocation(s.Timezone); err != nil {
			errs = append(errs, fmt.Sprintf("scheduler.timezone: %v", err))
		}
	}
	if s.OffHoursPollInterval.Duration < s.PollInterval.Duration {
		errs = append(errs, fmt.Sprintf("scheduler.off_hours_poll_interval (%s) must be >= scheduler.poll_interval (%s)",
			s.OffHoursPollInterval.Duration, s.PollInterval.Duration))
	}
	if len(errs) > 0 {
		return errs
	}
	s.whDays, s.whLoc, s.whEnabled = days, loc, true
	return nil
}

// parseWindow parses "HH:MM-HH:MM" into minutes since midnight.
func parseWindow(window string) (start, end int, err error) {
	bad := fmt.Errorf("work_hours: want \"HH:MM-HH:MM\" on a 24-hour clock (e.g. \"09:00-18:00\"), got %q", window)
	a, b, ok := strings.Cut(window, "-")
	if !ok {
		return 0, 0, bad
	}
	if start, err = parseClock(a); err != nil {
		return 0, 0, bad
	}
	if end, err = parseClock(b); err != nil {
		return 0, 0, bad
	}
	if start == end {
		return 0, 0, fmt.Errorf("work_hours %q: start and end must differ", window)
	}
	return start, end, nil
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", strings.TrimSpace(s))
	if err != nil {
		return 0, err
	}
	return t.Hour()*60 + t.Minute(), nil
}

// ResolvedRole is the effective configuration for one role after merging
// [global] and [roles.<name>].
type ResolvedRole struct {
	Name   string
	Prompt string // global prompt + role prompt (+ prompt files)
	Skills []string
	MCP    map[string]MCPServer
	Model  string
	// ModelBySize overrides Model per work item size; developer only.
	ModelBySize     map[string]string
	FallbackModel   string
	Effort          string
	MaxTurns        int
	Timeout         time.Duration
	AllowedTools    []string
	DisallowedTools []string
	Enabled         bool
	Shell           string
	Env             map[string]string
}

// Load reads and validates the bees.toml at path.
func Load(path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return nil, err
	}
	return Parse(string(data), path)
}

// Parse validates the text of a bees.toml as if it had been read from path,
// which is used for the Config's location and in error messages but is not
// read and need not exist. `bees init` parses the template it rendered before
// writing anything to disk.
func Parse(text, path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	version, err := fileVersion(text)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	cfg := Config{Path: abs, MigratedFrom: -1}
	if version < CurrentVersion {
		if text, err = migrate(text, version, CurrentVersion, migrations); err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		cfg.MigratedFrom = version
		cfg.migrated = text
	}
	md, err := toml.Decode(text, &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if undecoded := md.Undecoded(); len(undecoded) > 0 {
		keys := make([]string, 0, len(undecoded))
		for _, k := range undecoded {
			keys = append(keys, k.String())
		}
		return nil, fmt.Errorf("%s: unknown keys: %s", path, strings.Join(keys, ", "))
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// NeedsRewrite reports whether the file on disk is older than CurrentVersion.
func (c *Config) NeedsRewrite() bool { return c.MigratedFrom >= 0 }

// Rewrite writes the migrated text back to the file when NeedsRewrite. It
// keeps a copy of the original next to it as bees.toml.v<old>.bak.
func (c *Config) Rewrite() (backup string, err error) {
	if !c.NeedsRewrite() {
		return "", nil
	}
	backup = fmt.Sprintf("%s.v%d.bak", c.Path, c.MigratedFrom)
	orig, err := os.ReadFile(c.Path)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(backup, orig, 0o644); err != nil {
		return "", err
	}
	if err := os.WriteFile(c.Path, []byte(c.migrated), 0o644); err != nil {
		return "", err
	}
	c.MigratedFrom = -1
	return backup, nil
}

// fileVersion reads the top-level version key of a bees.toml. A file
// without one is version 0 (written before the key existed).
func fileVersion(text string) (int, error) {
	var raw map[string]any
	if _, err := toml.Decode(text, &raw); err != nil {
		return 0, fmt.Errorf("parse: %w", err)
	}
	v, ok := raw["version"]
	if !ok {
		return 0, nil
	}
	n, ok := v.(int64)
	if !ok || n < 0 {
		return 0, fmt.Errorf("version must be a non-negative integer (this bees writes %d), got %v", CurrentVersion, v)
	}
	if n > CurrentVersion {
		return 0, fmt.Errorf("version %d is newer than this bees understands (%d): upgrade bees", n, CurrentVersion)
	}
	return int(n), nil
}

// migrate upgrades text from version from to version to by applying
// steps[from], steps[from+1], ... in order.
func migrate(text string, from, to int, steps map[int]migration) (string, error) {
	for v := from; v < to; v++ {
		step, ok := steps[v]
		if !ok {
			return "", fmt.Errorf("no migration from bees.toml version %d to %d", v, v+1)
		}
		out, err := step(text)
		if err != nil {
			return "", fmt.Errorf("migrate bees.toml version %d to %d: %w", v, v+1, err)
		}
		text = setVersion(out, v+1)
	}
	return text, nil
}

var versionLineRE = regexp.MustCompile(`(?m)^[ \t]*version[ \t]*=[ \t]*\d+[ \t]*(#.*)?$`)

// setVersion replaces the version line, or inserts one (with a comment)
// before the first non-comment line, i.e. ahead of every table.
func setVersion(text string, v int) string {
	line := fmt.Sprintf("version = %d", v)
	if versionLineRE.MatchString(text) {
		return versionLineRE.ReplaceAllString(text, line)
	}
	lines := strings.Split(text, "\n")
	at := len(lines)
	for i, l := range lines {
		if t := strings.TrimSpace(l); t != "" && !strings.HasPrefix(t, "#") {
			at = i
			break
		}
	}
	block := []string{"# Format version of this file (see docs/configuration.md).", line, ""}
	if at > 0 && strings.TrimSpace(lines[at-1]) != "" {
		block = append([]string{""}, block...)
	}
	out := append([]string{}, lines[:at]...)
	out = append(out, block...)
	out = append(out, lines[at:]...)
	return strings.Join(out, "\n")
}

// addVersionKey is the 0 -> 1 migration: the format is unchanged, the file
// only gains its version key (which migrate itself sets).
func addVersionKey(text string) (string, error) { return text, nil }

// Find looks for bees.toml in dir and its parents.
func Find(dir string) (string, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	for {
		p := filepath.Join(dir, "bees.toml")
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("bees.toml not found in this directory or any parent (run `bees init`)")
		}
		dir = parent
	}
}

func (c *Config) applyDefaults() {
	if c.Project.Remote == "" {
		c.Project.Remote = DefaultRemote
	}
	if c.Filter.Label == "" {
		c.Filter.Label = DefaultLabel
	}
	if c.Project.StateDir == "" {
		c.Project.StateDir = DefaultStateDir
	}
	if c.Project.BranchPrefix == "" {
		c.Project.BranchPrefix = DefaultBranchPrefix
	}
	if c.Scheduler.PollInterval.Duration == 0 {
		c.Scheduler.PollInterval.Duration = DefaultPollInterval
	}
	if c.Scheduler.RateLimitBackoff.Duration == 0 {
		c.Scheduler.RateLimitBackoff.Duration = DefaultRateLimitWait
	}
	if c.Scheduler.MaxDevelopers == 0 {
		c.Scheduler.MaxDevelopers = DefaultMaxDevelopers
	}
	if c.Scheduler.MaxReviewRounds == 0 {
		c.Scheduler.MaxReviewRounds = DefaultReviewRounds
	}
	if c.Scheduler.ProductManagerInterval.Duration == 0 {
		c.Scheduler.ProductManagerInterval.Duration = DefaultPMInterval
	}
	if c.Scheduler.QAInterval.Duration == 0 {
		c.Scheduler.QAInterval.Duration = DefaultQAInterval
	}
	if c.Scheduler.TriageBatchSize == 0 {
		c.Scheduler.TriageBatchSize = DefaultTriageBatch
	}
	if c.Scheduler.NotesConsolidateEvery == 0 {
		c.Scheduler.NotesConsolidateEvery = DefaultNotesConsolidateEvery
	}
	if c.Scheduler.NotesMaxBytes == 0 {
		c.Scheduler.NotesMaxBytes = DefaultNotesMaxBytes
	}
	if c.Global.SkillsRefresh == "" {
		c.Global.SkillsRefresh = DefaultSkillsRefresh
	}
	if c.Scheduler.DispatchOrder == "" {
		c.Scheduler.DispatchOrder = DefaultDispatchOrder
	}
	if c.Scheduler.MaxLargeInFlight == nil {
		n := DefaultMaxLargeInFlight
		c.Scheduler.MaxLargeInFlight = &n
	}
	if c.Scheduler.PRFixConflicts == nil {
		b := DefaultPRFixConflicts
		c.Scheduler.PRFixConflicts = &b
	}
	if c.Scheduler.Retries == nil {
		n := DefaultRetries
		c.Scheduler.Retries = &n
	}
	if c.Scheduler.RetryDelay == nil {
		c.Scheduler.RetryDelay = &Duration{DefaultRetryDelay}
	}
	if c.Scheduler.RetryWithFallback == nil {
		b := DefaultRetryWithFallback
		c.Scheduler.RetryWithFallback = &b
	}
	if c.Scheduler.WorkHours != "" {
		if c.Scheduler.OffHoursPollInterval.Duration == 0 {
			// The default must never be shorter than poll_interval (already
			// defaulted above), or a config that only sets a long
			// poll_interval fails validation on a key it does not contain.
			c.Scheduler.OffHoursPollInterval.Duration = max(DefaultOffHoursPollInterval, c.Scheduler.PollInterval.Duration)
		}
		if c.Scheduler.WorkDays == nil {
			c.Scheduler.WorkDays = []string{"mon", "tue", "wed", "thu", "fri"}
		}
	}
	if c.Roles == nil {
		c.Roles = map[string]RoleSettings{}
	}
}

// Validate checks the configuration for errors.
func (c *Config) Validate() error {
	var errs []string
	if strings.ContainsAny(c.Project.Remote, " /") {
		errs = append(errs, fmt.Sprintf("project.remote %q is not a valid remote name", c.Project.Remote))
	}
	if parts := strings.Split(c.Project.Repo, "/"); c.Project.Repo != "" && (len(parts) != 2 || parts[0] == "" || parts[1] == "") {
		errs = append(errs, fmt.Sprintf("project.repo %q must be owner/name", c.Project.Repo))
	}
	if strings.ContainsAny(c.Filter.Label, " :") {
		errs = append(errs, "filter.label must not contain spaces or colons")
	}
	if !c.Filter.LabelRequired() && c.Filter.Assignee == "" && c.Filter.Milestone == "" {
		errs = append(errs, "filter.require_label = false needs filter.assignee or filter.milestone, otherwise every issue in the repo is visible")
	}
	for name := range c.Roles {
		if _, err := CanonicalRole(name); err != nil || name != mustCanonical(name) {
			errs = append(errs, fmt.Sprintf("roles.%s: unknown role (want one of %s)", name, strings.Join(Roles, ", ")))
		}
	}
	if c.Scheduler.MaxDevelopers < 1 {
		errs = append(errs, "scheduler.max_developers must be >= 1")
	}
	if c.Scheduler.MaxReviewRounds < 0 {
		errs = append(errs, "scheduler.max_review_rounds must be >= 0")
	}
	if n := c.Scheduler.Retries; n != nil && (*n < 0 || *n > MaxRetries) {
		errs = append(errs, fmt.Sprintf("scheduler.retries must be between 0 and %d", MaxRetries))
	}
	if d := c.Scheduler.RetryDelay; d != nil && d.Duration < 0 {
		errs = append(errs, "scheduler.retry_delay must be >= 0")
	}
	switch c.Scheduler.DispatchOrder {
	case "", DispatchSmallFirst, DispatchOldest, DispatchLargeFirst:
	default:
		errs = append(errs, fmt.Sprintf("scheduler.dispatch_order must be one of %s", strings.Join(DispatchOrders, ", ")))
	}
	if n := c.Scheduler.MaxLargeInFlight; n != nil && *n < 0 {
		errs = append(errs, "scheduler.max_large_in_flight must be >= 0")
	}
	if c.Scheduler.NotesConsolidateEvery < 0 {
		errs = append(errs, "scheduler.notes_consolidate_every must be >= 0 (0 means the default)")
	}
	if c.Scheduler.NotesMaxBytes < 0 {
		errs = append(errs, "scheduler.notes_max_bytes must be >= 0 (0 means the default)")
	}
	errs = append(errs, c.Scheduler.parseWorkHours()...)
	check := func(scope string, rs RoleSettings) {
		if scope != "roles."+RoleReviewer {
			if rs.AutoMerge != nil || rs.MergeMethod != "" || rs.ChecksWait.Duration != 0 || rs.ChecksPollInterval.Duration != 0 || rs.ChecksTimeout.Duration != 0 || rs.MaxCheckFixRounds != 0 {
				errs = append(errs, fmt.Sprintf("%s: auto_merge, merge_method, checks_wait, checks_poll_interval, checks_timeout and max_check_fix_rounds are only valid under roles.reviewer", scope))
			}
		}
		if scope != "global" && rs.SkillsRefresh != "" {
			errs = append(errs, fmt.Sprintf("%s: skills_refresh is only valid under global", scope))
		} else if _, _, err := parseSkillsRefresh(rs.SkillsRefresh); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", scope, err))
		}
		if scope != "roles."+RoleDeveloper && (rs.CommitFlags != "" || rs.MaxSize != "" || len(rs.ModelBySize) > 0) {
			errs = append(errs, fmt.Sprintf("%s: commit_flags, max_size and model_by_size are only valid under roles.developer", scope))
		}
		if rs.MaxSize != "" && !slices.Contains(Sizes, rs.MaxSize) {
			errs = append(errs, fmt.Sprintf("%s.max_size must be one of %s", scope, strings.Join(Sizes, ", ")))
		}
		for _, size := range slices.Sorted(maps.Keys(rs.ModelBySize)) {
			switch {
			case !slices.Contains(Sizes, size):
				errs = append(errs, fmt.Sprintf("%s.model_by_size: unknown size %q (want one of %s)", scope, size, strings.Join(Sizes, ", ")))
			case strings.TrimSpace(rs.ModelBySize[size]) == "":
				errs = append(errs, fmt.Sprintf("%s.model_by_size.%s must name a model", scope, size))
			}
		}
		switch rs.MergeMethod {
		case "", "squash", "merge", "rebase":
		default:
			errs = append(errs, fmt.Sprintf("%s.merge_method must be squash, merge or rebase", scope))
		}
		for name, m := range rs.MCP {
			if name == BuiltinMCPServer {
				errs = append(errs, fmt.Sprintf("%s.mcp.%s: mcp server name %q is reserved for the built-in server", scope, name, BuiltinMCPServer))
			}
			if m.Command == "" && m.URL == "" {
				errs = append(errs, fmt.Sprintf("%s.mcp.%s: either command or url is required", scope, name))
			}
			switch m.Type {
			case "", "stdio", "http", "sse":
			default:
				errs = append(errs, fmt.Sprintf("%s.mcp.%s: type must be stdio, http or sse", scope, name))
			}
		}
		switch rs.Effort {
		case "", "low", "medium", "high", "max":
		default:
			errs = append(errs, fmt.Sprintf("%s.effort must be low, medium, high or max", scope))
		}
		if rs.PromptFile != "" {
			if _, err := os.Stat(c.resolvePath(rs.PromptFile)); err != nil {
				errs = append(errs, fmt.Sprintf("%s.prompt_file: %v", scope, err))
			}
		}
		if rs.Shell != "" {
			if st, err := os.Stat(rs.Shell); err != nil || st.IsDir() {
				errs = append(errs, fmt.Sprintf("%s.shell %q is not an executable file", scope, rs.Shell))
			}
		}
		for k := range rs.Env {
			if k == "" || strings.ContainsAny(k, "= ") {
				errs = append(errs, fmt.Sprintf("%s.env: invalid variable name %q", scope, k))
			}
		}
	}
	check("global", c.Global)
	for name, rs := range c.Roles {
		check("roles."+name, rs)
	}
	if len(errs) > 0 {
		return fmt.Errorf("invalid bees.toml:\n  - %s", strings.Join(errs, "\n  - "))
	}
	return nil
}

func mustCanonical(name string) string {
	r, _ := CanonicalRole(name)
	return r
}

// Dir returns the directory containing bees.toml.
func (c *Config) Dir() string { return filepath.Dir(c.Path) }

// StateDir returns the absolute state directory.
func (c *Config) StateDir() string { return c.resolvePath(c.Project.StateDir) }

func (c *Config) resolvePath(p string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir(), p)
}

// Role returns the resolved settings for role name (aliases accepted).
func (c *Config) Role(name string) (ResolvedRole, error) {
	canonical, err := CanonicalRole(name)
	if err != nil {
		return ResolvedRole{}, err
	}
	rs := c.Roles[canonical]
	g := c.Global

	r := ResolvedRole{
		Name:          canonical,
		Model:         firstNonEmpty(rs.Model, g.Model, DefaultModel),
		ModelBySize:   sizeModels(rs.ModelBySize),
		FallbackModel: firstNonEmpty(rs.FallbackModel, g.FallbackModel, DefaultFallbackModel),
		Effort:        firstNonEmpty(rs.Effort, g.Effort),
		MaxTurns:      firstPositive(rs.MaxTurns, g.MaxTurns, DefaultMaxTurns),
		Timeout:       firstPositiveDur(rs.Timeout.Duration, g.Timeout.Duration, DefaultTimeout),
		Enabled:       true,
		Shell:         firstNonEmpty(rs.Shell, g.Shell),
		MCP:           map[string]MCPServer{},
		Env:           map[string]string{},
	}
	for k, v := range g.Env {
		r.Env[k] = v
	}
	for k, v := range rs.Env {
		r.Env[k] = v
	}
	if rs.Enabled != nil {
		r.Enabled = *rs.Enabled
	}

	// Prompt: global text, global file, role text, role file.
	var parts []string
	for _, p := range []struct {
		text, file string
	}{{g.Prompt, g.PromptFile}, {rs.Prompt, rs.PromptFile}} {
		if s := strings.TrimSpace(p.text); s != "" {
			parts = append(parts, s)
		}
		if p.file != "" {
			b, err := os.ReadFile(c.resolvePath(p.file))
			if err != nil {
				return ResolvedRole{}, fmt.Errorf("prompt_file for %s: %w", canonical, err)
			}
			if s := strings.TrimSpace(string(b)); s != "" {
				parts = append(parts, s)
			}
		}
	}
	r.Prompt = strings.Join(parts, "\n\n")

	// Skills: union, order preserved, global first.
	seen := map[string]bool{}
	for _, s := range append(append([]string{}, g.Skills...), rs.Skills...) {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		r.Skills = append(r.Skills, s)
	}

	// MCP: union, role overrides global on name conflict.
	for k, v := range g.MCP {
		r.MCP[k] = v
	}
	for k, v := range rs.MCP {
		r.MCP[k] = v
	}

	r.AllowedTools = append(append([]string{}, g.AllowedTools...), rs.AllowedTools...)
	r.DisallowedTools = append(append([]string{}, g.DisallowedTools...), rs.DisallowedTools...)
	return r, nil
}

// MCPNames returns the server names sorted, for stable output.
func (r ResolvedRole) MCPNames() []string {
	names := make([]string, 0, len(r.MCP))
	for n := range r.MCP {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

// ModelFor returns the model to run a work item of the given size with: the
// model_by_size override when there is one, else the role's Model. An empty or
// unknown size falls back to Model.
func (r ResolvedRole) ModelFor(size string) string {
	return firstNonEmpty(r.ModelBySize[size], r.Model)
}

// sizeModels copies a model_by_size table, trimming the model names and
// dropping empty entries. It returns nil for an empty table.
func sizeModels(m map[string]string) map[string]string {
	var out map[string]string
	for size, model := range m {
		if model = strings.TrimSpace(model); model == "" {
			continue
		}
		if out == nil {
			out = make(map[string]string, len(m))
		}
		out[size] = model
	}
	return out
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func firstPositive(vals ...int) int {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}

func firstPositiveDur(vals ...time.Duration) time.Duration {
	for _, v := range vals {
		if v > 0 {
			return v
		}
	}
	return 0
}
