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
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

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
	DefaultRateLimitWait = 15 * time.Minute
	DefaultMaxDevelopers = 1
	DefaultReviewRounds  = 3
	DefaultPMInterval    = time.Hour
	DefaultQAInterval    = 30 * time.Minute
	DefaultTriageBatch   = 5
)

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
	Remote string `toml:"remote"`
	// Repo is the GitHub repository in "owner/name" form. Derived from the
	// remote's URL when empty (see Config.Resolve).
	Repo string `toml:"repo"`
	// DefaultBranch is the branch developers branch from and QA tests.
	// Derived from the remote's HEAD when empty.
	DefaultBranch string `toml:"default_branch"`
	// StateDir is where mail, notes, logs and scheduler state live. Relative
	// paths are resolved against the directory containing bees.toml.
	StateDir string `toml:"state_dir"`
	// BranchPrefix is prepended to developer branches, e.g. "bees/issue-12".
	BranchPrefix string `toml:"branch_prefix"`
}

// Filter selects which GitHub issues and pull requests the factory can see.
// All configured criteria must match (they are ANDed).
type Filter struct {
	// Label is the factory's label. It is the base name of the workflow state
	// labels ("bees:ready", ...) and, when RequireLabel is true, the
	// visibility gate: only issues/PRs carrying it are visible. Default "bees".
	Label string `toml:"label"`
	// RequireLabel can be set to false so that Assignee and/or Milestone
	// alone define visibility. The factory still applies Label to everything
	// it creates. Default true.
	RequireLabel *bool `toml:"require_label"`
	// Assignee restricts visibility to issues/PRs assigned to this GitHub
	// login ("@me" resolves to the authenticated gh user). Everything the
	// factory creates is assigned to this user so it stays visible.
	Assignee string `toml:"assignee"`
	// Milestone restricts visibility to issues/PRs in this milestone title.
	Milestone string `toml:"milestone"`
}

// LabelRequired reports whether the label is part of the visibility gate.
func (f Filter) LabelRequired() bool { return f.RequireLabel == nil || *f.RequireLabel }

// MCPServer configures one MCP server. Either Command (stdio) or URL (http/sse)
// must be set.
type MCPServer struct {
	Type    string            `toml:"type"` // stdio (default when command set), http, sse
	Command string            `toml:"command"`
	Args    []string          `toml:"args"`
	Env     map[string]string `toml:"env"`
	URL     string            `toml:"url"`
	Headers map[string]string `toml:"headers"`
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

	// The following key is only valid under [roles.developer].

	// CommitFlags are extra flags the developer passes to every `git commit`,
	// for example "--gpg-sign --signoff". Appended to its system prompt.
	CommitFlags string `toml:"commit_flags"`

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

// CommitFlags returns the developer's extra git commit flags.
func (c *Config) CommitFlags() string { return strings.TrimSpace(c.Roles[RoleDeveloper].CommitFlags) }

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

// Scheduler configures the orchestrator loop.
type Scheduler struct {
	// PollInterval is how often GitHub is polled for work. Each poll costs
	// two API calls (open issues, open PRs); everything else is gated on what
	// those lists report. Default 5m.
	PollInterval Duration `toml:"poll_interval"`
	// RateLimitBackoff is how long to pause polling after GitHub reports a
	// rate limit. Default 15m.
	RateLimitBackoff Duration `toml:"rate_limit_backoff"`
	// MaxDevelopers is the number of concurrent developer workers. Each worker
	// runs a sequential developer <-> reviewer loop for one issue at a time.
	MaxDevelopers int `toml:"max_developers"`
	// MaxReviewRounds caps developer/reviewer iterations before an issue is
	// escalated with the needs-human label.
	MaxReviewRounds int `toml:"max_review_rounds"`
	// ProductManagerInterval is the minimum time between product manager runs
	// (mail in the PM inbox triggers an earlier run).
	ProductManagerInterval Duration `toml:"product_manager_interval"`
	// QAInterval is the minimum time between QA runs. QA only runs when
	// something has been merged since its last run.
	QAInterval Duration `toml:"qa_interval"`
	// TriageBatchSize is the maximum number of issues handed to the project
	// manager in one session.
	TriageBatchSize int `toml:"triage_batch_size"`
	// KeepWorkspaces leaves temp worktrees on disk after a session (debugging).
	KeepWorkspaces bool `toml:"keep_workspaces"`
	// WorkspaceRoot overrides the temp dir used for worktrees.
	WorkspaceRoot string `toml:"workspace_root"`
}

// ResolvedRole is the effective configuration for one role after merging
// [global] and [roles.<name>].
type ResolvedRole struct {
	Name            string
	Prompt          string // global prompt + role prompt (+ prompt files)
	Skills          []string
	MCP             map[string]MCPServer
	Model           string
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
	text := string(data)
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
	check := func(scope string, rs RoleSettings) {
		if scope != "roles."+RoleReviewer {
			if rs.AutoMerge != nil || rs.MergeMethod != "" || rs.ChecksWait.Duration != 0 || rs.ChecksPollInterval.Duration != 0 || rs.ChecksTimeout.Duration != 0 || rs.MaxCheckFixRounds != 0 {
				errs = append(errs, fmt.Sprintf("%s: auto_merge, merge_method, checks_wait, checks_poll_interval, checks_timeout and max_check_fix_rounds are only valid under roles.reviewer", scope))
			}
		}
		if scope != "roles."+RoleDeveloper && rs.CommitFlags != "" {
			errs = append(errs, fmt.Sprintf("%s: commit_flags is only valid under roles.developer", scope))
		}
		switch rs.MergeMethod {
		case "", "squash", "merge", "rebase":
		default:
			errs = append(errs, fmt.Sprintf("%s.merge_method must be squash, merge or rebase", scope))
		}
		for name, m := range rs.MCP {
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
