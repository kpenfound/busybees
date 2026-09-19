package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
	"github.com/kpenfound/busybees/core/ops"
	core "github.com/kpenfound/busybees/core/review"
	"github.com/kpenfound/busybees/internal/config"
)

// A review session is given no tools that write files, run commands or reach
// the network. The adapter may run read-only CLI probes, such as Codex's
// `mcp list`, before launching it. CI owns tests and builds; the distiller and
// every angle session share the same tool restrictions, set here once.
//
// Each CLI is held to it the way that CLI can be: claude by the tools it is
// given and the tools it is refused, codex by its own read-only sandbox.
// Neither is asked to take the restriction from the prompt.
var (
	// ReadOnlyTools are the tools a review session may use.
	ReadOnlyTools = []string{"Read", "Grep", "Glob", "LS", "NotebookRead"}
	// DeniedTools are the tools it may not: the ones that run something,
	// the ones that change something, and the ones that would send the
	// diff somewhere.
	DeniedTools = []string{"Bash", "BashOutput", "Edit", "KillShell", "MultiEdit", "NotebookEdit", "Task", "WebFetch", "WebSearch", "Write"}
)

// Defaults for a review session, which has one thing to do and no reason to
// take long over it.
const (
	// DefaultSessionTimeout is how long a session may run.
	DefaultSessionTimeout = 15 * time.Minute
	// DefaultMaxTurns is how many turns it may take.
	DefaultMaxTurns = 40
)

type AgentRequest = core.AgentRequest
type AgentResult = core.AgentResult
type Agent = core.Agent

// CLIAgent runs a review session as the coding agent the person configured,
// `claude -p` or `codex exec`. It is the whole of what `bees review` needs
// from an agent, and deliberately not the factory's session runner
// (internal/session): that one gives every session the built-in bees MCP
// server and the BEES_* environment of the factory it belongs to, and
// `bees review` reviews any pull request on GitHub without one.
type CLIAgent struct {
	// Provider is the CLI to run, one of SupportedProviders, and Model the
	// model it runs. An empty model leaves the choice to the CLI.
	Provider string
	Model    string
	// Fallback is the agent the session runs as instead when this one has
	// no capacity: the CLI answered, or died saying on stderr, that the
	// model is rate limited or overloaded (ops.RateLimitedText). An agent
	// of its own, so it is held
	// to the same read-only floor whatever CLI it runs, and it may have a
	// fallback of its own. When both run claude its model is also passed as
	// --fallback-model, so claude switches to it within the session. Nil
	// is no fallback.
	Fallback *CLIAgent
	// Effort maps to each backend's reasoning setting.
	Effort string
	// ClaudeBin and CodexBin are the executables, "claude" and "codex" when
	// they are empty.
	ClaudeBin string
	CodexBin  string
	// Timeout and MaxTurns bound the session, DefaultSessionTimeout and
	// DefaultMaxTurns when they are zero.
	Timeout  time.Duration
	MaxTurns int
	// Env are variables set for the session on top of the environment the
	// caller runs in, a role's own from bees.toml when the factory runs the
	// review. Nothing for `bees review`, whose sessions run as the person.
	Env map[string]string
}

// NewAgent is the agent a review's sessions run as, as the global
// configuration says.
func NewAgent(cfg *Config) *CLIAgent {
	agent := &CLIAgent{Provider: DefaultProvider, Model: DefaultModel}
	if cfg != nil {
		agent.Provider, agent.Model = cfg.Provider, cfg.Model
	}
	return agent
}

