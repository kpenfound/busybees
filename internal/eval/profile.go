package eval

import (
	"bytes"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/review"
)

// BuiltIn is the Source of a selection no file configures.
const BuiltIn = "built-in"

// Selection is the agent profiles a run's sessions use and where they came
// from. It carries the profile table and the keys that select from it, and
// nothing else of the file it was read from: every other setting of an eval
// is the eval's own, so two runs differ by their profiles only.
type Selection struct {
	// Name is the profile the report names: --profile's, the one [global]
	// selects in bees.toml, or "default".
	Name string `json:"name"`
	// Source is the file the profiles came from, or BuiltIn.
	Source string `json:"source"`
	// Roles is what each role runs as, "<agent> <model>", for the report.
	Roles map[string]string `json:"roles"`

	profiles map[string]config.AgentProfile
	global   roleTable
	roles    map[string]roleTable
}

// String is the selection as the table shows it: "fast (bees.toml)".
func (s Selection) String() string {
	source := s.Source
	if source != BuiltIn {
		source = filepath.Base(source)
	}
	return s.Name + " (" + source + ")"
}

// SelectProfile picks the profiles an eval runs on, the way a normal run
// resolves them. A name is looked up in bees.toml's profiles first, then in
// the global config.toml's, which the user defaults file fills below it, and
// runs every role; with none, bees.toml's own selection is taken as it
// stands; without a bees.toml, the global file's provider and model; with no
// config.toml file, the selection the defaults file's [global] and [roles]
// tables make; and without that either, the built-in profile. local and
// global may be nil.
func SelectProfile(name string, local *config.Config, global *review.Config) (Selection, error) {
	var s Selection
	switch {
	case name != "":
		switch {
		case local != nil && hasProfile(local.Profiles, name):
			s = Selection{Source: local.Path, profiles: local.Profiles}
		case global != nil && hasProfile(global.Profiles, name):
			source := global.Path
			if !global.Loaded {
				// config.toml is not there: the table the lookup found
				// the name in is the defaults file's, and the selection
				// names that file.
				source = config.DefaultDefaultsPath()
			}
			s = Selection{Source: source, profiles: global.Profiles}
		default:
			return Selection{}, fmt.Errorf("--profile %q: no such profile in %s", name, strings.Join(profileSources(local, global), " or "))
		}
		s.Name = name
		s.global = roleTable{Profile: name}
	case local != nil:
		s = selectionFromTables(local.Path, local.Global, local.Roles, local.Profiles)
	case global != nil && global.Loaded:
		s = Selection{Name: "default", Source: global.Path,
			profiles: map[string]config.AgentProfile{"default": {Agent: global.Provider, Model: global.Model}},
			global:   roleTable{Profile: "default"}}
	case global != nil:
		var err error
		if s, err = defaultsSelection(); err != nil {
			return Selection{}, err
		}
	default:
		s = Selection{Name: "default", Source: BuiltIn}
	}
	// Parsed once here, with a placeholder project, so a selection that
	// cannot run is refused before any fixture is built.
	cfg, err := config.Parse(s.configText(settings{Repo: "bees-eval/check", StateDir: "state", Workspaces: "worktrees", PassInterval: DefaultPassInterval}), "bees.toml")
	if err != nil {
		return Selection{}, fmt.Errorf("the profiles from %s: %w", s.Source, err)
	}
	if err := checkHostOnly(cfg); err != nil {
		return Selection{}, err
	}
	s.Roles = map[string]string{}
	for _, role := range config.Roles {
		r, err := cfg.Role(role)
		if err != nil {
			return Selection{}, err
		}
		s.Roles[role] = strings.TrimSpace(r.Agent + " " + r.Model)
	}
	return s, nil
}

func hasProfile(profiles map[string]config.AgentProfile, name string) bool {
	_, ok := profiles[name]
	return ok
}

