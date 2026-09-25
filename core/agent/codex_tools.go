package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// A writable codex turn whose grants name built-in tools rather than
// ToolsAll is held to them by its configuration, derived from the grant
// and verified where the turn runs before the model starts. Codex's
// controls are not one per tool, so a grant names codex's tools by the
// control that holds them (codexTools), and a grant no combination of
// controls expresses exactly is refused instead of widened.
//
//   - shell is command execution: the shell_tool feature, which gates
//     every form of codex's shell, unified_exec's included.
//   - apply_patch is codex's own file editing. No setting removes it, so
//     without it the turn runs in codex's read-only sandbox with approvals
//     off, where every patch is rejected. That sandbox would hold the shell
//     too, so shell without apply_patch is refused: codex's shell writes
//     files.
//   - update_plan is tools.update_plan.enabled, web_search the web_search
//     mode and the web search features, view_image and image_generation
//     their features.
//
// Every other feature the effective configuration reports enabled is
// switched off, except the ones that give the model no tool
// (codexNeutralFeatures) and the ones codex reports removed, which it
// ignores. Delegation is off (agents.enabled, orchestrator.mcp.enabled),
// and so is every MCP server but the session's own. The inventories run
// twice: once to learn what to switch off, and once more with the whole
// configuration, which refuses the turn when a managed or inherited
// configuration keeps a feature on, an inherited MCP server enabled, or
// changes one of the session's own servers.

// codexTools are the built-in tool names a grant may give a writable codex
// turn.
var codexTools = []string{"apply_patch", "image_generation", "shell", "update_plan", "view_image", "web_search"}

// codexToolFeatures are the features each granted tool keeps: a feature
// named here stays as the effective configuration has it while its tool is
// granted, and is switched off otherwise.
var codexToolFeatures = map[string][]string{
	"apply_patch":      {"apply_patch_preserve_line_endings", "apply_patch_streaming_events"},
	"image_generation": {"image_generation"},
	"shell":            {"shell_tool"},
	"view_image":       {"view_image"},
	"web_search":       {"web_search_request", "web_search_cached", "standalone_web_search"},
}

// codexNeutralFeatures are the features a held turn leaves as they are:
// none of them gives the model a tool. code_mode_host is the host codex
// runs MCP tool calls through. The shell's own features choose the form
// the shell tool takes, and shell_tool decides whether there is one:
// codex keeps unified_exec on whatever its configuration says.
var codexNeutralFeatures = []string{
	"auth_elicitation", "code_mode_host", "compaction_image_budget", "content_item_kinds",
	"enable_request_compression", "fast_mode", "guardian_approval", "mentions_v2", "personality",
	"tool_call_mcp_elicitation", "unbounded_connection_retries",
	"powershell_shell_version", "shell_snapshot", "shell_snapshot_v2", "shell_zsh_fork", "unified_exec", "unified_exec_tty",
}

// checkTools holds a codex grant to codex's own vocabulary and to what its
// controls can express.
func (codexBackend) checkTools(where Placement, tools, _ []string) error {
	for _, t := range tools {
		if !slices.Contains(codexTools, t) {
			return fmt.Errorf("%w: agent %q has no built-in tool %q; grant one of %s", ErrUnsupported, AgentCodex, t, strings.Join(codexTools, ", "))
		}
	}
	if slices.Contains(tools, "shell") && !slices.Contains(tools, "apply_patch") {
		return fmt.Errorf("%w: agent %q cannot hold a writable turn to its granted built-in tools in sandbox %s: it withholds %q only by making the turn read-only, which would hold %q too, and its shell writes files; grant %q as well, or leave out %q",
			ErrUnsupported, AgentCodex, where, "apply_patch", "shell", "apply_patch", "shell")
	}
	return nil
}

// codexHeldArgs are the configuration overrides that hold a writable
// codex turn to its granted tools: the fixed ones, the features switched
// off, and every inherited MCP server disabled. own are the session's own
// MCP servers, whose overrides the command line carries after these; the
// probes see them too, as the turn will.
func codexHeldArgs(ctx context.Context, probe prober, bin string, tools []string, own map[string]MCPEntry) ([]string, error) {
	var ownArgs []string
	for _, o := range codexMCPOverrides(own) {
		ownArgs = append(ownArgs, "-c", o)
	}
	args := codexHeldFixedArgs(tools)
	with := func(held []string) []string { return append(slices.Clone(held), ownArgs...) }

	features, err := codexFeatureInventory(ctx, probe, bin, with(args))
	if err != nil {
		return nil, err
	}
	for _, f := range features {
		if f.enabled && !codexFeatureHeld(f.name, tools) {
			args = append(args, "-c", "features."+f.name+"=false")
		}
	}
	servers, err := codexServerInventory(ctx, probe, bin, with(args))
	if err != nil {
		return nil, err
	}
	var disabled []string
	for _, s := range servers {
		if _, ok := own[s.Name]; !ok {
			disabled = append(disabled, codexValue(s.Name)+"={enabled=false}")
		}
	}
	if len(disabled) > 0 {
		args = append(args, "-c", "mcp_servers={"+strings.Join(disabled, ",")+"}")
	}

	// The same inventories again, with everything the turn is given.
	if features, err = codexFeatureInventory(ctx, probe, bin, with(args)); err != nil {
		return nil, err
	}
	for _, f := range features {
		if f.enabled && !codexFeatureHeld(f.name, tools) {
			return nil, fmt.Errorf("effective feature %q is still enabled", f.name)
		}
	}
	if servers, err = codexServerInventory(ctx, probe, bin, with(args)); err != nil {
		return nil, err
	}
	if err := validateCodexServers(servers, own); err != nil {
		return nil, err
	}
	return args, nil
}

