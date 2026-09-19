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
	Agent string `toml:"agent" json:"agent"`
	Model string `toml:"model" json:"model"`
	// Fallback names the profile a retry runs on instead after a session
	// on this one failed for infrastructure reasons (or a brief or angle
	// review session was refused for want of capacity): its agent, model,
	// effort and sandbox, and after it that profile's own fallback. Empty
	// is no fallback. Validate refuses a
	// name that is not a profile and a chain that comes back to a profile
	// it has been through, so the chain always ends.
	Fallback string `toml:"fallback" json:"fallback"`
	Effort   string `toml:"effort" json:"effort"`
	Sandbox  string `toml:"sandbox" json:"sandbox"`
}

func (p AgentProfile) resolved() AgentProfile {
	p.Agent = firstNonEmpty(p.Agent, DefaultAgent)
	p.Sandbox = firstNonEmpty(p.Sandbox, DefaultSandbox)
	if p.Agent == AgentClaude {
		p.Model = firstNonEmpty(p.Model, DefaultModel)
	}
	return p
}

// fallbackChain is the profiles a session on profile from, whose fallback
// names first, falls back to, in order: first, then the one its fallback
// names, until a profile with none. A name that is not in the table ends the
// chain, and so does a name already in it, so that a table Validate refused
// still ends.
func fallbackChain(profiles map[string]AgentProfile, from, first string) []string {
	var chain []string
	seen := map[string]bool{from: true}
	for next := first; next != ""; next = profiles[next].Fallback {
		if _, ok := profiles[next]; !ok || seen[next] {
			break
		}
		seen[next] = true
		chain = append(chain, next)
	}
	return chain
}

// legacyAgentProfile is a profile as versions 3 and 4 wrote it, with a
// fallback model instead of a fallback profile; version 5 turns the model
// into a profile of its own (migrateFallbackProfiles). The migrations before
// it write this shape so that a chain of migrations ends with the same
// profiles a file already at version 4 has.
type legacyAgentProfile struct {
	Agent         string `toml:"agent"`
	Model         string `toml:"model"`
	FallbackModel string `toml:"fallback_model"`
	Effort        string `toml:"effort"`
	Sandbox       string `toml:"sandbox"`
}

// legacyDefaultFallbackModel was the fallback model of a claude profile that
// named none, in versions 3 and 4.
const legacyDefaultFallbackModel = "sonnet"

func (p legacyAgentProfile) resolved() legacyAgentProfile {
	p.Agent = firstNonEmpty(p.Agent, DefaultAgent)
	p.Sandbox = firstNonEmpty(p.Sandbox, DefaultSandbox)
	model, fallback := "", ""
	if p.Agent == AgentClaude {
		model, fallback = DefaultModel, legacyDefaultFallbackModel
	}
	p.Model = firstNonEmpty(p.Model, model)
	p.FallbackModel = firstNonEmpty(p.FallbackModel, fallback)
	return p
}

// legacyProfileScope exists only to read version 2's independently inherited
// settings. Resolve them before creating profiles so an agent override does
// not accidentally inherit Claude's implicit model defaults.
type legacyProfileScope struct {
	legacyAgentProfile
	ModelBySize map[string]string `toml:"model_by_size"`
}