// selectionFromTables is the selection a bees.toml-shaped set of tables
// makes: [global]'s profile and profile_by_size, each role's own keys, and
// the [profiles] the selection names. A project's bees.toml and the user
// defaults file hold the same tables.
func selectionFromTables(source string, global config.RoleSettings, roles map[string]config.RoleSettings, profiles map[string]config.AgentProfile) Selection {
	s := Selection{Name: firstNonEmpty(global.Profile, "default"), Source: source, profiles: profiles,
		global: roleTable{Profile: global.Profile, ProfileBySize: global.ProfileBySize}}
	for _, role := range slices.Sorted(maps.Keys(roles)) {
		rs := roles[role]
		t := roleTable{Profile: rs.Profile, ProfileBySize: rs.ProfileBySize, BriefProfile: rs.BriefProfile,
			JudgeProfile: rs.JudgeProfile, AngleProfiles: rs.AngleProfiles}
		if !t.empty() {
			if s.roles == nil {
				s.roles = map[string]roleTable{}
			}
			s.roles[role] = t
		}
	}
	return s
}

// defaultsSelection is the selection the user defaults file makes below the
// global config: its [global] and [roles] tables and its profiles, named as
// the file they came from. With no defaults file there is no selection, and
// the caller takes the built-in profile.
func defaultsSelection() (Selection, error) {
	defaults, err := config.LoadUserDefaults()
	if err != nil || defaults == nil {
		return Selection{Name: "default", Source: BuiltIn}, err
	}
	return selectionFromTables(config.DefaultDefaultsPath(), defaults.Global, defaults.Roles, defaults.Profiles), nil
}

func profileSources(local *config.Config, global *review.Config) []string {
	var out []string
	if local != nil {
		out = append(out, local.Path)
	}
	if global != nil {
		out = append(out, global.Path)
	}
	if len(out) == 0 {
		out = append(out, "any configuration file")
	}
	return out
}

// checkHostOnly refuses a profile that runs sessions in a container or a
// Docker Sandbox: the eval's GitHub is reached through a gh on this host's
// PATH, which a session there does not have, and the gh it has would reach
// the real one. Every role of an eval runs, so every role is checked, on
// every size, fallback and the reviewer's judge profile; the brief and
// angle sessions ignore a profile's sandbox.
func checkHostOnly(cfg *config.Config) error {
	for _, name := range config.Roles {
		role, err := cfg.Role(name)
		if err != nil {
			return err
		}
		sandboxes := []string{}
		if role.JudgeProfile != nil {
			sandboxes = append(sandboxes, role.JudgeProfile.Sandbox)
		}
		for _, size := range append([]string{""}, config.Sizes...) {
			sized := role.ForSize(size)
			for _, r := range append([]config.ResolvedRole{sized}, sized.Fallbacks()...) {
				sandboxes = append(sandboxes, r.Sandbox)
			}
		}
		for _, sandbox := range sandboxes {
			if sandbox == agent.SandboxContainer || sandbox == agent.SandboxSbx {
				return fmt.Errorf("bees eval runs sessions on this host, and the %s role's profile uses sandbox %q: a session there cannot reach the eval's fake GitHub. Pick a profile with sandbox \"none\" or \"claude\" with --profile", name, sandbox)
			}
		}
	}
	return nil
}

// settings are the parts of an eval's bees.toml that are the eval's own.
type settings struct {
	Repo, StateDir, Workspaces string
	PassInterval               string
	// Role is the role a per-role case runs, and "" for a whole-factory
	// case: a reviewer case is given one review round (ReviewRounds).
	Role string
}

// The bees.toml an eval writes into its fixture's clone.
type fileConfig struct {
	Version   int                     `toml:"version"`
	Project   fileProject             `toml:"project"`
	Scheduler fileScheduler           `toml:"scheduler"`
	Global    roleTable               `toml:"global"`
	Roles     map[string]roleTable    `toml:"roles,omitempty"`
	Profiles  map[string]profileTable `toml:"profiles,omitempty"`
}