// codexHeldFixedArgs are the overrides every held turn gets whatever its
// effective configuration: no approvals to wait for, no delegation, and
// the non-feature tools the grant leaves out switched off.
func codexHeldFixedArgs(tools []string) []string {
	args := []string{"-c", `approval_policy="never"`, "-c", "orchestrator.mcp.enabled=false", "-c", "agents.enabled=false",
		"-c", "tools.experimental_request_user_input.enabled=false"}
	if !slices.Contains(tools, "update_plan") {
		args = append(args, "-c", "tools.update_plan.enabled=false")
	}
	if !slices.Contains(tools, "web_search") {
		args = append(args, "-c", `web_search="disabled"`)
	}
	return args
}

// codexFeatureHeld says whether a held turn with these tools may keep a
// feature enabled.
func codexFeatureHeld(name string, tools []string) bool {
	if slices.Contains(codexNeutralFeatures, name) {
		return true
	}
	for _, t := range tools {
		if slices.Contains(codexToolFeatures[t], name) {
			return true
		}
	}
	return false
}

// codexFeature is one line of `codex features list`.
type codexFeature struct {
	name    string
	enabled bool
}

// codexFeatureInventory runs `codex features list` where the turn will run
// and returns every feature it reports that is not removed: codex ignores
// a removed feature's setting.
func codexFeatureInventory(ctx context.Context, probe prober, bin string, args []string) ([]codexFeature, error) {
	out, err := probe(ctx, bin, append([]string{"features", "list"}, args...), nil)
	if err != nil {
		return nil, fmt.Errorf("list features: %w", err)
	}
	var features []codexFeature
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		state := fields[len(fields)-1]
		if len(fields) < 3 || (state != "true" && state != "false") {
			return nil, fmt.Errorf("list features: unreadable line %q", line)
		}
		if strings.Join(fields[1:len(fields)-1], " ") == "removed" {
			continue
		}
		features = append(features, codexFeature{name: fields[0], enabled: state == "true"})
	}
	if len(features) == 0 {
		return nil, errors.New("list features: no feature listed")
	}
	return features, nil
}

// codexServer is one entry of `codex mcp list --json`, reduced to what a
// held turn checks: its name, whether it is enabled, and how it is reached.
type codexServer struct {
	Name      string         `json:"name"`
	Enabled   *bool          `json:"enabled"`
	Transport map[string]any `json:"transport"`
}

// codexServerInventory runs `codex mcp list --json` where the turn will
// run.
func codexServerInventory(ctx context.Context, probe prober, bin string, args []string) ([]codexServer, error) {
	out, err := probe(ctx, bin, append([]string{"mcp", "list", "--json"}, args...), nil)
	if err != nil {
		return nil, fmt.Errorf("list MCP servers: %w", err)
	}
	var servers []codexServer
	if err := json.Unmarshal(out, &servers); err != nil {
		return nil, fmt.Errorf("decode MCP servers: %w", err)
	}
	if servers == nil {
		return nil, errors.New("MCP inventory must be an array")
	}
	return servers, nil
}

// validateCodexServers refuses an effective MCP configuration with an
// inherited server not disabled, or with one of the session's own servers
// missing, disabled or reached any other way than the runner wrote it.
func validateCodexServers(servers []codexServer, own map[string]MCPEntry) error {
	seen := map[string]bool{}
	for _, s := range servers {
		entry, ok := own[s.Name]
		if !ok {
			if s.Enabled == nil || *s.Enabled {
				return fmt.Errorf("effective MCP server %q is not disabled", s.Name)
			}
			continue
		}
		seen[s.Name] = true
		if s.Enabled == nil || !*s.Enabled {
			return fmt.Errorf("effective MCP server %q, the session's own, is not enabled", s.Name)
		}
		got, err := codexTransportKeys(s.Transport)
		if err != nil {
			return fmt.Errorf("effective MCP server %q: %w", s.Name, err)
		}
		want, err := codexTransportKeys(codexTransport(entry))
		if err != nil {
			return fmt.Errorf("MCP server %q: %w", s.Name, err)
		}
		if len(got) == 0 || !reflect.DeepEqual(got, want) {
			return fmt.Errorf("effective MCP server %q is not the session's own", s.Name)
		}
	}
	for _, name := range sortedKeys(own) {
		if !seen[name] {
			return fmt.Errorf("effective configuration removed the session's MCP server %q", name)
		}
	}
	return nil
}

// codexTransport is how `codex mcp list --json` reports a server the
// runner wrote from entry (codexMCPOverrides).
func codexTransport(e MCPEntry) map[string]any {
	if e.Command != "" {
		return map[string]any{"type": "stdio", "command": e.Command, "args": e.Args, "env": e.Env, "env_vars": e.EnvVars}
	}
	return map[string]any{"type": "streamable_http", "url": e.URL, "bearer_token_env_var": e.BearerTokenEnv, "http_headers": e.Headers}
}

// codexTransportKeys is a transport as generic JSON with every empty value
// left out, so that a key codex reports as null, empty or absent compares
// alike and every other key compares whole.
func codexTransportKeys(t map[string]any) (map[string]any, error) {
	data, err := json.Marshal(t)
	if err != nil {
		return nil, err
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for k, v := range generic {
		switch v := v.(type) {
		case nil:
			continue
		case string:
			if v == "" {
				continue
			}
		case []any:
			if len(v) == 0 {
				continue
			}
		case map[string]any:
			if len(v) == 0 {
				continue
			}
		}
		out[k] = v
	}
	return out, nil
}