func legacyProfile(role, global legacyProfileScope) legacyAgentProfile {
	return (legacyAgentProfile{
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
		Profiles map[string]legacyAgentProfile `toml:"profiles"`
	}
	md, err := toml.Decode(text, &old)
	if err != nil {
		return "", err
	}
	profiles := maps.Clone(old.Profiles)
	if profiles == nil {
		profiles = map[string]legacyAgentProfile{}
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
		add := func(base string, p legacyAgentProfile) string {
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
			// Decode wraps dotted assignments in parent maps. Serialize only
			// the assigned value, keeping its path relative to this section,
			// so sibling assignments do not redefine their shared parent.
			key := st.path[st.sectionDepth:]
			var assigned any = st.raw
			for _, part := range key {
				assigned = assigned.(map[string]any)[part]
			}
			value, err := inlineProfileValue(assigned)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&rewritten, "%s = %s\n", toml.Key(key).String(), value)
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

// migrateFallbackProfiles is version 4 -> 5: fallback_model, a model of the
// profile's own agent, becomes fallback, the name of another profile. Every
// profile with fallback_model = "X" gets fallback = "<name>_fallback" and a
// new [profiles.<name>_fallback] that is the profile with model X and no
// fallback of its own, so the migrated file behaves as it did. Complete TOML
// statements are rewritten, so dotted keys, inline tables and prompts
// containing examples survive; a commented-out fallback_model is told what
// replaced it and kept.
func migrateFallbackProfiles(text string) (string, error) {
	var old struct {
		Profiles map[string]legacyAgentProfile `toml:"profiles"`
	}
	md, err := toml.Decode(text, &old)
	if err != nil {
		return "", err
	}
	// The name each migrated profile's fallback profile gets, and the
	// profile itself. A profile whose fallback_model resolves to nothing
	// (codex or opencode with an empty value) had no fallback, and loses
	// the key.
	names := map[string]string{}
	added := map[string]map[string]any{}
	taken := maps.Clone(old.Profiles)
	for _, name := range slices.Sorted(maps.Keys(old.Profiles)) {
		p := old.Profiles[name]
		if !md.IsDefined("profiles", name, "fallback_model") {
			continue
		}
		if md.IsDefined("profiles", name, "fallback") {
			return "", fmt.Errorf("profiles.%s: cannot mix fallback_model and fallback; remove fallback_model", name)
		}
		model := p.resolved().FallbackModel
		if model == "" {
			continue
		}
		fname := name + "_fallback"
		for n := 2; ; n++ {
			if _, exists := taken[fname]; !exists {
				break
			}
			fname = fmt.Sprintf("%s_fallback_%d", name, n)
		}
		taken[fname] = legacyAgentProfile{}
		names[name] = fname
		added[fname] = map[string]any{}
		for _, kv := range []struct{ key, value string }{{"agent", p.Agent}, {"model", model}, {"effort", p.Effort}, {"sandbox", p.Sandbox}} {
			if kv.value != "" {
				added[fname][kv.key] = kv.value
			}
		}
	}
	statements, err := profileStatements(text)
	if err != nil {
		return "", err
	}
	isFallbackModel := func(path []string) bool {
		return len(path) == 3 && path[0] == "profiles" && path[2] == "fallback_model"
	}
	rename := func(path []string) []string {
		if isFallbackModel(path) {
			path = slices.Clone(path)
			path[2] = "fallback"
		}
		return path
	}
	// The new profiles go after the last statement as tables of their own,
	// except into a profiles = { ... } inline table, which no later table
	// header may extend.
	inlined := map[string]bool{}
	// rewrite returns the value with every fallback_model under it renamed
	// and pointed at the new profile, whether it changed, and whether the
	// value itself is a fallback_model to drop.
	var rewrite func(path []string, value any) (any, bool, bool, error)
	rewrite = func(path []string, value any) (any, bool, bool, error) {
		if table, ok := value.(map[string]any); ok {
			changed := false
			out := map[string]any{}
			for _, key := range slices.Sorted(maps.Keys(table)) {
				child := append(slices.Clone(path), key)
				v, did, drop, err := rewrite(child, table[key])
				if err != nil {
					return nil, false, false, err
				}
				changed = changed || did || drop
				if drop {
					continue
				}
				out[rename(child)[len(child)-1]] = v
			}
			if len(path) == 1 && path[0] == "profiles" {
				for _, fname := range slices.Sorted(maps.Keys(added)) {
					out[fname], inlined[fname], changed = added[fname], true, true
				}
			}
			return out, changed, false, nil
		}
		if !isFallbackModel(path) {
			return value, false, false, nil
		}
		if _, ok := value.(string); !ok {
			return nil, false, false, fmt.Errorf("%s must name a model", strings.Join(path, "."))
		}
		fname, ok := names[path[1]]
		if !ok {
			return nil, true, true, nil
		}
		return fname, true, false, nil
	}
	const guidance = "# fallback_model was replaced by fallback, which names another profile (version 5)."
	var out strings.Builder
	last := ""
	for _, st := range statements {
		if st.raw == nil {
			// A commented-out fallback_model, wherever it is: at version 4
			// the key was a profile's alone.
			if len(st.path) > 0 && st.path[len(st.path)-1] == "fallback_model" && last != guidance {
				fmt.Fprintln(&out, guidance)
			}
			out.WriteString(st.text)
			last = strings.TrimSuffix(st.text, "\n")
			continue
		}
		last = ""
		if st.header {
			out.WriteString(st.text)
			continue
		}
		key := st.path[st.sectionDepth:]
		var assigned any = st.raw
		for _, part := range key {
			assigned = assigned.(map[string]any)[part]
		}
		value, changed, dropped, err := rewrite(st.path, assigned)
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
		if dropped {
			continue
		}
		encoded, err := inlineProfileValue(value)
		if err != nil {
			return "", err
		}
		fmt.Fprintf(&out, "%s = %s\n", toml.Key(rename(st.path)[st.sectionDepth:]).String(), encoded)
	}
	for _, fname := range slices.Sorted(maps.Keys(added)) {
		if inlined[fname] {
			continue
		}
		fmt.Fprintf(&out, "\n[%s]\n", toml.Key{"profiles", fname}.String())
		for _, key := range []string{"agent", "model", "effort", "sandbox"} {
			if v, ok := added[fname][key]; ok {
				fmt.Fprintf(&out, "%s = %s\n", key, tomlQuote(v.(string)))
			}
		}
	}
	return out.String(), nil
}

// Resolved is the profile with the built-in defaults filled in for what it
// leaves out: the default agent and sandbox, and claude's default model.
func (p AgentProfile) Resolved() AgentProfile { return p.resolved() }

// ValidateProfiles checks a table of profiles on its own, each problem
// naming its key as profiles.<name>.<key>: an empty name, an effort, agent
// or sandbox outside its values, and a fallback chain that names a profile
// the table does not have or comes back to one it has been through. It is
// how bees.toml's [profiles] is checked, and how any other file that
// describes sessions with profiles (`bees review`'s config.toml) checks its
// own.
func ValidateProfiles(profiles map[string]AgentProfile) []string {
	var errs []string
	for _, name := range slices.Sorted(maps.Keys(profiles)) {
		p, scope := profiles[name], "profiles."+name
		if name == "" {
			errs = append(errs, "profiles: profile name must not be empty")
		}
		if p.resolved().Agent != AgentOpenCode {
			switch p.Effort {
			case "", "low", "medium", "high", "max":
			default:
				errs = append(errs, fmt.Sprintf("%s.effort must be low, medium, high or max", scope))
			}
		}
		if p.Agent != "" && !slices.Contains(Agents, p.Agent) {
			errs = append(errs, fmt.Sprintf("%s.agent must be one of %s", scope, strings.Join(Agents, ", ")))
		}
		// Every mode of SandboxModes loads, including the ones no session
		// can run in yet: whether a mode works on this machine is a question
		// about the machine, and CheckSandbox asks it once at `bees run`.
		if p.Sandbox != "" && !slices.Contains(SandboxModes, p.Sandbox) {
			errs = append(errs, fmt.Sprintf("%s.sandbox must be one of %s", scope, strings.Join(SandboxModes, ", ")))
		}
		// The sbx sandbox is created for claude, and bees builds no other
		// agent's command for it: refused at load, naming the profile, rather
		// than at the first session.
		if p.Sandbox == SandboxSbx && p.resolved().Agent != AgentClaude {
			errs = append(errs, fmt.Sprintf("%s.sandbox = %q runs agent %q only; %s.agent is %q", scope, SandboxSbx, AgentClaude, scope, p.resolved().Agent))
		}
		if p.Fallback != "" {
			if err := validateFallback(profiles, name); err != nil {
				errs = append(errs, fmt.Sprintf("%s.fallback: %v", scope, err))
			}
		}
	}
	return errs
}

// validateFallback checks the fallback chain that starts at profile name:
// every link names a profile, and none comes back to one the chain has been
// through, itself included. The error says which link is wrong and what to
// change.
func validateFallback(profiles map[string]AgentProfile, name string) error {
	seen := []string{name}
	for at, next := name, profiles[name].Fallback; next != ""; at, next = next, profiles[next].Fallback {
		if _, ok := profiles[next]; !ok {
			if at == name {
				return fmt.Errorf("unknown profile %q (declare it under [profiles.%s])", next, next)
			}
			return fmt.Errorf("profiles.%s.fallback: unknown profile %q (declare it under [profiles.%s])", at, next, next)
		}
		if next == at {
			if at == name {
				return fmt.Errorf("a profile cannot be its own fallback (name another profile, or remove the key)")
			}
			return fmt.Errorf("profiles.%s.fallback: a profile cannot be its own fallback (name another profile, or remove the key)", at)
		}
		if slices.Contains(seen, next) {
			return fmt.Errorf("fallback chain %s comes back to %q (end the chain at a profile without a fallback)", strings.Join(append(seen, next), " -> "), next)
		}
		seen = append(seen, next)
	}
	return nil
}

// ProfileChain is the profile called name followed by every profile its
// fallback chain runs through, in order, each one resolved; nil when
// profiles has no profile called name. A chain that names a profile the
// table does not have, or comes back round, ends where ValidateProfiles
// would have refused it.
func ProfileChain(profiles map[string]AgentProfile, name string) []AgentProfile {
	p, ok := profiles[name]
	if !ok {
		return nil
	}
	chain := []AgentProfile{p.resolved()}
	for _, next := range fallbackChain(profiles, name, p.Fallback) {
		chain = append(chain, profiles[next].resolved())
	}
	return chain
}