type fileProject struct {
	Repo          string `toml:"repo"`
	DefaultBranch string `toml:"default_branch"`
	StateDir      string `toml:"state_dir"`
}

type fileScheduler struct {
	PollInterval  string `toml:"poll_interval"`
	WorkspaceRoot string `toml:"workspace_root"`
	// MaxReviewRounds is set only for a per-role reviewer case
	// (reviewRounds); every other case leaves the key out and runs the
	// review loop the configured default number of times.
	MaxReviewRounds int `toml:"max_review_rounds,omitempty"`
}

// roleTable is the profile selection of [global] or a [roles.<name>], and
// for the reviewer the eval's own checks timings.
type roleTable struct {
	Profile       string            `toml:"profile,omitempty"`
	ProfileBySize map[string]string `toml:"profile_by_size,omitempty"`
	BriefProfile  string            `toml:"brief_profile,omitempty"`
	JudgeProfile  string            `toml:"judge_profile,omitempty"`
	AngleProfiles map[string]string `toml:"angle_profiles,omitempty"`
	// The fake GitHub reports no checks, so the reviewer has nothing to
	// wait for.
	ChecksWait         string `toml:"checks_wait,omitempty"`
	ChecksPollInterval string `toml:"checks_poll_interval,omitempty"`
}

func (t roleTable) empty() bool {
	return t.Profile == "" && len(t.ProfileBySize) == 0 && t.BriefProfile == "" && t.JudgeProfile == "" && len(t.AngleProfiles) == 0
}

// ReviewRounds is how many review rounds a per-role reviewer case runs.
// One: the round a reviewer eval measures is the review, and what the
// factory does with "changes requested" is to start a developer session,
// which a per-role run must not do — the scheduler's role scope gates
// dispatch, not the stage the review loop moves to next. With one round the
// worker escalates the issue to a person instead, and the run ends where
// the reviewer's verdict is. The session is told it is the final round,
// which every reviewer case therefore is.
const ReviewRounds = 1

// reviewRounds is the scheduler's max_review_rounds for a case of role, and
// 0 — the key left out — for every role but the reviewer.
func reviewRounds(role string) int {
	if role == config.RoleReviewer {
		return ReviewRounds
	}
	return 0
}

type profileTable struct {
	Agent    string `toml:"agent,omitempty"`
	Model    string `toml:"model,omitempty"`
	Fallback string `toml:"fallback,omitempty"`
	Effort   string `toml:"effort,omitempty"`
	Sandbox  string `toml:"sandbox,omitempty"`
}

// configText is the bees.toml of an eval with this selection.
func (s Selection) configText(set settings) string {
	f := fileConfig{
		Version:   config.CurrentVersion,
		Project:   fileProject{Repo: set.Repo, DefaultBranch: DefaultBranch, StateDir: set.StateDir},
		Scheduler: fileScheduler{PollInterval: set.PassInterval, WorkspaceRoot: set.Workspaces, MaxReviewRounds: reviewRounds(set.Role)},
		Global:    s.global,
		Roles:     maps.Clone(s.roles),
	}
	if f.Roles == nil {
		f.Roles = map[string]roleTable{}
	}
	reviewer := f.Roles[config.RoleReviewer]
	reviewer.ChecksWait, reviewer.ChecksPollInterval = "1s", "1s"
	f.Roles[config.RoleReviewer] = reviewer
	for name, p := range s.profiles {
		if f.Profiles == nil {
			f.Profiles = map[string]profileTable{}
		}
		f.Profiles[name] = profileTable{Agent: p.Agent, Model: p.Model, Fallback: p.Fallback, Effort: p.Effort, Sandbox: p.Sandbox}
	}
	var b bytes.Buffer
	b.WriteString("# Written by bees eval for one case: the profiles are the ones the run\n# selected, everything else is the eval's own.\n")
	if err := toml.NewEncoder(&b).Encode(f); err != nil {
		// Encoding plain strings and string maps does not fail.
		panic(err)
	}
	return b.String()
}
