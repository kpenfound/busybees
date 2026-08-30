// Package session runs one headless Claude Code session for a role.
//
// A session is `claude -p` executed inside a workspace with the role's
// resolved settings: model and fallback model, appended system prompt,
// skills (as plugin dirs), MCP servers and tool restrictions. Every session
// also gets the built-in bees MCP server (see internal/mcpserver). The session
// communicates back through an outcome file — written by the `done` tool or
// `bees done`, both through Report — and through the local mailbox.
package session

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/procs"
	"github.com/kpenfound/busybees/internal/skills"
)

// Environment variable names exported to every session.
const (
	EnvRole       = "BEES_ROLE"
	EnvSessionDir = "BEES_SESSION_DIR"
	EnvStateDir   = "BEES_STATE_DIR"
	EnvRepo       = "BEES_REPO"
	EnvLabel      = "BEES_LABEL"
	EnvIssue      = "BEES_ISSUE"
	EnvPR         = "BEES_PR"
	EnvBranch     = "BEES_BRANCH"
	EnvNotesFile  = "BEES_NOTES_FILE"
	EnvConfig     = "BEES_CONFIG"
	EnvBin        = "BEES_BIN"
)

// beesEnvPrefix marks the variables bees owns: they are set per session and
// never inherited from the process that started the scheduler.
const beesEnvPrefix = "BEES_"

// Request describes one session to run.
type Request struct {
	// Name identifies the session in logs, e.g. "developer-issue-12-r1".
	Name string
	Role config.ResolvedRole
	// WorkDir is the directory claude runs in (the worktree).
	WorkDir string
	// SystemPrompt is appended to claude's default system prompt.
	SystemPrompt string
	// Prompt is the task given to the session.
	Prompt string
	// Env are extra environment variables.
	Env map[string]string
	// SessionDir is the per-session directory. When empty one is created;
	// callers that want to reference it in prompts create it first with
	// Runner.NewSessionDir.
	SessionDir string
}

// Result is what a finished session produced.
type Result struct {
	Name         string        `json:"name"`
	Role         string        `json:"role"`
	SessionDir   string        `json:"session_dir"`
	Transcript   string        `json:"transcript"`
	StartedAt    time.Time     `json:"started_at"`
	Duration     time.Duration `json:"duration"`
	ExitCode     int           `json:"exit_code"`
	ClaudeID     string        `json:"claude_session_id,omitempty"`
	ResultText   string        `json:"result_text,omitempty"`
	IsError      bool          `json:"is_error"`
	ErrorSubtype string        `json:"error_subtype,omitempty"`
	NumTurns     int           `json:"num_turns"`
	CostUSD      float64       `json:"cost_usd"`
	TimedOut     bool          `json:"timed_out"`
	Outcome      Outcome       `json:"outcome"`
	HasOutcome   bool          `json:"has_outcome"`
}

// Runner executes sessions.
type Runner struct {
	// ClaudeBin is the claude executable. Default "claude".
	ClaudeBin string
	// BeesBin is the path of the bees executable, made available on PATH so
	// sessions can run `bees mail` and `bees done`.
	BeesBin string
	// SessionsDir is where per-session directories are created.
	SessionsDir string
	// StateDir, ConfigPath, Repo and Label are exported to sessions.
	StateDir   string
	ConfigPath string
	Repo       string
	Label      string
	// Skills prepares skill plugin dirs. Optional.
	Skills *skills.Manager
	// AddDirs are extra directories claude may access (the state dir).
	AddDirs []string
	// Stream, when set, receives every stream-json line (debug output).
	Stream io.Writer
	Logger *slog.Logger
}

