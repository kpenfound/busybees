package config

import "maps"

// View is the resolved configuration as printed by `bees config show`. Its
// JSON field names are the bees.toml key names, so what the command prints can
// be matched against what the user wrote, and durations render as duration
// strings ("45m0s") rather than nanoseconds.
type View struct {
	Path      string              `json:"path"`
	Version   int                 `json:"version"`
	Project   Project             `json:"project"`
	Filter    FilterView          `json:"filter"`
	Scheduler Scheduler           `json:"scheduler"`
	Roles     map[string]RoleView `json:"roles"`
}

// FilterView is [filter] with require_label resolved to the bool the factory
// uses, so it never prints null.
type FilterView struct {
	Label        string `json:"label"`
	RequireLabel bool   `json:"require_label"`
	Assignee     string `json:"assignee"`
	Milestone    string `json:"milestone"`
}

// RoleView is a ResolvedRole under its bees.toml key names. The role-specific
// keys are only present on the role that owns them; skills_refresh is global
// only, so it reads the same under every role.
type RoleView struct {
	Name            string               `json:"name"`
	Prompt          string               `json:"prompt"`
	Skills          []string             `json:"skills"`
	SkillsRefresh   string               `json:"skills_refresh"`
	MCP             map[string]MCPServer `json:"mcp"`
	Model           string               `json:"model"`
	FallbackModel   string               `json:"fallback_model"`
	Effort          string               `json:"effort"`
	MaxTurns        int                  `json:"max_turns"`
	Timeout         Duration             `json:"timeout"`
	AllowedTools    []string             `json:"allowed_tools"`
	DisallowedTools []string             `json:"disallowed_tools"`
	Enabled         bool                 `json:"enabled"`
	Shell           string               `json:"shell"`
	Env             map[string]string    `json:"env"`

	// CommitFlags, MaxSize and ModelBySize are only set on the developer.
	CommitFlags *string            `json:"commit_flags,omitempty"`
	MaxSize     *string            `json:"max_size,omitempty"`
	ModelBySize *map[string]string `json:"model_by_size,omitempty"`
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
}

// View resolves the configuration for the named roles.
func (c *Config) View(roles []string) (View, error) {
	v := View{
		Path:      c.Path,
		Version:   c.Version,
		Project:   c.Project,
		Filter:    FilterView{Label: c.Filter.Label, RequireLabel: c.Filter.LabelRequired(), Assignee: c.Filter.Assignee, Milestone: c.Filter.Milestone},
		Scheduler: c.Scheduler,
		Roles:     map[string]RoleView{},
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
			Name:            rr.Name,
			Prompt:          rr.Prompt,
			Skills:          rr.Skills,
			SkillsRefresh:   c.SkillsRefreshPolicy(),
			MCP:             rr.MCP,
			Model:           rr.Model,
			FallbackModel:   rr.FallbackModel,
			Effort:          rr.Effort,
			MaxTurns:        rr.MaxTurns,
			Timeout:         Duration{rr.Timeout},
			AllowedTools:    rr.AllowedTools,
			DisallowedTools: rr.DisallowedTools,
			Enabled:         rr.Enabled,
			Shell:           rr.Shell,
			Env:             rr.Env,
		}
		// Empty collections print as [] / {} rather than null.
		if rv.Skills == nil {
			rv.Skills = []string{}
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
		switch rr.Name {
		case RoleDeveloper:
			flags := c.CommitFlags()
			rv.CommitFlags = &flags
			size := c.MaxSize()
			rv.MaxSize = &size
			bySize := map[string]string{}
			maps.Copy(bySize, rr.ModelBySize)
			rv.ModelBySize = &bySize
		case RoleReviewer:
			m := c.Merge()
			rv.MergeView = &MergeView{
				AutoMerge:          m.AutoMerge,
				MergeMethod:        m.Method,
				ChecksWait:         Duration{m.ChecksWait},
				ChecksPollInterval: Duration{m.ChecksPollInterval},
				ChecksTimeout:      Duration{m.ChecksTimeout},
				MaxCheckFixRounds:  m.MaxCheckFixRounds,
			}
		}
		v.Roles[rr.Name] = rv
	}
	return v, nil
}