// Run runs the session and returns what it said last. An error is a session
// that could not be started, one that failed, and one that ended without
// saying anything: none of those is a review, and the caller has nothing to
// go on either way.
func (a *CLIAgent) Run(ctx context.Context, req AgentRequest) (*AgentResult, error) {
	bin, args, err := a.command(req)
	if err != nil {
		return nil, err
	}
	// The session runs on this host: a test binary is kept from running a
	// real agent (see core/agent/agentbin).
	if bin, err = agentbin.Resolve(bin); err != nil {
		return nil, fmt.Errorf("%s session: %w", req.Name, err)
	}
	timeout := a.Timeout
	if timeout <= 0 {
		timeout = DefaultSessionTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = req.Dir
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = reviewEnvironment(os.Environ(), a.Env)
	if a.Provider == config.AgentCodex {
		// Empty TOML tables merge with local configuration; they do not erase
		// inherited MCP servers. Inventory with the session's restrictions so
		// disabled plugins and apps cannot contribute transportless overrides.
		probeArgs := append([]string{"mcp", "list", "--json"}, codexReviewConfigArgs()...)
		probe := exec.CommandContext(runCtx, bin, probeArgs...)
		probe.Dir, probe.Env = cmd.Dir, cmd.Env
		data, err := probe.Output()
		if err != nil {
			return nil, fmt.Errorf("%s session: list codex MCP servers: %w", req.Name, err)
		}
		var servers []struct {
			Name string `json:"name"`
		}
		if err := json.Unmarshal(data, &servers); err != nil {
			return nil, fmt.Errorf("%s session: decode codex MCP servers: %w", req.Name, err)
		}
		if servers == nil {
			return nil, fmt.Errorf("%s session: codex MCP inventory must be an array", req.Name)
		}
		var disabled []string
		for _, server := range servers {
			name, _ := json.Marshal(server.Name)
			disabled = append(disabled, string(name)+"={enabled=false}")
		}
		if len(disabled) > 0 {
			// Codex splits override paths on dots literally, including quotes.
			// Names belong in the TOML value, where quoted keys are parsed and
			// the disabled flags merge with each server's existing transport.
			cmd.Args = append(cmd.Args[:len(cmd.Args)-1], "-c", "mcp_servers={"+strings.Join(disabled, ",")+"}", "-")
		}
	}
	// A session that ran out of time is killed with everything it started,
	// the way the factory's runner does it: the CLI's own children hold the
	// pipes open, so killing the CLI alone would leave the review waiting
	// for them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	runErr := cmd.Run()

	res, parseErr := a.read(stdout.Bytes())
	var failure error
	switch {
	case res != nil && res.err != "":
		failure = fmt.Errorf("%s session: %s", req.Name, res.err)
	case runErr != nil:
		failure = fmt.Errorf("%s session: %w%s", req.Name, runErr, tail(stderr.String()))
	case parseErr != nil:
		failure = fmt.Errorf("%s session: %w%s", req.Name, parseErr, tail(stderr.String()))
	}
	if failure == nil {
		return &AgentResult{ID: res.id, Text: res.text, Turns: res.turns, CostUSD: res.cost, CostKnown: res.costKnown, Provider: a.provider(), Model: a.Model}, nil
	}
	// A session refused for want of capacity is run again as the fallback
	// agent, which answers for itself, its own fallback included. A CLI that
	// died saying so on stderr alone is caught too: the error carries the
	// tail of what it said.
	if a.Fallback != nil && ops.RateLimitedText(failure.Error()) {
		return a.Fallback.Run(ctx, req)
	}
	return nil, failure
}

// provider is the CLI this agent runs, claude when none was named.
func (a *CLIAgent) provider() string {
	if a.Provider == "" {
		return config.AgentClaude
	}
	return a.Provider
}

// command is the CLI and the arguments this session runs as, read-only
// restriction included.
func (a *CLIAgent) command(req AgentRequest) (string, []string, error) {
	maxTurns := a.MaxTurns
	if maxTurns <= 0 {
		maxTurns = DefaultMaxTurns
	}
	switch a.Provider {
	case "", config.AgentClaude:
		bin := a.ClaudeBin
		if bin == "" {
			bin = "claude"
		}
		args := []string{
			"-p",
			"--output-format", "json",
			"--max-turns", strconv.Itoa(maxTurns),
			// With no permission prompt to answer, a tool that is not
			// allowed is refused rather than waited on. That is the whole
			// enforcement: unlike a factory session, a review session is
			// never given --dangerously-skip-permissions.
			"--permission-prompts", "none",
			"--tools", strings.Join(ReadOnlyTools, ","),
			"--setting-sources", "",
			"--allowedTools", strings.Join(ReadOnlyTools, ","),
			"--disallowedTools", strings.Join(DeniedTools, ","),
			// An MCP server is another way to run something, and the
			// person's own servers are configured for their own work, not
			// for this. A review session gets none.
			"--mcp-config", `{"mcpServers":{}}`,
			"--strict-mcp-config",
		}
		if a.Model != "" {
			args = append(args, "--model", a.Model)
		}
		// Claude can switch to another claude model itself; a fallback on
		// another CLI is a new session, Run's to start.
		if f := a.Fallback; f != nil && (f.Provider == "" || f.Provider == config.AgentClaude) && f.Model != "" && f.Model != a.Model {
			args = append(args, "--fallback-model", f.Model)
		}
		if a.Effort != "" {
			args = append(args, "--effort", a.Effort)
		}
		if req.ResumeID != "" {
			args = append(args, "--resume", req.ResumeID)
		}
		return bin, args, nil
	case config.AgentCodex:
		bin := a.CodexBin
		if bin == "" {
			bin = "codex"
		}
		args := append([]string{"exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check"}, codexReviewConfigArgs()...)
		if a.Model != "" {
			args = append(args, "--model", a.Model)
		}
		if a.Effort != "" {
			effort := a.Effort
			if effort == "max" {
				effort = "high"
			}
			args = append(args, "-c", "model_reasoning_effort="+strconv.Quote(effort))
		}
		args = append(args, "-")
		return bin, args, nil
	}
	return "", nil, fmt.Errorf("review: unknown provider %s (want one of %s)", strconv.Quote(a.Provider), strings.Join(SupportedProviders, ", "))
}

// codexReviewConfigArgs restricts both MCP discovery and the review session.
// Read-only sandboxing blocks writes but needs separate tool restrictions.
func codexReviewConfigArgs() []string {
	args := []string{"-c", `approval_policy="never"`, "-c", `web_search="disabled"`,
		"-c", "orchestrator.mcp.enabled=false"}
	for _, feature := range []string{"shell_tool", "unified_exec", "js_repl", "browser_use", "browser_use_external", "computer_use", "in_app_browser", "multi_agent", "multi_agent_v2", "apps", "plugins", "hooks", "codex_hooks", "plugin_hooks", "skill_mcp_dependency_install", "tool_suggest", "web_search_request", "web_search_cached"} {
		args = append(args, "-c", "features."+feature+"=false")
	}
	return args
}

// sessionEnd is what a CLI said at the end of a session, in the terms both
// of them can be read in.
type sessionEnd struct {
	id    string
	text  string
	turns int
	// cost is what the CLI reported the session cost, and costKnown
	// whether it reported one at all.
	cost      float64
	costKnown bool
	// err is what the session failed with, and "" for one that finished.
	err string
}

// read turns a CLI's output into that end.
func (a *CLIAgent) read(out []byte) (*sessionEnd, error) {
	if a.Provider == config.AgentCodex {
		return readCodex(out)
	}
	return readClaude(out)
}

// claudeResult is `claude -p --output-format json`: one object, printed when
// the session ends.
type claudeResult struct {
	IsError      bool    `json:"is_error"`
	Subtype      string  `json:"subtype"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
}

func readClaude(out []byte) (*sessionEnd, error) {
	var res claudeResult
	if err := json.Unmarshal(bytes.TrimSpace(out), &res); err != nil {
		return nil, errors.New("claude printed no result")
	}
	end := &sessionEnd{id: res.SessionID, text: res.Result, turns: res.NumTurns, cost: res.TotalCostUSD, costKnown: true}
	if res.IsError {
		end.err = failure(res.Subtype, res.Result)
	}
	return end, nil
}

// codexEvent is one line of `codex exec --json`, reduced to what a review
// session is read for.
type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Item     struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"item"`
	Error struct {
		Message string `json:"message"`
	} `json:"error"`
	Message string `json:"message"`
}

// readCodex reads codex's event stream: the thread id is the session id, the
// last agent message is what the session said, every completed item is a
// turn, and the turn ends with "turn.completed", "turn.failed" or a bare
// "error". Codex reports no cost.
func readCodex(out []byte) (*sessionEnd, error) {
	end := &sessionEnd{}
	ended := false
	for _, line := range bytes.Split(out, []byte("\n")) {
		var ev codexEvent
		if err := json.Unmarshal(bytes.TrimSpace(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "thread.started":
			end.id = ev.ThreadID
		case "item.completed":
			end.turns++
			if ev.Item.Type == "agent_message" && ev.Item.Text != "" {
				end.text = ev.Item.Text
			}
		case "turn.completed":
			ended = true
		case "turn.failed", "error":
			ended = true
			msg := ev.Error.Message
			if msg == "" {
				msg = ev.Message
			}
			end.err = failure(strings.ReplaceAll(ev.Type, ".", "_"), msg)
		}
	}
	if !ended {
		return nil, errors.New("codex ended without finishing the turn")
	}
	return end, nil
}

// failure is how a failed session is named: what the CLI called the failure,
// and what it said about it.
func failure(subtype, message string) string {
	if subtype == "" {
		subtype = "failed"
	}
	if message = strings.TrimSpace(message); message == "" {
		return subtype
	}
	return subtype + ": " + message
}

// tail is the end of what a CLI printed on stderr, appended to the error
// that says it failed. It is bounded: a CLI that failed on every line of a
// long prompt would otherwise print the prompt back.
func tail(s string) string {
	if s = strings.TrimSpace(s); s == "" {
		return ""
	}
	if len(s) > 400 {
		s = "..." + s[len(s)-400:]
	}
	return ": " + s
}

// A review must not inherit factory identity or Git access overrides, even when
// its caller is itself a factory session or the role's env names those keys.
func reviewEnvironment(inherited []string, overrides map[string]string) []string {
	env := map[string]string{}
	for _, entry := range inherited {
		k, v, ok := strings.Cut(entry, "=")
		if ok {
			env[k] = v
		}
	}
	maps.Copy(env, overrides)
	out := []string{}
	for _, k := range slices.Sorted(maps.Keys(env)) {
		if strings.HasPrefix(k, "BEES_") || strings.HasPrefix(k, "GIT_") {
			continue
		}
		out = append(out, k+"="+env[k])
	}
	return out
}