// Run executes the session and returns its result. An error is returned
// only when the session could not be started or produced no usable result;
// a session that ran but reported failure returns a Result with IsError set.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	claudeBin := r.ClaudeBin
	if claudeBin == "" {
		claudeBin = "claude"
	}
	started := time.Now()
	sessionDir := req.SessionDir
	if sessionDir == "" {
		var err error
		sessionDir, err = r.NewSessionDir(req.Name)
		if err != nil {
			return nil, err
		}
	}
	res := &Result{Name: req.Name, Role: req.Role.Name, SessionDir: sessionDir, StartedAt: started}

	systemPromptPath := filepath.Join(sessionDir, "system-prompt.md")
	if err := os.WriteFile(systemPromptPath, []byte(req.SystemPrompt), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(sessionDir, "prompt.md"), []byte(req.Prompt), 0o644); err != nil {
		return nil, err
	}

	args := []string{
		"-p",
		"--output-format", "stream-json",
		"--verbose",
		"--dangerously-skip-permissions",
		"--append-system-prompt-file", systemPromptPath,
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
	mcpPath := filepath.Join(sessionDir, "mcp.json")
	entries := MCPEntries(req.Role.MCP)
	entries[config.BuiltinMCPServer] = r.builtinMCP(req, sessionDir)
	if err := WriteMCPConfig(mcpPath, entries); err != nil {
		return nil, err
	}
	args = append(args, "--mcp-config", mcpPath, "--strict-mcp-config")
	if len(req.Role.Skills) > 0 {
		if r.Skills == nil {
			return nil, errors.New("session: skills configured but no skills manager")
		}
		dirs, err := r.Skills.Prepare(ctx, req.Role.Skills)
		if err != nil {
			return nil, err
		}
		for _, d := range dirs {
			args = append(args, "--plugin-dir", d)
		}
	}

	timeout := req.Role.Timeout
	if timeout <= 0 {
		timeout = config.DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, claudeBin, args...)
	cmd.Dir = req.WorkDir
	cmd.Stdin = strings.NewReader(req.Prompt)
	cmd.Env = r.env(req, sessionDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the whole process group so MCP servers die with claude.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 10 * time.Second

	transcriptPath := filepath.Join(sessionDir, "transcript.jsonl")
	transcript, err := os.Create(transcriptPath)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transcript.Close() }()
	res.Transcript = transcriptPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	r.Logger.Info("session start", "session", req.Name, "role", req.Role.Name, "model", req.Role.Model, "dir", req.WorkDir)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start claude: %w", err)
	}
	// Record the pid so `bees kill` can find the session after a crash.
	if err := procs.WritePID(sessionDir, cmd.Process.Pid); err != nil {
		r.Logger.Warn("write pid file", "session", req.Name, "err", err)
	}
	defer procs.RemovePID(sessionDir)

	final, scanErr := r.consume(stdout, transcript)
	waitErr := cmd.Wait()
	res.Duration = time.Since(started)
	if scanErr != nil {
		r.Logger.Warn("session output error", "session", req.Name, "err", scanErr)
	}

	if final != nil {
		res.ClaudeID = final.SessionID
		res.ResultText = final.Result
		res.IsError = final.IsError
		res.NumTurns = final.NumTurns
		res.CostUSD = final.TotalCostUSD
		if final.Subtype != "success" {
			res.ErrorSubtype = final.Subtype
			res.IsError = true
		}
	}
	var exitErr *exec.ExitError
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
		res.IsError = true
		res.ExitCode = -1
		res.ErrorSubtype = "timeout"
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		res.IsError = true
		if res.ErrorSubtype == "" {
			res.ErrorSubtype = "exit_" + strconv.Itoa(res.ExitCode)
		}
	case waitErr != nil:
		return nil, fmt.Errorf("claude: %w", waitErr)
	}
	if stderr.Len() > 0 {
		_ = os.WriteFile(filepath.Join(sessionDir, "stderr.log"), stderr.Bytes(), 0o644)
	}
	if final == nil && !res.TimedOut {
		res.IsError = true
		if res.ErrorSubtype == "" {
			res.ErrorSubtype = "no_result"
		}
		if res.ResultText == "" {
			res.ResultText = strings.TrimSpace(stderr.String())
		}
	}

	o, ok, err := ReadOutcome(sessionDir)
	if err != nil {
		r.Logger.Warn("outcome unreadable", "session", req.Name, "err", err)
	}
	res.Outcome, res.HasOutcome = o, ok

	if data, err := json.MarshalIndent(res, "", "  "); err == nil {
		_ = os.WriteFile(filepath.Join(sessionDir, "result.json"), data, 0o644)
	}
	r.Logger.Info("session end", "session", req.Name, "turns", res.NumTurns, "cost_usd", res.CostUSD,
		"duration", res.Duration.Round(time.Second), "error", res.IsError, "subtype", res.ErrorSubtype,
		"outcome", res.Outcome.Status)
	return res, nil
}

// beesEnv returns the BEES_* variables that describe the session, in a stable
// order. They go both into claude's environment and, explicitly, into the
// built-in MCP server's entry in mcp.json.
func (r *Runner) beesEnv(req Request, sessionDir string) []envVar {
	vars := []envVar{
		{EnvRole, req.Role.Name},
		{EnvSessionDir, sessionDir},
		{EnvStateDir, r.StateDir},
		{EnvConfig, r.ConfigPath},
		{EnvRepo, r.Repo},
		{EnvLabel, r.Label},
	}
	if r.BeesBin != "" {
		vars = append(vars, envVar{EnvBin, r.BeesBin})
	}
	// The per-request variables (issue, PR, branch, notes file) are set by
	// the scheduler; only the BEES_* ones describe the session.
	for _, k := range slices.Sorted(maps.Keys(req.Env)) {
		if strings.HasPrefix(k, beesEnvPrefix) {
			vars = append(vars, envVar{k, req.Env[k]})
		}
	}
	return vars
}

// envVar is one name/value pair.
type envVar struct{ name, value string }

