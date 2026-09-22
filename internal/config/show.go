package config

import (
	"maps"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// View is the resolved configuration as printed by `bees config show`. Its
// JSON field names follow bees.toml, with profiles_by_size showing each size
// override as resolved agent settings. Durations render as duration
// strings ("45m0s") rather than nanoseconds.
type View struct {
	Path      string                  `json:"path"`
	Version   int                     `json:"version"`
	Project   Project                 `json:"project"`
	Filter    FilterView              `json:"filter"`
	GitHub    GitHubView              `json:"github"`
	Scheduler Scheduler               `json:"scheduler"`
	Logging   Logging                 `json:"logging"`
	Notes     Notes                   `json:"notes"`
	Roles     map[string]RoleView     `json:"roles"`
	Profiles  map[string]AgentProfile `json:"profiles"`

	// ProfileSources names, per profile, the defaults.toml that defined it.
	// Profiles the project file declared are absent.
	ProfileSources map[string]string `json:"profile_sources,omitempty"`
}

// FilterView is [filter] with require_label resolved to the bool the factory
// uses, so it never prints null.
type FilterView struct {
	Label        string `json:"label"`
	RequireLabel bool   `json:"require_label"`
	Assignee     string `json:"assignee"`
	Milestone    string `json:"milestone"`
	Creator      string `json:"creator"`
}

// GitHubView is [github] as printed. The token is redacted (see
// GitHub.RedactedToken): the resolved secret must never reach `bees config
// show`, whose output people paste into issues.
type GitHubView struct {
	Login    string `json:"login"`
	Token    string `json:"token"`
	GitName  string `json:"git_name"`
	GitEmail string `json:"git_email"`
}

// RoleView is a ResolvedRole under its bees.toml key names. The role-specific
// keys are only present on the role that owns them; skills_refresh is global
// only, so it reads the same under every role.
type RoleView struct {
	Name                    string               `json:"name"`
	Prompt                  string               `json:"prompt"`
	Skills                  []string             `json:"skills"`
	PiPackages              []string             `json:"pi_packages"`
	SkillsRefresh           string               `json:"skills_refresh"`
	MCP                     map[string]MCPServer `json:"mcp"`
	Model                   string               `json:"model"`
	Fallback                string               `json:"fallback"`
	Agent                   string               `json:"agent"`
	Effort                  string               `json:"effort"`
	MaxTurns                int                  `json:"max_turns"`
	Timeout                 Duration             `json:"timeout"`
	AllowedTools            []string             `json:"allowed_tools"`
	DisallowedTools         []string             `json:"disallowed_tools"`
	Enabled                 bool                 `json:"enabled"`
	Shell                   string               `json:"shell"`
	Env                     map[string]string    `json:"env"`
	Sandbox                 string               `json:"sandbox"`
	SandboxImage            string               `json:"sandbox_image"`
	ContainerUseEnvironment string               `json:"container_use_environment"`
	SandboxDaggerEngine     string               `json:"sandbox_dagger_engine"`
	SandboxDaggerVersion    string               `json:"sandbox_dagger_version"`

	// ProfilesBySize describes the effective size overrides for every role.
	ProfilesBySize map[string]AgentProfile `json:"profiles_by_size"`

	// ProfileSources names the defaults.toml behind every selector it set:
	// profile and profile_by_size on every role, the reviewer's
	// brief_profile, judge_profile, angle_profiles and angles besides, the
	// map selectors per entry. A selection the project file made is absent,
	// as is everything built in.
	ProfileSources *RoleProfileSources `json:"profile_sources,omitempty"`

	// CommitFlags, MaxSize and the best-of-N and
	// mixture-of-experts keys are only set on the developer.
	CommitFlags     *string         `json:"commit_flags,omitempty"`
	MaxSize         *string         `json:"max_size,omitempty"`
	BestOfNBySize   *map[string]int `json:"best_of_n_by_size,omitempty"`
	BestOfNModel    *string         `json:"best_of_n_model,omitempty"`
	BestOfNPrompt   *string         `json:"best_of_n_prompt,omitempty"`
	AssemblerModel  *string         `json:"assembler_model,omitempty"`
	AssemblerPrompt *string         `json:"assembler_prompt,omitempty"`

	MoEExpertsBySize   *map[string][]string  `json:"moe_experts_by_size,omitempty"`
	MoEExperts         *map[string]MoEExpert `json:"moe_experts,omitempty"`
	MoEAssemblerModel  *string               `json:"moe_assembler_model,omitempty"`
	MoEAssemblerPrompt *string               `json:"moe_assembler_prompt,omitempty"`
	// MinIssueSize is only set on the product manager.
	MinIssueSize *string `json:"min_issue_size,omitempty"`
	// Review phase profiles include the resolved fallback at each size.
	Angles *map[string][]string `json:"angles,omitempty"`
	*ReviewProfilesView
	ReviewProfilesBySize map[string]ReviewProfilesView `json:"review_profiles_by_size,omitempty"`
	// MergeView is only set on the reviewer; its keys are inlined.
	*MergeView
}

// MergeView is the resolved [roles.reviewer] merge policy.
type MergeView struct {
	AutoMerge          bool     `json:"auto_merge"`
	MergeMethod        string   `json:"merge_method"`
	ChecksWait         Duration `json:"checks_wait"`
	ChecksPollInterval Duration `json:"checks_poll_interval"`
	ChecksTimeout      Duration `json:"checks_timeout"`
	MaxCheckFixRounds  int      `json:"max_check_fix_rounds"`

	PreReviewChecks        bool     `json:"pre_review_checks"`
	PreReviewChecksTimeout Duration `json:"pre_review_checks_timeout"`
}

// RoleProfileSources is a role's profile selectors as the user-level
// defaults.toml set them, under their bees.toml key names and per map entry.
// A selection the project file made, and every value built in, are absent.
type RoleProfileSources struct {
	Profile       string            `json:"profile,omitempty"`
	ProfileBySize map[string]string `json:"profile_by_size,omitempty"`
	BriefProfile  string            `json:"brief_profile,omitempty"`
	JudgeProfile  string            `json:"judge_profile,omitempty"`
	AngleProfiles map[string]string `json:"angle_profiles,omitempty"`
	Angles        map[string]string `json:"angles,omitempty"`
}

// View resolves the configuration for the named roles.
func (c *Config) View(roles []string) (View, error) {
	v := View{
		Path:      c.Path,
		Version:   c.Version,
		Project:   c.Project,
		Filter:    FilterView{Label: c.Filter.Label, RequireLabel: c.Filter.LabelRequired(), Assignee: c.Filter.Assignee, Milestone: c.Filter.Milestone, Creator: c.Filter.Creator},
		GitHub:    GitHubView{Login: c.GitHub.Login, Token: c.GitHub.RedactedToken(), GitName: c.GitHub.GitName, GitEmail: c.GitHub.GitEmail},
		Scheduler: c.Scheduler,
		Logging:   c.Logging,
		Notes:     c.Notes.redacted(),
		Roles:     map[string]RoleView{},
		Profiles:  maps.Clone(c.Profiles),
	}
	if v.Profiles == nil {
		v.Profiles = map[string]AgentProfile{}
	}
	if c.defaultsPath != "" {
		sources := map[string]string{}
		for name := range v.Profiles {
			if c.sources[toml.Key{"profiles", name}.String()] == c.defaultsPath {
				sources[name] = c.defaultsPath
			}
		}
		if len(sources) > 0 {
			v.ProfileSources = sources
		}
	}
	if v.Scheduler.WorkDays == nil {
		v.Scheduler.WorkDays = []string{}
	}
	for _, name := range roles {
		rr, err := c.Role(name)
		if err != nil {
			return View{}, err
		}
		rv := RoleView{
			Name:                    rr.Name,
			ProfilesBySize:          maps.Clone(rr.ProfilesBySize),
			Prompt:                  rr.Prompt,
			Skills:                  rr.Skills,
			PiPackages:              rr.PiPackages,
			SkillsRefresh:           c.SkillsRefreshPolicy(),
			MCP:                     rr.MCP,
			Model:                   rr.Model,
			Fallback:                rr.Fallback,
			Agent:                   rr.Agent,
			Effort:                  rr.Effort,
			MaxTurns:                rr.MaxTurns,
			Timeout:                 Duration{rr.Timeout},
			AllowedTools:            rr.AllowedTools,
			DisallowedTools:         rr.DisallowedTools,
			Enabled:                 rr.Enabled,
			Shell:                   rr.Shell,
			Env:                     rr.Env,
			Sandbox:                 rr.Sandbox,
			SandboxImage:            rr.SandboxImage,
			ContainerUseEnvironment: rr.ContainerUseEnvironment,
			SandboxDaggerEngine:     rr.SandboxDaggerEngine,
			SandboxDaggerVersion:    rr.SandboxDaggerVersion,
		}
		if rv.ProfilesBySize == nil {
			rv.ProfilesBySize = map[string]AgentProfile{}
		}
		// Empty collections print as [] / {} rather than null.
		if rv.Skills == nil {
			rv.Skills = []string{}
		}
		if rv.PiPackages == nil {
			rv.PiPackages = []string{}
		}
		if rv.AllowedTools == nil {
			rv.AllowedTools = []string{}
		}
		if rv.DisallowedTools == nil {
			rv.DisallowedTools = []string{}
		}
		if rv.MCP == nil {
			rv.MCP = map[string]MCPServer{}
		}
		if rv.Env == nil {
			rv.Env = map[string]string{}
		}
		rv.ProfileSources = c.roleProfileSources(rr.Name)
		switch rr.Name {
		case RoleProductManager:
			min := c.MinIssueSize()
			rv.MinIssueSize = &min
		case RoleDeveloper:
			flags := c.CommitFlags()
			rv.CommitFlags = &flags
			size := c.MaxSize()
			rv.MaxSize = &size
			nBySize := map[string]int{}
			maps.Copy(nBySize, rr.BestOfNBySize)
			rv.BestOfNBySize = &nBySize
			rv.BestOfNModel = &rr.BestOfNModel
			rv.BestOfNPrompt = &rr.BestOfNPrompt
			rv.AssemblerModel = &rr.AssemblerModel
			rv.AssemblerPrompt = &rr.AssemblerPrompt
			expertsBySize := map[string][]string{}
			maps.Copy(expertsBySize, rr.MoEExpertsBySize)
			rv.MoEExpertsBySize = &expertsBySize
			experts := map[string]MoEExpert{}
			maps.Copy(experts, rr.MoEExpertsByName)
			rv.MoEExperts = &experts
			rv.MoEAssemblerModel = &rr.MoEAssemblerModel
			rv.MoEAssemblerPrompt = &rr.MoEAssemblerPrompt
		case RoleReviewer:
			angles := map[string][]string{}
			maps.Copy(angles, rr.Angles)
			rv.Angles = &angles
			phases := reviewProfilesView(rr)
			rv.ReviewProfilesView = &phases
			rv.ReviewProfilesBySize = map[string]ReviewProfilesView{}
			for _, size := range Sizes {
				rv.ReviewProfilesBySize[size] = reviewProfilesView(rr.ForSize(size))
			}

			m := c.Merge()
			rv.MergeView = &MergeView{
				AutoMerge:          m.AutoMerge,
				MergeMethod:        m.Method,
				ChecksWait:         Duration{m.ChecksWait},
				ChecksPollInterval: Duration{m.ChecksPollInterval},
				ChecksTimeout:      Duration{m.ChecksTimeout},
				MaxCheckFixRounds:  m.MaxCheckFixRounds,

				PreReviewChecks:        m.PreReviewChecks,
				PreReviewChecksTimeout: Duration{m.PreReviewChecksTimeout},
			}
		}
		v.Roles[rr.Name] = rv
	}
	return v, nil
}

// ReviewProfilesView shows the selected profile data. Brief/angle sandbox values
// are descriptive only: the host adapter enforces read-only regardless of them.
type ReviewProfilesView struct {
	BriefProfile     AgentProfile            `json:"brief_profile"`
	JudgeProfile     AgentProfile            `json:"judge_profile"`
	AngleProfiles    map[string]AgentProfile `json:"angle_profiles"`
	HostReviewPolicy string                  `json:"host_review_policy"`
}

func reviewProfilesView(r ResolvedRole) ReviewProfilesView {
	v := ReviewProfilesView{BriefProfile: r.ForBrief().AgentProfile(), JudgeProfile: r.ForJudge().AgentProfile(), AngleProfiles: map[string]AgentProfile{}, HostReviewPolicy: "brief/angles: sandbox ignored; read-only checkout; no commands, writes, web/network tools, MCP, factory identity or writable/shared VCS"}
	for _, angle := range KnownReviewAngles {
		v.AngleProfiles[angle] = r.ForAngle(angle).AgentProfile()
	}
	return v
}

// roleProfileSources marks the profile selectors defaults.toml set for one
// role. It resolves each selector the way Role does and reads the file the
// merged key came from (Config.sources, which the defaults.toml merge fills),
// so the marking sits on the same value the selector chose. A role nothing in
// defaults.toml reaches has no sources.
func (c *Config) roleProfileSources(name string) *RoleProfileSources {
	if c.defaultsPath == "" {
		return nil
	}
	rs := c.Roles[name]
	g := c.Global
	out := &RoleProfileSources{}
	base := firstNonEmpty(rs.Profile, g.Profile)
	if base != "" {
		key := []string{"global", "profile"}
		if rs.Profile != "" {
			key = []string{"roles", name, "profile"}
		}
		out.Profile = c.defaultedSource(key...)
	}
	for _, size := range Sizes {
		if firstNonEmpty(rs.ProfileBySize[size], rs.Profile, g.ProfileBySize[size], g.Profile) == base {
			continue // no size override, nothing printed to mark
		}
		key := []string{"global", "profile_by_size", size}
		if rs.ProfileBySize[size] != "" {
			key = []string{"roles", name, "profile_by_size", size}
		}
		if s := c.defaultedSource(key...); s != "" {
			if out.ProfileBySize == nil {
				out.ProfileBySize = map[string]string{}
			}
			out.ProfileBySize[size] = s
		}
	}
	if name == RoleReviewer {
		if rs.BriefProfile != "" {
			out.BriefProfile = c.defaultedSource("roles", name, "brief_profile")
		}
		if rs.JudgeProfile != "" {
			out.JudgeProfile = c.defaultedSource("roles", name, "judge_profile")
		}
		for _, angle := range slices.Sorted(maps.Keys(rs.AngleProfiles)) {
			if s := c.defaultedSource("roles", name, "angle_profiles", angle); s != "" {
				if out.AngleProfiles == nil {
					out.AngleProfiles = map[string]string{}
				}
				out.AngleProfiles[angle] = s
			}
		}
		for _, size := range Sizes {
			if len(rs.Angles[size]) == 0 {
				continue
			}
			if s := c.defaultedSource("roles", name, "angles", size); s != "" {
				if out.Angles == nil {
					out.Angles = map[string]string{}
				}
				out.Angles[size] = s
			}
		}
	}
	if out.Profile == "" && out.ProfileBySize == nil && out.BriefProfile == "" &&
		out.JudgeProfile == "" && out.AngleProfiles == nil && out.Angles == nil {
		return nil
	}
	return out
}

// defaultedSource names defaults.toml when the merged key came from that
// file, and is empty when the project supplied the key or nothing did.
func (c *Config) defaultedSource(key ...string) string {
	if c.defaultsPath == "" || c.sources[strings.Join(key, ".")] != c.defaultsPath {
		return ""
	}
	return c.defaultsPath
}
