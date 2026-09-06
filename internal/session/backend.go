package session

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// A backend is one CLI a session can run as, chosen by the role's resolved
// agent setting (config.AgentClaude or config.AgentCodex). The runner owns
// everything a session is regardless of its backend — the session
// directory, the prompt files, the environment, the process group, the
// timeout, the transcript, the pid file, the outcome and the result file —
// and asks the backend for the two things that differ: the command line that
// starts the CLI, and how to read what it printed.
type backend interface {
	// command builds the executable and its arguments, and what to write to
	// its stdin. paths tells it where the runner wrote the session's files.
	command(ctx context.Context, r *Runner, req Request, paths sessionPaths) (bin string, args []string, stdin string, err error)
	// consume reads the CLI's stdout to its end, copying every line to the
	// transcript (and to r.Stream when set), and returns what the stream
	// said at its end: nil when it ended without saying.
	consume(r *Runner, stdout io.Reader, transcript io.Writer) (*streamEnd, *RateLimit, error)
}

// sessionPaths are the files the runner writes for a session before the CLI
// starts, which a backend's command line may refer to.
type sessionPaths struct {
	dir          string
	systemPrompt string
	prompt       string
}

// streamEnd is what a backend read off the end of a session's stream, in
// the terms Result is written in: the fields a backend cannot supply stay
// zero, and CostKnown says whether the cost is one.
type streamEnd struct {
	SessionID string
	Result    string
	IsError   bool
	// Subtype is "success" for a session that finished, and otherwise names
	// the failure the way the error subtypes do.
	Subtype   string
	NumTurns  int
	CostUSD   float64
	CostKnown bool
}

// backendFor returns the backend the role's agent setting names. The
// setting is validated when bees.toml loads, so an unknown value here is a
// role that never went through config (a test's hand-built ResolvedRole)
// and the empty value is the default, claude.
func backendFor(agent string) (backend, error) {
	switch agent {
	case "", config.AgentClaude:
		return claudeBackend{}, nil
	case config.AgentCodex:
		return codexBackend{}, nil
	}
	return nil, errors.New("session: unknown agent " + strconv.Quote(agent))
}

// claudeBackend runs a session as `claude -p`.
type claudeBackend struct{}

func (claudeBackend) command(ctx context.Context, r *Runner, req Request, paths sessionPaths) (string, []string, string, error) {
	bin := r.ClaudeBin
	if bin == "" {
		bin = "claude"
	}
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--append-system-prompt-file", paths.systemPrompt,
		"--model", req.Role.Model,
		"--max-turns", strconv.Itoa(req.Role.MaxTurns),
		"--name", "bees-" + req.Name,
	}
	if req.Role.FallbackModel != "" && req.Role.FallbackModel != req.Role.Model {
		args = append(args, "--fallback-model", req.Role.FallbackModel)
	}
	if req.Role.Effort != "" {
		args = append(args, "--effort", req.Role.Effort)
	}
	for _, d := range r.AddDirs {
		args = append(args, "--add-dir", d)
	}
	if len(req.Role.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(req.Role.AllowedTools, ","))
	}
	if len(req.Role.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(req.Role.DisallowedTools, ","))
	}
	// Every session gets the built-in bees server next to whatever bees.toml
	// configures, so mcp.json is always written.
	mcpPath := filepath.Join(paths.dir, "mcp.json")
	if err := WriteMCPConfig(mcpPath, r.mcpEntries(req, paths.dir)); err != nil {
		return "", nil, "", err
	}
	args = append(args, "--mcp-config", mcpPath, "--strict-mcp-config")
	if len(req.Role.Skills) > 0 {
		if r.Skills == nil {
			return "", nil, "", errors.New("session: skills configured but no skills manager")
		}
		dirs, err := r.Skills.Prepare(ctx, req.Role.Skills)
		if err != nil {
			return "", nil, "", err
		}
		for _, d := range dirs {
			args = append(args, "--plugin-dir", d)
		}
	}
	return bin, args, req.Prompt, nil
}