// builtinMCP describes the built-in bees MCP server: this very binary, run as
// `bees mcp serve`, with the session's context passed explicitly.
func (r *Runner) builtinMCP(req Request, sessionDir string) MCPEntry {
	bin := r.BeesBin
	if bin == "" {
		if self, err := os.Executable(); err == nil {
			bin = self
		} else {
			bin = "bees"
		}
	}
	env := map[string]string{}
	for _, v := range r.beesEnv(req, sessionDir) {
		if v.value != "" {
			env[v.name] = v.value
		}
	}
	return MCPEntry{Type: "stdio", Command: bin, Args: []string{"mcp", "serve"}, Env: env}
}

func (r *Runner) env(req Request, sessionDir string) []string {
	// Every BEES_* variable a session sees is one bees set for it. Dropping the
	// inherited ones keeps a session started from inside another session (a
	// nested `bees run`, `bees exec`, or a test binary) from picking up a stale
	// issue, PR or branch number; the ones this session has none of are then
	// absent rather than wrong.
	envs := os.Environ()
	env := make([]string, 0, len(envs))
	for _, kv := range envs {
		if !strings.HasPrefix(kv, beesEnvPrefix) {
			env = append(env, kv)
		}
	}
	set := func(k, v string) { env = append(env, k+"="+v) }
	// Configured environment first, so bees' own variables below win.
	for k, v := range req.Role.Env {
		set(k, os.ExpandEnv(v))
	}
	if req.Role.Shell != "" {
		set("SHELL", req.Role.Shell)
	}
	for _, v := range r.beesEnv(req, sessionDir) {
		set(v.name, v.value)
	}
	if r.BeesBin != "" {
		set("PATH", filepath.Dir(r.BeesBin)+string(os.PathListSeparator)+os.Getenv("PATH"))
	}
	// Let sessions run a plain `git push` on a fresh branch without touching
	// the user's git configuration (git >= 2.31 reads GIT_CONFIG_* vars).
	if os.Getenv("GIT_CONFIG_COUNT") == "" {
		set("GIT_CONFIG_COUNT", "2")
		set("GIT_CONFIG_KEY_0", "push.autoSetupRemote")
		set("GIT_CONFIG_VALUE_0", "true")
		set("GIT_CONFIG_KEY_1", "push.default")
		set("GIT_CONFIG_VALUE_1", "current")
	}
	for k, v := range req.Env {
		set(k, v)
	}
	return env
}

// NewSessionDir creates a fresh per-session directory under SessionsDir.
func (r *Runner) NewSessionDir(name string) (string, error) {
	if err := os.MkdirAll(r.SessionsDir, 0o755); err != nil {
		return "", err
	}
	prefix := time.Now().Format("20060102-150405") + "-" + sanitize(name) + "-"
	return os.MkdirTemp(r.SessionsDir, prefix)
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

// consume copies stream-json lines to the transcript and returns the final
// result event.
func (r *Runner) consume(stdout io.Reader, transcript io.Writer) (*streamResult, error) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	var final *streamResult
	for sc.Scan() {
		line := sc.Bytes()
		_, _ = transcript.Write(line)
		_, _ = transcript.Write([]byte{'\n'})
		if r.Stream != nil {
			_, _ = r.Stream.Write(line)
			_, _ = r.Stream.Write([]byte{'\n'})
		}
		var probe struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(line, &probe); err != nil || probe.Type != "result" {
			continue
		}
		var sr streamResult
		if err := json.Unmarshal(line, &sr); err == nil {
			final = &sr
		}
	}
	return final, sc.Err()
}

// MCPEntry is one server in a claude --mcp-config file.
type MCPEntry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
}

// MCPEntries converts configured servers into file entries, expanding $VAR
// references in their environment and headers. `bees doctor` builds a role's
// entries with it so what it probes is what a session would actually start.
func MCPEntries(servers map[string]config.MCPServer) map[string]MCPEntry {
	out := make(map[string]MCPEntry, len(servers)+1)
	for name, s := range servers {
		typ := s.Type
		if typ == "" && s.Command == "" && s.URL != "" {
			typ = "http"
		}
		env := map[string]string{}
		for k, v := range s.Env {
			env[k] = os.ExpandEnv(v)
		}
		headers := map[string]string{}
		for k, v := range s.Headers {
			headers[k] = os.ExpandEnv(v)
		}
		out[name] = MCPEntry{Type: typ, Command: s.Command, Args: s.Args, Env: env, URL: s.URL, Headers: headers}
	}
	return out
}

// WriteMCPConfig writes the file claude is given as --mcp-config.
func WriteMCPConfig(path string, entries map[string]MCPEntry) error {
	out := struct {
		MCPServers map[string]MCPEntry `json:"mcpServers"`
	}{MCPServers: entries}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	return b.String()
}
