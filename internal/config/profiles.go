package config

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"
)

// AgentProfile bundles the settings that select and configure an agent.
// Profiles are independent: omitted values use built-in defaults, never
// settings from another profile. Model defaults depend on this profile's agent.
type AgentProfile struct {
	Agent         string `toml:"agent" json:"agent"`
	Model         string `toml:"model" json:"model"`
	FallbackModel string `toml:"fallback_model" json:"fallback_model"`
	Effort        string `toml:"effort" json:"effort"`
	Sandbox       string `toml:"sandbox" json:"sandbox"`
}

func (p AgentProfile) resolved() AgentProfile {
	p.Agent = firstNonEmpty(p.Agent, DefaultAgent)
	p.Sandbox = firstNonEmpty(p.Sandbox, DefaultSandbox)
	model, fallback := "", ""
	if p.Agent == AgentClaude {
		model, fallback = DefaultModel, DefaultFallbackModel
	}
	p.Model = firstNonEmpty(p.Model, model)
	p.FallbackModel = firstNonEmpty(p.FallbackModel, fallback)
	return p
}

// legacyProfileScope exists only to read version 2's independently inherited
// settings. Resolve them before creating profiles so an agent override does
// not accidentally inherit Claude's implicit model defaults.
type legacyProfileScope struct {
	AgentProfile
	ModelBySize map[string]string `toml:"model_by_size"`
}

func legacyProfile(role, global legacyProfileScope) AgentProfile {
	return (AgentProfile{
		Agent:         firstNonEmpty(role.Agent, global.Agent),
		Model:         firstNonEmpty(role.Model, global.Model),
		FallbackModel: firstNonEmpty(role.FallbackModel, global.FallbackModel),
		Effort:        firstNonEmpty(role.Effort, global.Effort),
		Sandbox:       firstNonEmpty(role.Sandbox, global.Sandbox),
	}).resolved()
}

var legacyProfileKeys = []string{"agent", "model", "fallback_model", "effort", "sandbox", "model_by_size"}