// streamResult is the final "result" event of claude's stream-json output.
type streamResult struct {
	Type         string  `json:"type"`
	Subtype      string  `json:"subtype"`
	IsError      bool    `json:"is_error"`
	Result       string  `json:"result"`
	SessionID    string  `json:"session_id"`
	NumTurns     int     `json:"num_turns"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	DurationMS   int64   `json:"duration_ms"`
}

// rateLimitEvent is a "rate_limit_event" of claude's stream-json output:
// what the account's capacity looks like right now. Only the three fields
// the factory acts on are parsed — the nested unifiedWindows are not
// needed, and reading status as a field is what keeps the "overageStatus"
// of the same object from being mistaken for it.
type rateLimitEvent struct {
	Info struct {
		Status   string `json:"status"`
		Type     string `json:"rateLimitType"`
		ResetsAt int64  `json:"resetsAt"`
	} `json:"rate_limit_info"`
}

// consume reads claude's stream-json: the final "result" event carries the
// result text, the turn count, the cost and the session id, and the last
// "rate_limit_event" is what the account's capacity looked like.
func (claudeBackend) consume(r *Runner, stdout io.Reader, transcript io.Writer) (*streamEnd, *RateLimit, error) {
	var final *streamResult
	var limit *RateLimit
	err := r.tee(stdout, transcript, func(line []byte, typ string) {
		switch typ {
		case "result":
			var sr streamResult
			if err := json.Unmarshal(line, &sr); err == nil {
				final = &sr
			}
		case "rate_limit_event":
			var ev rateLimitEvent
			if err := json.Unmarshal(line, &ev); err != nil {
				return
			}
			rl := &RateLimit{Status: ev.Info.Status, Type: ev.Info.Type}
			if ev.Info.ResetsAt > 0 {
				rl.ResetsAt = time.Unix(ev.Info.ResetsAt, 0)
			}
			limit = rl
		}
	})
	if final == nil {
		return nil, limit, err
	}
	return &streamEnd{
		SessionID: final.SessionID,
		Result:    final.Result,
		IsError:   final.IsError,
		Subtype:   final.Subtype,
		NumTurns:  final.NumTurns,
		CostUSD:   final.TotalCostUSD,
		CostKnown: true,
	}, limit, err
}

// codexBackend runs a session as `codex exec --json`, Codex CLI's
// non-interactive mode.
//
// What differs from claude, and how each difference is met:
//
//   - Approvals and the sandbox are switched off with
//     --dangerously-bypass-approvals-and-sandbox, the counterpart of
//     --dangerously-skip-permissions.
//   - There is no flag to append to the system prompt, so the rendered system
//     prompt is written ahead of the task on stdin, separated by a rule. The
//     two files in the session directory are still written apart, as they are
//     for claude, and the prompt argument is `-`, which tells codex to read
//     stdin.
//   - There is no --mcp-config: MCP servers are configuration, so each one
//     is passed as `-c mcp_servers.<name>.<key>=<value>` overrides — the
//     built-in bees server included, with the session's BEES_* variables as
//     its env, the way mcp.json carries them for claude. Every value is a
//     JSON string or a JSON array of strings, which codex parses whether it
//     reads its overrides as JSON or as TOML (a JSON object is not a TOML
//     inline table, so no override is one). Codex starts an MCP server with
//     a small fixed environment plus that env, not with its own, so the
//     built-in server sees only what the override names.
//   - The model goes as -m when the role resolved one: with agent = "codex"
//     the model keys default to empty (config), and an empty model leaves
//     the choice to codex's own configuration. There is no fallback model.
//   - Effort goes as the model_reasoning_effort configuration key. Codex's
//     levels stop at high, so "max" is passed as "high".
//   - max_turns, allowed_tools, disallowed_tools, skills and the --add-dir
//     list have no counterpart and are not passed.
//   - The stream is JSON lines of events: "thread.started" names the thread
//     (the session id), "item.completed" is one action taken — a message,
//     a command, a file change, an MCP call — and the turn ends with
//     "turn.completed" or "turn.failed". There is no cost in it: codex
//     reports tokens, and turning those into dollars needs a price table,
//     so a codex session's cost is unknown rather than zero.
type codexBackend struct{}

func (codexBackend) command(_ context.Context, r *Runner, req Request, paths sessionPaths) (string, []string, string, error) {
	bin := r.CodexBin
	if bin == "" {
		bin = "codex"
	}
	args := []string{
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
	}
	if req.Role.Model != "" {
		args = append(args, "--model", req.Role.Model)
	}
	if req.Role.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+codexValue(codexEffort(req.Role.Effort)))
	}
	for _, o := range codexMCPOverrides(r.mcpEntries(req, paths.dir)) {
		args = append(args, "-c", o)
	}
	args = append(args, "-")
	stdin := req.Prompt
	if req.SystemPrompt != "" {
		stdin = req.SystemPrompt + "\n\n---\n\n" + req.Prompt
	}
	return bin, args, stdin, nil
}

// codexEffort maps a configured effort to a codex reasoning level. The
// four accepted values are claude's; codex has no "max", and "high" is the
// closest it offers.
func codexEffort(effort string) string {
	if effort == "max" {
		return "high"
	}
	return effort
}

// codexValue renders a string as the value of a codex -c override: a JSON
// string literal, which is also a TOML basic string.
func codexValue(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// codexList renders a list of strings the same way.
func codexList(l []string) string {
	if l == nil {
		l = []string{}
	}
	b, _ := json.Marshal(l)
	return string(b)
}

// codexMCPOverrides renders MCP entries as `-c` overrides of codex's
// mcp_servers table, one per key, in a stable order: stdio servers by
// command, args and env; remote ones by url and http_headers. Names are
// sorted so two sessions of one role build the same command line.
func codexMCPOverrides(entries map[string]MCPEntry) []string {
	var out []string
	for _, name := range sortedKeys(entries) {
		e := entries[name]
		prefix := "mcp_servers." + name + "."
		if e.Command != "" {
			out = append(out, prefix+"command="+codexValue(e.Command))
			if len(e.Args) > 0 {
				out = append(out, prefix+"args="+codexList(e.Args))
			}
			for _, k := range sortedKeys(e.Env) {
				out = append(out, prefix+"env."+k+"="+codexValue(e.Env[k]))
			}
			continue
		}
		if e.URL != "" {
			out = append(out, prefix+"url="+codexValue(e.URL))
			for _, k := range sortedKeys(e.Headers) {
				out = append(out, prefix+"http_headers."+k+"="+codexValue(e.Headers[k]))
			}
		}
	}
	return out
}

// codexEvent is one line of `codex exec --json`, reduced to the fields the
// runner reads.
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

// consume reads codex's event stream. The thread id is the session id, the
// last agent message is the result text, every completed item is a turn,
// and "turn.completed" or "turn.failed" says how it ended; an "error" event
// after the turn is a failure too. A stream that ends with neither ended
// without saying, like a claude stream with no result event. Codex has no
// rate-limit event of its own; a session it refuses to run reports the
// limit in the failure's message, which SessionLimited reads.
func (codexBackend) consume(r *Runner, stdout io.Reader, transcript io.Writer) (*streamEnd, *RateLimit, error) {
	var end *streamEnd
	var threadID, lastMessage string
	turns := 0
	err := r.tee(stdout, transcript, func(line []byte, typ string) {
		var ev codexEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return
		}
		switch typ {
		case "thread.started":
			threadID = ev.ThreadID
		case "item.completed":
			turns++
			if ev.Item.Type == "agent_message" && ev.Item.Text != "" {
				lastMessage = ev.Item.Text
			}
		case "turn.completed":
			end = &streamEnd{Subtype: "success"}
		case "turn.failed", "error":
			// The subtype alone marks the failure: Run reports any end
			// whose subtype is not "success" as an error, whatever the
			// exit code, so a turn codex gave up on with a clean exit is
			// still one.
			msg := ev.Error.Message
			if msg == "" {
				msg = ev.Message
			}
			end = &streamEnd{Subtype: strings.ReplaceAll(typ, ".", "_"), Result: msg}
		}
	})
	if end == nil {
		return nil, nil, err
	}
	end.SessionID = threadID
	end.NumTurns = turns
	if end.Result == "" {
		end.Result = lastMessage
	}
	return end, nil, err
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// mcpEntries is what a session's MCP servers are, whatever backend runs it:
// the resolved role's servers and the built-in bees server.
func (r *Runner) mcpEntries(req Request, sessionDir string) map[string]MCPEntry {
	entries := MCPEntries(req.Role.MCP)
	entries[config.BuiltinMCPServer] = r.builtinMCP(req, sessionDir)
	return entries
}
