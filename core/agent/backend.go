package agent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/core/agent/procs"
)

// A backend is one CLI a session can run as, chosen by the role's resolved
// agent setting (AgentClaude, AgentCodex, AgentOpenCode or
// AgentPi): the implementation half of a Backend descriptor, which
// carries the per-agent facts. The runner owns everything a session is
// regardless of its backend — the session directory, the prompt files, the
// environment, the process group, the timeout, the transcript, the pid
// file, the outcome and the result file — and asks the backend for the two
// things that differ: the command line that starts the CLI, and how to read
// what it printed.
type backend interface {
	// command builds the executable and its arguments, what to write to its
	// stdin, and the variables to add to the session's environment: the
	// ones a CLI is configured through when it has no flag, laid over the
	// environment the runner builds (on the host, in a container or in a
	// sandbox). b is the backend's own descriptor: the executable the
	// session runs comes from it (Backend.executable), so an
	// implementation never names its own command. paths tells it where
	// the runner wrote the session's files.
	command(ctx context.Context, r *Runner, b Backend, req Request, paths sessionPaths) (bin string, args []string, stdin string, env []envVar, err error)
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
	// mcp are the session's MCP servers, the built-in one included: the
	// runner decides how that one is reached (a caller-owned server the agent
	// starts, or the host's HTTP server for a container or sandbox session).
	mcp map[string]MCPEntry
	// turn is the verified request.
	turn *Turn
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

// claudeBackend runs a session as `claude -p`.
type claudeBackend struct{}

func (claudeBackend) command(ctx context.Context, r *Runner, b Backend, req Request, paths sessionPaths) (string, []string, string, []envVar, error) {
	bin := b.executable(r)
	boxed := req.Profile.Sandbox == SandboxClaude
	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
	}
	if boxed {
		// Inside the box the permission layer is what holds the built-in
		// tools, which run in the claude process and not under the OS
		// sandbox: acceptEdits lets Write and Edit work in the worktree
		// and the --add-dir state dir and asks about anything else, and
		// with nobody to ask, "none" refuses it. What the session may do
		// without asking is the allow list of the settings block below.
		// --dangerously-skip-permissions would answer yes to every one of
		// those questions, and a Write anywhere on the machine with it.
		args = append(args, "--permission-mode", "acceptEdits", "--permission-prompts", "none")
	} else {
		args = append(args, "--dangerously-skip-permissions")
	}
	args = append(args,
		"--append-system-prompt-file", paths.systemPrompt,
		"--model", req.Profile.Model,
		"--max-turns", strconv.Itoa(req.Profile.MaxTurns),
		"--name", r.namePrefix()+req.Name,
	)
	// Claude can switch to another claude model itself; a fallback on
	// another agent is a new session, the caller's to run.
	if f := req.Profile.Fallback; f != nil && (f.Agent == "" || f.Agent == AgentClaude) && f.Model != "" && f.Model != req.Profile.Model {
		args = append(args, "--fallback-model", f.Model)
	}
	if req.Profile.Effort != "" {
		args = append(args, "--effort", req.Profile.Effort)
	}
	if req.ResumeID != "" {
		// Claude renders the system prompt once, on a conversation's first
		// request, and by default every later request reuses that recording
		// verbatim (--system-prompt-snapshot on): the system prompt written
		// for this round would be ignored, and what it says about the round
		// would stay what round 1 said. Turning the snapshot off for the
		// resumed launch makes claude read this round's file.
		args = append(args, "--resume", req.ResumeID, "--system-prompt-snapshot", "off")
	}
	addDirs := r.AddDirs
	if paths.turn != nil && !req.Profile.isolated() {
		addDirs = paths.turn.WriteDirs
	}
	for _, d := range addDirs {
		args = append(args, "--add-dir", d)
	}
	if paths.turn != nil && paths.turn.Tools != nil {
		// --tools is the built-in set the session has at all; the
		// permission flags below only decide what runs without asking.
		args = append(args, "--tools", strings.Join(paths.turn.Tools, ","))
	}
	if len(req.Profile.AllowedTools) > 0 {
		args = append(args, "--allowedTools", strings.Join(req.Profile.AllowedTools, ","))
	}
	if len(req.Profile.DisallowedTools) > 0 {
		args = append(args, "--disallowedTools", strings.Join(req.Profile.DisallowedTools, ","))
	}
	// Write the caller's complete server set, including an empty set, so
	// local MCP configuration does not add unexpected servers.
	mcpPath := filepath.Join(paths.dir, "mcp.json")
	if err := WriteMCPConfig(mcpPath, paths.mcp); err != nil {
		return "", nil, "", nil, err
	}
	args = append(args, "--mcp-config", mcpPath, "--strict-mcp-config")
	if boxed {
		var denied []string
		if paths.turn != nil {
			denied = paths.turn.DeniedExecutables
		}
		settings, err := claudeSandboxSettings(sortedKeys(paths.mcp), runtime.GOOS, req.Profile.SandboxDomains, denied)
		if err != nil {
			return "", nil, "", nil, err
		}
		if err := os.WriteFile(filepath.Join(paths.dir, sandboxFile), settings, 0o644); err != nil {
			return "", nil, "", nil, err
		}
		args = append(args, "--settings", string(settings))
	}
	if len(req.Profile.Skills) > 0 {
		if r.Skills == nil {
			return "", nil, "", nil, errors.New("session: skills configured but no skills manager")
		}
		dirs, err := r.Skills.Prepare(ctx, req.Profile.Skills)
		if err != nil {
			return "", nil, "", nil, err
		}
		for _, d := range dirs {
			args = append(args, "--plugin-dir", d)
		}
	}
	return bin, args, req.Prompt, nil, nil
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
//     caller-owned server included, with the session context as
//     its env, the way mcp.json carries them for claude. Every value is a
//     JSON string or a JSON array of strings, which codex parses whether it
//     reads its overrides as JSON or as TOML (a JSON object is not a TOML
//     inline table, so no override is one). Codex starts an MCP server with
//     a small fixed environment plus that env, not with its own, so the
//     built-in server sees only what the override names.
//   - The model goes as -m when the role resolved one: with agent = "codex"
//     the model keys default to empty (config), and an empty model leaves
//     the choice to codex's own configuration. There is no fallback-model
//     flag: a fallback profile is the caller's to run.
//   - Effort goes as the model_reasoning_effort configuration key. Codex's
//     levels stop at high, so "max" is passed as "high".
//   - max_turns, allowed_tools, disallowed_tools, skills and the --add-dir
//     list have no counterpart and are not passed.
//   - Request.ResumeID is ignored: `codex exec` has no resume subcommand or
//     flag (codex-cli 0.0.2506052246), so a later round of a codex role is
//     a new thread whatever id the caller knows.
//   - The stream is JSON lines of events: "thread.started" names the thread
//     (the session id), "item.completed" is one action taken — a message,
//     a command, a file change, an MCP call — and the turn ends with
//     "turn.completed", "turn.failed" or a bare "error" event. There is no
//     cost in it: codex reports tokens, and turning those into dollars
//     needs a price table, so a codex session's cost is unknown rather
//     than zero.
type codexBackend struct{}

func (codexBackend) command(_ context.Context, r *Runner, b Backend, req Request, paths sessionPaths) (string, []string, string, []envVar, error) {
	bin := b.executable(r)
	args := []string{
		"exec",
		"--json",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		// A path-bearing marker independent of optional MCP configuration.
		"-c", procs.CodexMarker(r.EnvironmentPrefix) + codexValue(paths.dir),
	}
	if req.Profile.Model != "" {
		args = append(args, "--model", req.Profile.Model)
	}
	if req.Profile.Effort != "" {
		args = append(args, "-c", "model_reasoning_effort="+codexValue(codexEffort(req.Profile.Effort)))
	}
	for _, o := range codexMCPOverrides(paths.mcp) {
		args = append(args, "-c", o)
	}
	args = append(args, "-")
	stdin := req.Prompt
	if req.SystemPrompt != "" {
		stdin = req.SystemPrompt + "\n\n---\n\n" + req.Prompt
	}
	return bin, args, stdin, nil, nil
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
// command, args, env and env_vars; remote ones by url and http_headers. Names are
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
			if len(e.EnvVars) > 0 {
				out = append(out, prefix+"env_vars="+codexList(e.EnvVars))
			}
			for _, k := range sortedKeys(e.Env) {
				out = append(out, prefix+"env."+k+"="+codexValue(e.Env[k]))
			}
			continue
		}
		if e.URL != "" {
			out = append(out, prefix+"url="+codexValue(e.URL))
			if e.BearerTokenEnv != "" {
				out = append(out, prefix+"bearer_token_env_var="+codexValue(e.BearerTokenEnv))
			}
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

// opencodeBackend runs a session as `opencode run --format json`,
// opencode's non-interactive mode.

// makeSuccessEnd creates a generic successful streamEnd used when a backend
// finishes without an explicit end event.
func makeSuccessEnd(sessionID, result string, turns int, cost float64, costKnown bool) *streamEnd {
	return &streamEnd{
		SessionID: sessionID,
		Result:    result,
		IsError:   false,
		Subtype:   "success",
		NumTurns:  turns,
		CostUSD:   cost,
		CostKnown: costKnown,
	}
}

// opencode's non-interactive mode.
//
// What differs from claude and from codex, and how each difference is met:
//
//   - Permissions: opencode's default agent runs every tool without
//     asking, except the few that "ask" by default (writing outside the
//     project directory, which is what the state directory is to a session,
//     and a tool call repeated too often), and a non-interactive run
//     rejects what would ask. --auto is passed, the counterpart of
//     --dangerously-skip-permissions: it approves those and leaves an
//     explicit "deny" in the project's own configuration in force.
//   - opencode has no flag to append to the system prompt, no --mcp-config
//     and no --add-dir, but it reads one more configuration file from the
//     path OPENCODE_CONFIG names, merged over its global one and under the
//     project's own opencode.json. The session gets such a file,
//     opencode.json in the session directory, and OPENCODE_CONFIG pointing
//     at it (opencodeConfig): its `instructions` entry lists the rendered
//     system prompt file, which opencode appends to its own system prompt
//     the way --append-system-prompt-file does for claude; its `mcp` table
//     is every MCP server, any caller-owned server included, with the
//     session context as the server's environment. The file is
//     never written into the worktree: the project's own opencode.json, when
//     it has one, is read as well, so the session sees the project's
//     servers next to these. opencode starts a stdio server with the
//     entry's environment laid over its own, so the built-in server sees
//     the session's environment the way it does under claude.
//   - The model goes as --model when the role resolved one: with agent =
//     "opencode" the model keys default to empty (config), and an empty
//     model leaves the choice to opencode's own configuration. There is no
//     fallback-model flag: a fallback profile is the caller's to run.
//   - Request.ResumeID goes as --session, opencode's way of continuing an
//     earlier session; opencode reads the instruction files again on each
//     request, so the round's own system prompt is what a resumed session
//     gets, with no snapshot to switch off. The session is titled after
//     the session name, as claude's is named.
//   - Effort goes as the default build agent's `variant` in opencodeConfig.
//     opencode calls variants names the model defines (anthropic's are
//     "high" and "max", openai's "low" to "xhigh"), not levels, so the
//     profile's value is passed through for the configured model to interpret.
//   - max_turns, allowed_tools, disallowed_tools and skills have no
//     counterpart and are not passed.
//   - The stream is JSON lines of events, each carrying the session id as
//     sessionID: "text" is a message the model wrote (the whole text, not a
//     delta), "tool_use" a tool call that completed, "step_finish" one
//     model step done (a turn), with what the step cost in `part.cost`, and
//     the run ends with a "step_finish" whose reason is "stop" or with an
//     "error" event. opencode reports each step's cost in dollars, so a
//     session's cost is the sum and is known — zero for a local model,
//     which is a real zero — unless no step finished at all.
type opencodeBackend struct{}

// EnvOpenCodeConfig is the variable opencode reads a configuration file's
// path from, set for every opencode session to the file opencodeConfig
// wrote.
const EnvOpenCodeConfig = "OPENCODE_CONFIG"

// OpenCodeConfigFile is the name of that file in the session directory.
const OpenCodeConfigFile = "opencode.json"

func (opencodeBackend) command(_ context.Context, r *Runner, b Backend, req Request, paths sessionPaths) (string, []string, string, []envVar, error) {
	bin := b.executable(r)
	args := []string{
		"run",
		"--format", "json",
		"--auto",
		"--title", r.namePrefix() + req.Name,
	}
	if req.Profile.Model != "" {
		args = append(args, "--model", req.Profile.Model)
	}
	if req.ResumeID != "" {
		args = append(args, "--session", req.ResumeID)
	}
	configPath := filepath.Join(paths.dir, OpenCodeConfigFile)
	instructions := paths.systemPrompt
	if req.SystemPrompt == "" {
		instructions = ""
	}
	if err := writeOpenCodeConfig(configPath, instructions, paths.mcp, req.Profile.Effort); err != nil {
		return "", nil, "", nil, err
	}
	return bin, args, req.Prompt, []envVar{{EnvOpenCodeConfig, configPath}}, nil
}

// opencodeMCP is one server of opencode.json's mcp table: a local one by
// its command line (the executable and its arguments in one list) and
// environment, a remote one by its url and headers.
type opencodeMCP struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Enabled     bool              `json:"enabled"`
}

// opencodeAgent is the part of opencode.json that selects a model variant for
// the default build agent.
type opencodeAgent struct {
	Variant string `json:"variant,omitempty"`
}

// opencodeConfig is the configuration file an opencode session is given:
// the system prompt file as an instruction, the session's MCP servers and,
// when configured, the default build agent's model variant.
type opencodeConfig struct {
	Schema       string                   `json:"$schema"`
	Instructions []string                 `json:"instructions,omitempty"`
	Agent        map[string]opencodeAgent `json:"agent,omitempty"`
	MCP          map[string]opencodeMCP   `json:"mcp"`
}

// opencodeServers renders MCP entries as opencode.json's mcp table: a
// stdio entry is a "local" server, one with a url a "remote" one, and an
// entry with neither is left out.
func opencodeServers(entries map[string]MCPEntry) map[string]opencodeMCP {
	out := map[string]opencodeMCP{}
	for name, e := range entries {
		switch {
		case e.Command != "":
			out[name] = opencodeMCP{Type: "local", Command: append([]string{e.Command}, e.Args...), Environment: e.Env, Enabled: true}
		case e.URL != "":
			headers := e.Headers
			if e.BearerTokenEnv != "" {
				headers = bearerHeaders(e, "{env:"+e.BearerTokenEnv+"}")
			}
			out[name] = opencodeMCP{Type: "remote", URL: e.URL, Headers: headers, Enabled: true}
		}
	}
	return out
}

// writeOpenCodeConfig writes the session's opencode.json: instructions is
// the system prompt file, or empty when there is no system prompt; effort is
// the default build agent's variant, or empty when it is not configured.
func writeOpenCodeConfig(path, instructions string, entries map[string]MCPEntry, effort string) error {
	cfg := opencodeConfig{Schema: "https://opencode.ai/config.json", MCP: opencodeServers(entries)}
	if instructions != "" {
		cfg.Instructions = []string{instructions}
	}
	if effort != "" {
		cfg.Agent = map[string]opencodeAgent{"build": {Variant: effort}}
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// opencodeEvent is one line of `opencode run --format json`, reduced to
// the fields the runner reads.
type opencodeEvent struct {
	Type         string `json:"type"`
	SessionID    string `json:"sessionID"`
	SessionIDAlt string `json:"session_id"`
	Part         struct {
		Type   string  `json:"type"`
		Text   string  `json:"text"`
		Reason string  `json:"reason"`
		Cost   float64 `json:"cost"`
	} `json:"part"`
	Error struct {
		Name string `json:"name"`
		Data struct {
			Message string `json:"message"`
		} `json:"data"`
	} `json:"error"`
}

// consume reads opencode's event stream. The session id is on every event,
// the last text event is the result text, every finished step is a turn
// and the steps' costs add up to the session's; a "step_finish" whose
// reason is "stop" says the run ended well, and an "error" event that it
// did not, with the error's message as the result text. A stream that ends
// with neither ended without saying, like a claude stream with no result
// event. opencode has no rate-limit event of its own; a provider that
// refused the request reports it in the error's message, which
// SessionLimited reads.
func (opencodeBackend) consume(r *Runner, stdout io.Reader, transcript io.Writer) (*streamEnd, *RateLimit, error) {
	var end *streamEnd
	var sessionID, lastText string
	turns, cost, costKnown := 0, 0.0, false
	err := r.tee(stdout, transcript, func(line []byte, typ string) {
		var ev opencodeEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return
		}
		if sessionID == "" {
			if ev.SessionID != "" {
				sessionID = ev.SessionID
			} else {
				sessionID = ev.SessionIDAlt
			}
		}
		switch typ {
		case "text":
			if ev.Part.Text != "" {
				lastText = ev.Part.Text
			}
		case "step_finish":
			turns++
			cost += ev.Part.Cost
			costKnown = true
			switch ev.Part.Reason {
			case "stop":
				end = &streamEnd{Subtype: "success"}
			case "", "tool-calls":
				// The step ended to call tools, or without saying why:
				// the run goes on.
			default:
				// The model stopped for a reason of its own — length,
				// content-filter — which is a run that did not finish.
				end = &streamEnd{Subtype: "step_" + strings.ReplaceAll(ev.Part.Reason, "-", "_")}
			}
		case "error":
			// The subtype alone marks the failure, as it does for codex:
			// Run reports any end whose subtype is not "success" as an
			// error, whatever the exit code.
			msg := ev.Error.Data.Message
			if msg == "" {
				msg = ev.Error.Name
			}
			end = &streamEnd{Subtype: "error", Result: msg}
		}
	})
	if end == nil {
		if lastText != "" {
			end = makeSuccessEnd(sessionID, lastText, turns, cost, costKnown)
		} else {
			return nil, nil, err
		}
	}
	end.SessionID = sessionID
	end.NumTurns = turns
	end.CostUSD, end.CostKnown = cost, costKnown
	if end.Result == "" {
		end.Result = lastText
	}
	return end, nil, err
}

// sortedKeys returns a map's keys in order.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}