// migrateAgentProfiles rewrites only the obsolete settings, retaining the
// surrounding text. Original setting text becomes a historical comment so
// inline comments survive too; commented defaults receive migration guidance.
func migrateAgentProfiles(text string) (string, error) {
	var old struct {
		Global   legacyProfileScope            `toml:"global"`
		Roles    map[string]legacyProfileScope `toml:"roles"`
		Profiles map[string]AgentProfile       `toml:"profiles"`
	}
	md, err := toml.Decode(text, &old)
	if err != nil {
		return "", err
	}
	profiles := maps.Clone(old.Profiles)
	if profiles == nil {
		profiles = map[string]AgentProfile{}
	}
	refs := map[string]string{}
	var blocks strings.Builder
	scopes := append([]string{"global"}, slices.Sorted(maps.Keys(old.Roles))...)
	for _, name := range scopes {
		scope, path, rs := "global", []string{"global"}, old.Global
		if name != "global" {
			scope, path, rs = "roles."+name, []string{"roles", name}, old.Roles[name]
		}
		present := false
		for _, key := range legacyProfileKeys {
			if md.IsDefined(append(slices.Clone(path), key)...) {
				present = true
			}
		}
		if !present {
			continue
		}
		if md.IsDefined(append(slices.Clone(path), "profile")...) || md.IsDefined(append(slices.Clone(path), "profile_by_size")...) {
			return "", fmt.Errorf("%s: cannot mix legacy agent settings with profile or profile_by_size; migrate the legacy settings first", scope)
		}
		if len(rs.ModelBySize) > 0 && scope != "roles.developer" {
			return "", fmt.Errorf("%s: model_by_size is only valid under roles.developer", scope)
		}
		add := func(base string, p AgentProfile) string {
			name := base
			for n := 2; ; n++ {
				if _, exists := profiles[name]; !exists {
					break
				}
				name = fmt.Sprintf("%s_%d", base, n)
			}
			profiles[name] = p
			fmt.Fprintf(&blocks, "\n[%s]\n", toml.Key{"profiles", name}.String())
			_ = toml.NewEncoder(&blocks).Encode(p) // five strings; encoding cannot fail
			return name
		}
		base := legacyProfile(rs, old.Global)
		refs[scope] = "profile = " + tomlQuote(add(name, base)) + "\n"
		if len(rs.ModelBySize) > 0 {
			entries := []string{}
			for _, size := range slices.Sorted(maps.Keys(rs.ModelBySize)) {
				if !slices.Contains(Sizes, size) {
					return "", fmt.Errorf("%s.model_by_size: unknown size %q (want one of %s)", scope, size, strings.Join(Sizes, ", "))
				}
				model := strings.TrimSpace(rs.ModelBySize[size])
				if model == "" {
					return "", fmt.Errorf("%s.model_by_size.%s must name a model", scope, size)
				}
				p := base
				p.Model = model
				entries = append(entries, size+" = "+tomlQuote(add(name+"_"+size, p)))
			}
			refs[scope] += "profile_by_size = { " + strings.Join(entries, ", ") + " }\n"
		}
	}
	statements, err := profileStatements(text)
	if err != nil {
		return "", err
	}
	// A scope can be an inline table (global = {...}, or developer = {...}
	// inside [roles]). Rewrite that statement in place; inline tables cannot
	// be extended by a separate profile assignment elsewhere in the file.
	inlineScopes := map[string]bool{}
	for i := range statements {
		st := &statements[i]
		if st.header || st.raw == nil {
			continue
		}
		changed := false
		for scope, ref := range refs {
			path := strings.Split(scope, ".")
			if len(st.path) > len(path) || !slices.Equal(st.path, path[:len(st.path)]) {
				continue
			}
			node := st.raw
			for _, part := range path[st.sectionDepth:] {
				node, _ = node[part].(map[string]any)
			}
			if node == nil {
				continue
			}
			for _, key := range legacyProfileKeys {
				delete(node, key)
			}
			var selected map[string]any
			if _, err := toml.Decode(ref, &selected); err != nil {
				return "", err
			}
			maps.Copy(node, selected)
			inlineScopes[scope], changed = true, true
		}
		if changed {
			var rewritten strings.Builder
			for _, line := range strings.Split(strings.TrimSuffix(st.text, "\n"), "\n") {
				fmt.Fprintln(&rewritten, "# Previous: "+line)
			}
			for _, key := range slices.Sorted(maps.Keys(st.raw)) {
				value, err := inlineProfileValue(st.raw[key])
				if err != nil {
					return "", err
				}
				fmt.Fprintf(&rewritten, "%s = %s\n", toml.Key{key}.String(), value)
			}
			st.text = rewritten.String()
		}
	}
	// Explicit scope headers are the natural home for references. An implicit
	// scope (dotted keys or only a model_by_size subtable) gets dotted references
	// at the root, before the first table.
	headers := map[string]bool{}
	for _, st := range statements {
		if st.header {
			headers[strings.Join(st.path, ".")] = true
		}
	}
	var out strings.Builder
	for _, scope := range slices.Sorted(maps.Keys(refs)) {
		if !headers[scope] && !inlineScopes[scope] {
			for _, line := range strings.Split(strings.TrimSuffix(refs[scope], "\n"), "\n") {
				fmt.Fprintln(&out, scope+"."+line)
			}
		}
	}
	for _, st := range statements {
		_, key := legacySettingPath(st.path)
		if key != "" {
			if st.header || !strings.HasPrefix(strings.TrimSpace(st.text), "#") {
				fmt.Fprintln(&out, "# Agent settings moved to profiles (version 3).")
				for _, line := range strings.Split(strings.TrimSuffix(st.text, "\n"), "\n") {
					fmt.Fprintln(&out, "# Previous: "+line)
				}
			} else {
				fmt.Fprintln(&out, "# Configure "+key+" through profile / profile_by_size and [profiles.<name>].")
			}
			continue
		}
		out.WriteString(st.text)
		if st.header && refs[strings.Join(st.path, ".")] != "" {
			if !strings.HasSuffix(st.text, "\n") {
				out.WriteByte('\n')
			}
			out.WriteString(refs[strings.Join(st.path, ".")])
		}
	}
	out.WriteString(blocks.String())
	return out.String(), nil
}

