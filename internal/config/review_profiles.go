package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

func (c *Config) reviewProfile(name string) *AgentProfile {
	if name == "" {
		return nil
	}
	p := c.Profiles[name].resolved()
	return &p
}

func (c *Config) reviewAngleProfiles(names map[string]string) map[string]AgentProfile {
	out := map[string]AgentProfile{}
	for angle, name := range names {
		out[angle] = c.Profiles[name].resolved()
	}
	return out
}

// AgentProfile returns only the five execution settings, never role permissions.
func (r ResolvedRole) AgentProfile() AgentProfile {
	return AgentProfile{Agent: r.Agent, Model: r.Model, Fallback: r.Fallback, Effort: r.Effort, Sandbox: r.Sandbox}
}

func (r ResolvedRole) withReviewProfile(p *AgentProfile) ResolvedRole {
	if p != nil {
		r = r.withProfile(*p)
	}
	return r
}

// ForBrief selects execution settings after ForSize. The host adapter ignores
// Sandbox and supplies its mandatory read-only policy; role settings stay owned
// by the workflow, not by the profile.
func (r ResolvedRole) ForBrief() ResolvedRole { return r.withReviewProfile(r.BriefProfile) }

// ForAngle selects an angle override or retains the size-resolved fallback.
func (r ResolvedRole) ForAngle(angle string) ResolvedRole {
	if p, ok := r.AngleProfiles[angle]; ok {
		return r.withReviewProfile(&p)
	}
	return r
}

// ForJudge applies all five fields to the ordinary reviewer session.
func (r ResolvedRole) ForJudge() ResolvedRole { return r.withReviewProfile(r.JudgeProfile) }

func (c *Config) validateReviewProfiles() []string {
	r, err := c.Role(RoleReviewer)
	if err != nil {
		return nil
	}
	var errs []string
	// The read-only floor holds on whatever profile a fallback lands on, so
	// the chain a profile starts is held to the same agents it is.
	check := func(path string, p AgentProfile) {
		if p.Agent != AgentClaude && p.Agent != AgentCodex {
			errs = append(errs, fmt.Sprintf("%s: brief and angle sessions require agent claude or codex, got %q", path, p.Agent))
		}
		for _, name := range fallbackChain(c.resolvedProfiles(), c.profileNamed(p), p.Fallback) {
			if f := c.Profiles[name].resolved(); f.Agent != AgentClaude && f.Agent != AgentCodex {
				errs = append(errs, fmt.Sprintf("%s: brief and angle sessions require agent claude or codex, and fallback profile %q runs %q", path, name, f.Agent))
			}
		}
	}
	if r.BriefProfile != nil {
		check("roles.reviewer.brief_profile", *r.BriefProfile)
	}
	for _, angle := range slices.Sorted(maps.Keys(r.AngleProfiles)) {
		check("roles.reviewer.angle_profiles."+angle, r.AngleProfiles[angle])
	}
	// An absent phase override falls back after ordinary size selection. Validate
	// every possible fallback, even sizes not selected by the current work item.
	if r.Enabled && (r.BriefProfile == nil || len(r.AngleProfiles) < len(KnownReviewAngles)) {
		check("roles.reviewer.profile", r.AgentProfile())
		for _, size := range slices.Sorted(maps.Keys(r.ProfilesBySize)) {
			check("roles.reviewer.profile_by_size."+size, r.ProfilesBySize[size])
		}
	}
	return errs
}

// profileNamed is the name of the profile p is in the table, or "" for the
// implicit built-in profile, which is in no table.
func (c *Config) profileNamed(p AgentProfile) string {
	for _, name := range slices.Sorted(maps.Keys(c.Profiles)) {
		if c.Profiles[name].resolved() == p {
			return name
		}
	}
	return ""
}

