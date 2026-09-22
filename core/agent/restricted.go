package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
)

const (
	// DefaultRestrictedTimeout bounds a restricted turn whose profile does
	// not choose one.
	DefaultRestrictedTimeout = 15 * time.Minute
	// DefaultRestrictedMaxTurns bounds a restricted turn whose profile does
	// not choose one.
	DefaultRestrictedMaxTurns = 40
)

var (
	// restrictedReadTools are the Claude built-ins available to a restricted
	// turn. Codex expresses the same floor through its feature configuration
	// and read-only sandbox.
	restrictedReadTools = []string{"Read", "Grep", "Glob", "LS", "NotebookRead"}
	// restrictedDeniedTools name Claude built-ins that can change state, run
	// commands, delegate work or fetch from the network.
	restrictedDeniedTools = []string{"Bash", "BashOutput", "Edit", "KillShell", "MultiEdit", "NotebookEdit", "Task", "WebFetch", "WebSearch", "Write"}
)

// RestrictedResult is a successful restricted turn. Result is the ordinary
// runner result; Model is the selected profile's model, including the model of
// a fallback that answered.
type RestrictedResult struct {
	*Result
	Model string
}

// RunRestricted runs a read-only agent turn through the same backend and
// process lifecycle as Run. It is intended for reviews and other analysis
// that needs no caller-owned tools: the turn receives no MCP server, writable
// mount, VCS access, skills, hooks or local project configuration. Claude and
// Codex implement the restriction today; other backends are refused before
// any process starts.
//
// The request may set Name, Profile's execution fields (Name, Agent, Model,
// Fallback, Effort, MaxTurns, Timeout and Env), Workspace, Prompt, Env and
// ResumeID. Other fields describe capabilities a restricted turn cannot have
// and are rejected rather than ignored.
func (r *Runner) RunRestricted(ctx context.Context, req Request) (*RestrictedResult, error) {
	if err := validateRestrictedRequest(req); err != nil {
		return nil, err
	}
	runner := *r
	runner.AddDirs = nil
	runner.MountDirs = nil
	runner.SkillMountDirs = nil
	runner.Skills = nil
	if runner.SessionsDir == "" {
		runner.SessionsDir = os.TempDir()
	}

	profile := req.Profile
	for {
		profile = restrictedDefaults(profile)
		attempt := restrictedRequest(&runner, req, profile)
		attemptCtx, cancel := context.WithTimeout(ctx, profile.Timeout)
		res, err := runner.run(attemptCtx, attempt, true)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("%s session: %w", req.Name, err)
		}
		failure := restrictedFailure(req.Name, res)
		if failure == nil {
			return &RestrictedResult{Result: res, Model: profile.Model}, nil
		}
		if profile.Fallback == nil || !RateLimitedText(failure.Error()) {
			return nil, failure
		}
		profile = *profile.Fallback
	}
}

func restrictedDefaults(p Profile) Profile {
	if p.MaxTurns <= 0 {
		p.MaxTurns = DefaultRestrictedMaxTurns
	}
	if p.Timeout <= 0 {
		p.Timeout = DefaultRestrictedTimeout
	}
	return p
}

func restrictedRequest(r *Runner, base Request, profile Profile) Request {
	profile.Env = restrictedEnvMap(r.EnvironmentPrefix, profile.Env)
	base.Profile = profile
	base.Env = restrictedEnvMap(r.EnvironmentPrefix, base.Env)
	base.Grants = &Grants{
		Env:    restrictedEnvNames(r.EnvironmentPrefix, profile.Env, base.Env),
		Tools:  slices.Clone(restrictedReadTools),
		Mounts: []Mount{{Path: string(os.PathSeparator), Access: ReadOnly}},
	}
	return base
}

func restrictedEnvMap(prefix string, in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		if restrictedEnvName(prefix, k) {
			out[k] = v
		}
	}
	return out
}

func restrictedEnvNames(prefix string, sets ...map[string]string) []string {
	names := map[string]bool{}
	for _, entry := range os.Environ() {
		name, _, ok := strings.Cut(entry, "=")
		if ok && restrictedEnvName(prefix, name) {
			names[name] = true
		}
	}
	for _, set := range sets {
		for name := range set {
			names[name] = true
		}
	}
	return slices.Sorted(maps.Keys(names))
}

func restrictedEnvName(prefix, name string) bool {
	return name != "" && !strings.HasPrefix(name, "BEES_") &&
		(prefix == "" || !strings.HasPrefix(name, prefix)) && !isVCSEnv(name)
}

