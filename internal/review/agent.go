package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// A review session reads and says what it read. It never writes a file,
// never runs a command and never reaches the network: CI owns tests and
// builds, and a review that changed the thing it is reviewing would be a
// different tool. The distiller is the first such session and every angle
// session after it runs under the same restriction, so it is set here once.
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

// AgentRequest is one review session.
type AgentRequest struct {
	// Name identifies the session in an error message.
	Name string
	// Prompt is the whole of what the session is told: a review session has
	// no separate system prompt, because codex takes none.
	Prompt string
	// Dir is the directory the session runs in, which is what its
	// read-only tools can reach. It must be a directory the review is
	// willing to have read: the checkout of the repository under review, or
	// an empty one.
	Dir string
	// ResumeID continues an earlier session of this agent (AgentResult.ID)
	// instead of starting one, for the triage action that asks an angle a
	// follow-up question. Only claude can; codex has no resume and ignores
	// it.
	ResumeID string
}

// AgentResult is what a finished review session produced.
type AgentResult struct {
	// ID is the agent's own id for the session, which resumes it later.
	ID string
	// Text is the session's last message, which is what it was asked for.
	Text string
	// Turns is how many turns it took, and CostUSD what it cost when the
	// CLI reported one: codex reports none, so its cost is zero rather
	// than known.
	Turns   int
	CostUSD float64
}

// Agent runs one review session. It is the seam every session in the
// pipeline goes through: the distiller here, the angle sessions after it.
type Agent interface {
	Run(ctx context.Context, req AgentRequest) (*AgentResult, error)
}

// CLIAgent runs a review session as the coding agent the person configured,
// `claude -p` or `codex exec`. It is the whole of what `bees review` needs
// from an agent, and deliberately not the factory's session runner
// (internal/session): that one gives every session the built-in bees MCP
// server and the BEES_* environment of the factory it belongs to, and
// `bees review` reviews any pull request on GitHub without one.
type CLIAgent struct {
	// Provider is the CLI to run, one of config.Agents, and Model the model
	// it runs. An empty model leaves the choice to the CLI.
	Provider string
	Model    string
	// ClaudeBin and CodexBin are the executables, "claude" and "codex" when
	// they are empty.
	ClaudeBin string
	CodexBin  string
	// Timeout and MaxTurns bound the session, DefaultSessionTimeout and
	// DefaultMaxTurns when they are zero.
	Timeout  time.Duration
	MaxTurns int
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
	cmd.Env = os.Environ()
	// A session that ran out of time is killed with everything it started,
	// the way the factory's runner does it: the CLI's own children hold the
	// pipes open, so killing the CLI alone would leave the review waiting
	// for them.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 10 * time.Second
	runErr := cmd.Run()

	res, parseErr := a.read(stdout.Bytes())
	switch {
	case res != nil && res.err != "":
		return nil, fmt.Errorf("%s session: %s", req.Name, res.err)
	case runErr != nil:
		return nil, fmt.Errorf("%s session: %w%s", req.Name, runErr, tail(stderr.String()))
	case parseErr != nil:
		return nil, fmt.Errorf("%s session: %w%s", req.Name, parseErr, tail(stderr.String()))
	}
	return &AgentResult{ID: res.id, Text: res.text, Turns: res.turns, CostUSD: res.cost}, nil
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
		if req.ResumeID != "" {
			args = append(args, "--resume", req.ResumeID)
		}
		return bin, args, nil
	case config.AgentCodex:
		bin := a.CodexBin
		if bin == "" {
			bin = "codex"
		}
		// Codex has no tool list to restrict. Its own sandbox is the
		// restriction: read-only refuses a write and a command alike,
		// which is what the tool lists above add up to for claude.
		args := []string{"exec", "--json", "--sandbox", "read-only", "--skip-git-repo-check"}
		if a.Model != "" {
			args = append(args, "--model", a.Model)
		}
		args = append(args, "-")
		return bin, args, nil
	}
	return "", nil, fmt.Errorf("review: unknown provider %s (want one of %s)", strconv.Quote(a.Provider), strings.Join(config.Agents, ", "))
}

// sessionEnd is what a CLI said at the end of a session, in the terms both
// of them can be read in.
type sessionEnd struct {
	id    string
	text  string
	turns int
	cost  float64
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
	end := &sessionEnd{id: res.SessionID, text: res.Result, turns: res.NumTurns, cost: res.TotalCostUSD}
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