// migrateReviewProfiles is version 3 -> 4. Rewrite complete TOML statements so
// dotted keys, subtables, inline tables and prompts containing examples survive.
// The ordinary (unsized) reviewer profile supplies the five fallback fields;
// phase model overrides become independent profiles, like ordinary profiles.
func migrateReviewProfiles(text string) (string, error) {
	type scope struct {
		Profile string `toml:"profile"`
	}
	var c struct {
		Global   scope                         `toml:"global"`
		Roles    map[string]scope              `toml:"roles"`
		Profiles map[string]legacyAgentProfile `toml:"profiles"`
	}
	md, err := toml.Decode(text, &c)
	if err != nil {
		return "", err
	}
	for old, current := range map[string]string{"brief_model": "brief_profile", "judge_model": "judge_profile", "angle_models": "angle_profiles"} {
		if md.IsDefined("roles", "reviewer", old) && md.IsDefined("roles", "reviewer", current) {
			return "", fmt.Errorf("roles.reviewer.%s: cannot mix legacy model and profile settings", old)
		}
	}
	fallback := c.Profiles[firstNonEmpty(c.Roles[RoleReviewer].Profile, c.Global.Profile)].resolved()
	profiles := maps.Clone(c.Profiles)
	if profiles == nil {
		profiles = map[string]legacyAgentProfile{}
	}
	var blocks strings.Builder
	add := func(base, model string) string {
		p := fallback
		p.Model = strings.TrimSpace(model)
		for _, name := range slices.Sorted(maps.Keys(profiles)) {
			if profiles[name].resolved() == p {
				return name
			}
		}
		name := base
		for n := 2; ; n++ {
			if _, exists := profiles[name]; !exists {
				break
			}
			name = fmt.Sprintf("%s_%d", base, n)
		}
		profiles[name] = p
		fmt.Fprintf(&blocks, "\n[%s]\n", toml.Key{"profiles", name}.String())
		_ = toml.NewEncoder(&blocks).Encode(p)
		return name
	}
	statements, err := profileStatements(text)
	if err != nil {
		return "", err
	}
	var rewrite func([]string, any) (any, bool, error)
	rename := func(path []string) []string {
		p := slices.Clone(path)
		if len(p) >= 3 && p[0] == "roles" && p[1] == "reviewer" {
			switch p[2] {
			case "brief_model":
				p[2] = "brief_profile"
			case "judge_model":
				p[2] = "judge_profile"
			case "angle_models":
				p[2] = "angle_profiles"
			}
		}
		return p
	}
	rewrite = func(path []string, value any) (any, bool, error) {
		if table, ok := value.(map[string]any); ok {
			changed := false
			out := map[string]any{}
			for _, key := range slices.Sorted(maps.Keys(table)) {
				child := append(slices.Clone(path), key)
				v, did, err := rewrite(child, table[key])
				if err != nil {
					return nil, false, err
				}
				newPath := rename(child)
				newKey := newPath[len(newPath)-1]
				if newKey != key {
					if _, exists := table[newKey]; exists {
						return nil, false, fmt.Errorf("%s: cannot mix legacy model and profile settings", strings.Join(child, "."))
					}
				}
				out[newKey] = v
				changed = changed || did || newKey != key
			}
			return out, changed, nil
		}
		renamed := rename(path)
		if slices.Equal(path, renamed) {
			return value, false, nil
		}
		model, ok := value.(string)
		if !ok {
			return nil, false, fmt.Errorf("%s must name a model", strings.Join(path, "."))
		}
		if strings.TrimSpace(model) == "" {
			if path[2] == "angle_models" {
				return nil, false, fmt.Errorf("%s must name a model", strings.Join(path, "."))
			}
			return "", true, nil
		}
		return add("reviewer_"+strings.Join(renamed[2:], "_"), model), true, nil
	}
	var out strings.Builder
	for _, st := range statements {
		path := rename(st.path)
		if st.raw == nil {
			if !slices.Equal(path, st.path) {
				fmt.Fprintf(&out, "# Configure %s through [profiles.<name>].\n", strings.Join(path, "."))
				// Keep the comment text, updating its obsolete key.
				out.WriteString(strings.Replace(st.text, st.path[2], path[2], 1))
			} else {
				out.WriteString(st.text)
			}
			continue
		}
		if st.header {
			if !slices.Equal(path, st.path) {
				fmt.Fprintf(&out, "# Previous: %s\n[%s]\n", strings.TrimSuffix(st.text, "\n"), toml.Key(path).String())
			} else {
				out.WriteString(st.text)
			}
			continue
		}
		key := st.path[st.sectionDepth:]
		var assigned any = st.raw
		for _, part := range key {
			assigned = assigned.(map[string]any)[part]
		}
		value, changed, err := rewrite(st.path, assigned)
		if err != nil {
			return "", err
		}
		if !changed {
			out.WriteString(st.text)
			continue
		}
		for _, line := range strings.Split(strings.TrimSuffix(st.text, "\n"), "\n") {
			fmt.Fprintln(&out, "# Previous: "+line)
		}
		encoded, err := inlineProfileValue(value)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "%s = %s\n", toml.Key(path[st.sectionDepth:]).String(), encoded)
	}
	out.WriteString(blocks.String())
	return out.String(), nil
}
