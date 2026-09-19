package agent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// piBackend runs a session as `pi -p --mode json`, pi's non-interactive
// mode with its event stream on stdout.
//
// What differs from claude and from opencode, and how each difference is met:
//
//   - Pi has no MCP support of its own. Every pi session loads the
//     third-party pi-mcp-adapter extension (PiMCPAdapter) with -e, and hands
//     it the session's MCP servers through the adapter's --mcp-config flag:
//     pi-mcp.json in the session directory (writePiMCPConfig), the built-in
//     server included. PI_MCP_CONFIG_MODE=exclusive makes that file the only
//     configuration the adapter reads, the way --strict-mcp-config does for
//     claude: no ~/.config/mcp/mcp.json, no project .mcp.json. Every server
//     registers its tools directly (directTools), so a pi session sees the
//     same tools a claude session does rather than the adapter's single
//     proxy tool, and connects at startup (lifecycle "eager").
//   - --no-extensions keeps pi from loading the extensions it would discover
//     (its settings' packages, ~/.pi/agent/extensions, the project's): what a
//     session loads is the adapter and Profile.PiPackages, each with -e, and
//     an adapter a person also installed with `pi install` is not loaded a
//     second time from there. A missing package is installed by pi itself
//     on first use, into its own cache.
//   - Pi has no approvals and no sandbox of its own: every tool runs without
//     asking, the counterpart of --dangerously-skip-permissions, so none
//     and container are the sandboxes it runs in and claude's is refused
//     (Profile.Validate).
//   - The rendered system prompt goes as --append-system-prompt, which takes
//     a file's contents when given a path; the task goes on stdin, which pi
//     reads in print mode as the initial prompt.
//   - The model goes as --model when the role resolved one (provider/id):
//     with agent = "pi" the model keys default to empty, and an empty model
//     leaves the choice to pi's own settings. There is no fallback-model
//     flag: a fallback profile is the caller's to run.
//   - Effort goes as --thinking, whose levels include claude's four.
//   - Request.ResumeID goes as --session-id, which opens the session of that
//     id in the working directory's project and creates a new one under
//     that id when there is none, never asking. The session is titled with
//     --name, as claude's is.
//   - max_turns, allowed_tools, disallowed_tools and skills have no
//     counterpart and are not passed; a turn that restricts built-in tools
//     is refused (grants.go), as it is for codex and opencode.
//   - The stream is JSON lines: a "session" header carrying the session id,
//     then pi's agent events. A "message_end" of an assistant message is one
//     model response, with its cost in dollars in usage.cost.total and why
//     it stopped in stopReason; a "turn_end" is one turn. The run ended well
//     when the last assistant message stopped with "stop", and did not when
//     it stopped with "error" (errorMessage says why), "length" or
//     "aborted". Pi reports every response's cost, so a session's cost is the
//     sum and is known — zero for a local model — once one response ended.
type piBackend struct{}

// PiMCPAdapter is the pi package that gives pi MCP support, loaded by every
// pi session ahead of Profile.PiPackages.
const PiMCPAdapter = "npm:pi-mcp-adapter"

// PiMCPConfigFile is the adapter's configuration file in the session
// directory, passed as --mcp-config.
const PiMCPConfigFile = "pi-mcp.json"

// EnvPiMCPConfigMode is the variable the adapter reads to decide whether
// the --mcp-config file is the only configuration it reads; every pi
// session sets it to "exclusive".
const EnvPiMCPConfigMode = "PI_MCP_CONFIG_MODE"

func (piBackend) command(_ context.Context, r *Runner, req Request, paths sessionPaths) (string, []string, string, []envVar, error) {
	bin := r.PiBin
	if bin == "" {
		bin = "pi"
	}
	configPath := filepath.Join(paths.dir, PiMCPConfigFile)
	args := []string{
		"-p",
		"--mode", "json",
		"--no-extensions",
		"-e", PiMCPAdapter,
	}
	for _, pkg := range req.Profile.PiPackages {
		args = append(args, "-e", pkg)
	}
	args = append(args,
		"--mcp-config", configPath,
		"--name", r.namePrefix()+req.Name,
	)
	if req.SystemPrompt != "" {
		args = append(args, "--append-system-prompt", paths.systemPrompt)
	}
	if req.Profile.Model != "" {
		args = append(args, "--model", req.Profile.Model)
	}
	if req.Profile.Effort != "" {
		args = append(args, "--thinking", req.Profile.Effort)
	}
	if req.ResumeID != "" {
		args = append(args, "--session-id", req.ResumeID)
	}
	if err := writePiMCPConfig(configPath, paths.mcp); err != nil {
		return "", nil, "", nil, err
	}
	return bin, args, req.Prompt, []envVar{{EnvPiMCPConfigMode, "exclusive"}}, nil
}