func validateRestrictedRequest(req Request) error {
	switch {
	case req.Workspace == nil:
		return errors.New("restricted session: request has no workspace")
	case req.Grants != nil:
		return errors.New("restricted session: grants are fixed by RunRestricted")
	case req.HostMCP != nil:
		return errors.New("restricted session: MCP servers are not supported")
	case req.ValidOutcomes != nil:
		return errors.New("restricted session: outcomes are not supported")
	case len(req.ContainerEnv) > 0 || len(req.VCSEnv) > 0 || len(req.VCSContainerEnv) > 0:
		return errors.New("restricted session: container and VCS environments are not supported")
	case req.SystemPrompt != "":
		return errors.New("restricted session: put all instructions in Prompt; SystemPrompt is not supported")
	case req.SessionDir != "":
		return errors.New("restricted session: SessionDir is owned by RunRestricted")
	}
	seen := map[*Profile]bool{}
	for p := &req.Profile; p != nil; p = p.Fallback {
		if seen[p] {
			return errors.New("restricted session: fallback profiles form a cycle")
		}
		seen[p] = true
		provider := p.Agent
		if provider == "" {
			provider = AgentClaude
		}
		if provider != AgentClaude && provider != AgentCodex {
			return fmt.Errorf("restricted session: agent %q cannot establish the read-only restriction (supported: %s, %s)", provider, AgentClaude, AgentCodex)
		}
		if p.Sandbox != "" || p.Confine || p.SandboxImage != "" || p.ContainerUseEnvironment != "" || p.Dagger != nil || len(p.SandboxDomains) > 0 {
			return errors.New("restricted session: caller-selected sandboxes are not supported; RunRestricted establishes the boundary")
		}
		if p.VCSAccess || p.Shell != "" || len(p.MCP) > 0 || len(p.AllowedTools) > 0 || len(p.DisallowedTools) > 0 || len(p.Skills) > 0 || len(p.PiPackages) > 0 {
			return errors.New("restricted session: VCS, shell, MCP, tool, skill and package configuration cannot be supplied")
		}
	}
	return nil
}

func restrictedFailure(name string, res *Result) error {
	if !res.IsError && res.ExitCode == 0 {
		return nil
	}
	detail := strings.TrimSpace(res.ResultText)
	kind := res.ErrorSubtype
	if kind == "" {
		kind = "failed"
	}
	if detail == "" {
		return fmt.Errorf("%s session: %s", name, kind)
	}
	return fmt.Errorf("%s session: %s: %s", name, kind, detail)
}

// RateLimitedText recognizes capacity and transient service-limit messages.
// It lives with agent execution so restricted fallback chains and callers in
// core/ops make the same decision.
func RateLimitedText(msg string) bool {
	msg = strings.ToLower(msg)
	for _, phrase := range []string{"rate limit", "abuse detection", "secondary rate", "overloaded", "usage limit", "session limit"} {
		if strings.Contains(msg, phrase) {
			return true
		}
	}
	return false
}

func codexRestrictedConfigArgs() []string {
	args := []string{"-c", `approval_policy="never"`, "-c", `web_search="disabled"`,
		"-c", "orchestrator.mcp.enabled=false"}
	for _, feature := range []string{"shell_tool", "unified_exec", "js_repl", "browser_use", "browser_use_external", "computer_use", "in_app_browser", "multi_agent", "multi_agent_v2", "apps", "plugins", "hooks", "codex_hooks", "plugin_hooks", "skill_mcp_dependency_install", "tool_suggest", "web_search_request", "web_search_cached"} {
		args = append(args, "-c", "features."+feature+"=false")
	}
	return args
}

func codexMCPInventory(ctx context.Context, bin, dir string, env []string) ([]string, error) {
	args := append([]string{"mcp", "list", "--json"}, codexRestrictedConfigArgs()...)
	cmd := agentbin.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("list MCP servers: %w%s", err, stderrTail(stderr.String()))
	}
	var servers []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &servers); err != nil {
		return nil, fmt.Errorf("decode MCP servers: %w", err)
	}
	if servers == nil {
		return nil, errors.New("MCP inventory must be an array")
	}
	names := make([]string, 0, len(servers))
	for _, server := range servers {
		names = append(names, server.Name)
	}
	return names, nil
}

func stderrTail(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return ""
	}
	if len(s) > 400 {
		s = "..." + s[len(s)-400:]
	}
	return ": " + s
}