// tomlQuote uses the TOML encoder rather than Go quoting (whose \x escapes
// are not TOML) for arbitrary user profile names and model strings.
func tomlQuote(s string) string {
	var b strings.Builder
	_ = toml.NewEncoder(&b).Encode(map[string]string{"v": s})
	return strings.TrimSpace(strings.TrimPrefix(b.String(), "v = "))
}

type profileStatement struct {
	raw          map[string]any
	sectionDepth int
	text         string
	path         []string
	header       bool
}

// profileStatements lets the TOML parser delimit complete statements, including
// multiline strings and arrays. Looking for setting-looking lines alone would
// corrupt prompts containing TOML examples. Decode already validated the file.
func profileStatements(text string) ([]profileStatement, error) {
	var out []profileStatement
	var section []string
	lines := strings.SplitAfter(text, "\n")
	for i := 0; i < len(lines); i++ {
		chunk := lines[i]
		trimmed := strings.TrimSpace(chunk)
		st := profileStatement{text: chunk}
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			// Only simple commented defaults in a role scope are rewritten.
			if strings.HasPrefix(trimmed, "#") {
				var raw map[string]any
				if md, err := toml.Decode(strings.TrimSpace(strings.TrimPrefix(trimmed, "#")), &raw); err == nil && len(md.Keys()) > 0 {
					st.path = append(slices.Clone(section), md.Keys()[0]...)
				}
			}
			out = append(out, st)
			continue
		}
		var raw map[string]any
		md, err := toml.Decode(chunk, &raw)
		for err != nil && i+1 < len(lines) {
			i++
			chunk += lines[i]
			md, err = toml.Decode(chunk, &raw)
		}
		if err != nil {
			return nil, err
		}
		st.text = chunk
		st.raw, st.sectionDepth = raw, len(section)
		if strings.HasPrefix(trimmed, "[") {
			st.header = true
			section = slices.Clone(md.Keys()[len(md.Keys())-1])
			st.path = slices.Clone(section)
		} else if len(md.Keys()) > 0 {
			st.path = append(slices.Clone(section), md.Keys()[0]...)
		}
		out = append(out, st)
	}
	return out, nil
}

func legacySettingPath(path []string) (scope, key string) {
	n := 0
	if len(path) >= 2 && path[0] == "global" {
		n = 1
	}
	if len(path) >= 3 && path[0] == "roles" {
		n = 2
	}
	if n > 0 && slices.Contains(legacyProfileKeys, path[n]) {
		return strings.Join(path[:n], "."), path[n]
	}
	return "", ""
}

// inlineProfileValue serializes values from a decoded inline scope without
// introducing table headers that would change the scope of following lines.
func inlineProfileValue(value any) (string, error) {
	if table, ok := value.(map[string]any); ok {
		var entries []string
		for _, key := range slices.Sorted(maps.Keys(table)) {
			v, err := inlineProfileValue(table[key])
			if err != nil {
				return "", err
			}
			entries = append(entries, toml.Key{key}.String()+" = "+v)
		}
		return "{ " + strings.Join(entries, ", ") + " }", nil
	}
	var b strings.Builder
	if err := toml.NewEncoder(&b).Encode(map[string]any{"v": value}); err != nil {
		return "", err
	}
	return strings.TrimSpace(strings.TrimPrefix(b.String(), "v = ")), nil
}
