package agent

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
)

// A writable codex turn whose grants name built-in tools rather than
// ToolsAll is held to them by its configuration: a fixed set of overrides
// (codexHeldSettings) derived from the grant, plus the sandbox/approval
// table every codex turn's thread/start carries (codexThreadStartParams,
// codex_rpc.go). Codex's controls are not one per tool, so a grant names
// codex's tools by the control that holds them (codexTools), and a grant
// no combination of controls expresses exactly is refused instead of
// widened.
//
//   - shell is command execution. apply_patch is codex's own file editing;
//     no setting removes it, so without it the turn runs in codex's
//     read-only sandbox, where every patch and shell command is rejected.
//     That sandbox would hold the shell too, so shell without apply_patch
//     is refused: codex's shell writes files.
//   - update_plan is tools.update_plan.enabled, web_search the web_search
//     mode.
//
// Delegation is off (agents.enabled, orchestrator.mcp.enabled) and
// approvals are never asked for, whatever the turn is granted. codex's app
// server, asked `config/read` for the turn's own working directory once
// the process it will run the turn on is up (codexRPCRun, codex_rpc.go),
// is the one check that the turn's command line actually won every one of
// these settings: `validateCodexSettings` refuses the turn, before any
// thread starts, when the effective configuration shows one of them set
// by a layer above the command line (a managed configuration) or holding
// another value.

// codexTools are the built-in tool names a grant may give a writable codex
// turn.
var codexTools = []string{"apply_patch", "image_generation", "shell", "update_plan", "view_image", "web_search"}

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

// codexHeldArgs are the command-line overrides that hold a writable codex
// turn to its granted tools: codexHeldSettings, rendered as `-c` overrides.
// Codex's app server checks, on the same process the turn runs on, that
// every one of them took effect (codexRPCRun, codex_rpc.go).
func codexHeldArgs(tools []string) []string {
	var args []string
	for _, s := range codexHeldSettings(tools) {
		args = append(args, "-c", s.key+"="+s.value)
	}
	return args
}

// A codexSetting is one configuration override: a dotted key and its
// value as TOML, which for every held setting is also JSON.
type codexSetting struct{ key, value string }

// codexHeldSettings are the overrides every held turn gets whatever its
// effective configuration: no approvals to wait for, no delegation, and
// the non-feature tools the grant leaves out switched off.
func codexHeldSettings(tools []string) []codexSetting {
	settings := []codexSetting{
		{"approval_policy", `"never"`},
		{"orchestrator.mcp.enabled", "false"},
		{"agents.enabled", "false"},
		{"tools.experimental_request_user_input.enabled", "false"},
	}
	if !slices.Contains(tools, "update_plan") {
		settings = append(settings, codexSetting{"tools.update_plan.enabled", "false"})
	}
	if !slices.Contains(tools, "web_search") {
		settings = append(settings, codexSetting{"web_search", `"disabled"`})
	}
	return settings
}

// codexEffectiveConfig is codex's answer to config/read: the effective
// configuration, and for every key set anywhere the layer it comes from.
type codexEffectiveConfig struct {
	Config  map[string]any `json:"config"`
	Origins map[string]struct {
		Name struct {
			Type string `json:"type"`
		} `json:"name"`
	} `json:"origins"`
}

// codexSessionFlags is the layer config/read names a -c override's.
const codexSessionFlags = "sessionFlags"

// validateCodexSettings refuses an effective configuration in which a
// held setting is not the turn's own: set by another layer than the
// command line, or, where the configuration shows the key, holding
// another value.
func validateCodexSettings(cfg codexEffectiveConfig, settings []codexSetting) error {
	for _, s := range settings {
		origin, ok := cfg.Origins[s.key]
		if !ok {
			return fmt.Errorf("effective setting %q has no origin", s.key)
		}
		if origin.Name.Type != codexSessionFlags {
			return fmt.Errorf("effective setting %q comes from the %q layer, not the turn's command line", s.key, origin.Name.Type)
		}
		got, shown := lookupKey(cfg.Config, s.key)
		if !shown || got == nil {
			continue
		}
		var want any
		if err := json.Unmarshal([]byte(s.value), &want); err != nil {
			return fmt.Errorf("setting %q: %w", s.key, err)
		}
		if !reflect.DeepEqual(got, want) {
			return fmt.Errorf("effective setting %q is %v, want %s", s.key, got, s.value)
		}
	}
	return nil
}

// lookupKey finds a dotted key in a decoded JSON object.
func lookupKey(m map[string]any, key string) (any, bool) {
	var cur any = m
	for _, part := range strings.Split(key, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		if cur, ok = obj[part]; !ok {
			return nil, false
		}
	}
	return cur, true
}