// piServer is one server of the adapter's mcpServers table: a stdio one by
// its command, arguments and environment, a remote one by its url and
// headers.
type piServer struct {
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Lifecycle   string            `json:"lifecycle"`
	DirectTools bool              `json:"directTools"`
}

// piMCPConfig is the file the adapter is given with --mcp-config.
type piMCPConfig struct {
	MCPServers map[string]piServer `json:"mcpServers"`
}

// piServers renders MCP entries as the adapter's mcpServers table: a stdio
// entry by command, one with a url by url, an entry with neither left out.
// The adapter expands ${VAR} in env and header values, which is how a
// bearer token is referred to by the variable holding it, and runs a value
// that starts with "!" as a command; a value bees means literally is
// written with that "!" doubled, the adapter's escape for it.
func piServers(entries map[string]MCPEntry) map[string]piServer {
	out := map[string]piServer{}
	for name, e := range entries {
		s := piServer{Lifecycle: "eager", DirectTools: true}
		switch {
		case e.Command != "":
			s.Command, s.Args, s.Env = e.Command, e.Args, piLiteral(e.Env)
		case e.URL != "":
			s.URL, s.Headers = e.URL, piLiteral(e.Headers)
			if e.BearerTokenEnv != "" {
				s.Headers = bearerHeaders(MCPEntry{Headers: s.Headers}, "${"+e.BearerTokenEnv+"}")
			}
		default:
			continue
		}
		out[name] = s
	}
	return out
}

// piLiteral escapes the values the adapter would run as a command.
func piLiteral(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if strings.HasPrefix(v, "!") {
			v = "!" + v
		}
		out[k] = v
	}
	return out
}

// writePiMCPConfig writes the session's pi-mcp.json.
func writePiMCPConfig(path string, entries map[string]MCPEntry) error {
	data, err := json.MarshalIndent(piMCPConfig{MCPServers: piServers(entries)}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

// piEvent is one line of `pi --mode json`, reduced to the fields the runner
// reads: the header's session id and an ended message.
type piEvent struct {
	Type    string    `json:"type"`
	ID      string    `json:"id"`
	Message piMessage `json:"message"`
}

// piMessage is a message of pi's event stream. Only an assistant message
// carries a stop reason and a cost.
type piMessage struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
	Usage        struct {
		Cost struct {
			Total float64 `json:"total"`
		} `json:"cost"`
	} `json:"usage"`
}

// text is what an assistant message said, its text parts joined.
func (m piMessage) text() string {
	var parts []string
	for _, c := range m.Content {
		if c.Type == "text" && c.Text != "" {
			parts = append(parts, c.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// consume reads pi's event stream. The header names the session, every
// "turn_end" is a turn, every assistant "message_end" adds its cost, and the
// last one that stopped says how the run ended: "stop" well, "error" not,
// with its errorMessage as the result text, and any other reason pi ends a
// run with ("length", "aborted") not either, named after the reason. A
// response that stopped to call tools, or one pi retried, is followed by
// another, whose reason replaces it. A stream with no assistant message
// that stopped ended without saying, like a claude stream with no result
// event. Pi has no rate-limit event; a provider that refused the request
// says so in the error message, which SessionLimited reads.
func (piBackend) consume(r *Runner, stdout io.Reader, transcript io.Writer) (*streamEnd, *RateLimit, error) {
	var end *streamEnd
	var sessionID, lastText string
	turns, cost, costKnown := 0, 0.0, false
	err := r.tee(stdout, transcript, func(line []byte, typ string) {
		var ev piEvent
		if err := json.Unmarshal(line, &ev); err != nil {
			return
		}
		switch typ {
		case "session":
			sessionID = ev.ID
		case "turn_end":
			turns++
		case "message_end":
			m := ev.Message
			if m.Role != "assistant" {
				return
			}
			cost += m.Usage.Cost.Total
			costKnown = true
			if t := m.text(); t != "" {
				lastText = t
			}
			switch m.StopReason {
			case "stop":
				end = &streamEnd{Subtype: "success"}
			case "error":
				// The subtype alone marks the failure, as it does for
				// codex and opencode: Run reports any end whose subtype is
				// not "success" as an error, whatever the exit code.
				end = &streamEnd{Subtype: "error", Result: m.ErrorMessage}
			case "length", "aborted":
				end = &streamEnd{Subtype: "stop_" + m.StopReason}
			}
		}
	})
	if end == nil {
		return nil, nil, err
	}
	end.SessionID = sessionID
	end.NumTurns = turns
	end.CostUSD, end.CostKnown = cost, costKnown
	if end.Result == "" {
		end.Result = lastText
	}
	return end, nil, err
}
