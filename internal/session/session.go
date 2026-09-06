// Package session runs one headless agent session for a role.
//
// A session is one non-interactive run of the role's agent — `claude -p`,
// or `codex exec` when the role's agent setting says so (see backend.go) —
// executed inside a workspace with the role's resolved settings: model and
// fallback model, appended system prompt, skills (as plugin dirs), MCP
// servers and tool restrictions. Every session also gets the built-in bees
// MCP server (see internal/mcpserver). The session communicates back through
// an outcome file — written by the `done` tool or `bees done`, both through
// Report — and through the local mailbox.
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

// EnvGHToken is the variable gh reads its credentials from. It is not one of
// bees' own BEES_* variables — it is gh's, set for a session only when
// [github] configures a token — so it survives the BEES_ strip in env.
const EnvGHToken = "GH_TOKEN"

// Request describes one session to run.
type Request struct {
	// Name identifies the session in logs, e.g. "developer-issue-12-r1".
	Name string
	Role config.ResolvedRole
	// WorkDir is the directory the agent runs in (the worktree).
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
	Name       string        `json:"name"`
	Role       string        `json:"role"`
	SessionDir string        `json:"session_dir"`
	Transcript string        `json:"transcript"`
	StartedAt  time.Time     `json:"started_at"`
	Duration   time.Duration `json:"duration"`
	ExitCode   int           `json:"exit_code"`
	// Signal is the signal that terminated the agent, or 0 when it exited
	// of its own accord. Go reports an ExitCode of -1 for a signalled
	// process and the signal is the only part that says why, so both are
	// recorded: the number here, its name in ErrorSubtype ("signal_killed").
	Signal int `json:"signal,omitempty"`
	// ClaudeID is the id the agent gave the session: claude's session id,
	// or codex's thread id. The JSON name is kept for the readers of
	// result.json that predate codex.
	ClaudeID     string  `json:"claude_session_id,omitempty"`
	ResultText   string  `json:"result_text,omitempty"`
	IsError      bool    `json:"is_error"`
	ErrorSubtype string  `json:"error_subtype,omitempty"`
	NumTurns     int     `json:"num_turns"`
	CostUSD      float64 `json:"cost_usd"`
	// CostKnown says whether CostUSD is what the session cost or merely
	// what is known about it: claude reports the cost in the result event
	// of its stream alone, so a session killed before it emitted one has
	// no cost at all rather than a cost of zero, and codex reports tokens
	// but never a cost. Nothing derives one.
	CostKnown  bool    `json:"cost_known"`
	TimedOut   bool    `json:"timed_out"`
	Outcome    Outcome `json:"outcome"`
	HasOutcome bool    `json:"has_outcome"`
	// RateLimit is the last rate-limit event of the session's stream, or
	// nil when it carried none.
	RateLimit *RateLimit `json:"rate_limit,omitempty"`
}

// RateLimit is what claude last said about the account's capacity: the
// rate_limit_info of the final "rate_limit_event" of a session's stream.
type RateLimit struct {
	// Status is rate_limit_info.status. Only "allowed" and
	// "allowed_warning" are known to mean the session may keep going;
	// anything else is treated as blocking, because the blocked value has
	// never been observed here and must not be guessed at.
	Status string `json:"status,omitempty"`
	// Type is rate_limit_info.rateLimitType ("five_hour", "seven_day").
	Type string `json:"type,omitempty"`
	// ResetsAt is when the window rolls, from rate_limit_info.resetsAt.
	// Zero when the event carried none.
	ResetsAt time.Time `json:"resets_at,omitempty"`
}

// blocking reports whether the event says the session may not proceed. An
// event with no status at all is a parse artifact rather than a signal and
// never blocks; the result text is the second trigger that covers it.
func (rl *RateLimit) blocking() bool {
	return rl != nil && rl.Status != "" && rl.Status != "allowed" && rl.Status != "allowed_warning"
}

// sessionLimitPhrases mark the message a session reports when the account
// itself is out of capacity ("You've hit your session limit · resets
// 11:50pm (America/Detroit)"). They are deliberately narrower than the
// scheduler's rate-limit phrases: a throttled or overloaded API is worth
// retrying, an exhausted account is not.
var sessionLimitPhrases = []string{"session limit", "usage limit"}

// SessionLimited answers the only question the scheduler asks of a finished
// session's capacity report: did it die on the account-wide claude limit,
// and when does that limit reset? It says yes when the last rate-limit
// event was blocking, or when a session that failed without reporting an
// outcome has a result text naming a session or usage limit. The reset time
// is the one the last event carried and is zero when there was none — the
// human-readable sentence is never scraped for it.
func (r *Result) SessionLimited() (time.Time, bool) {
	var resets time.Time
	if r.RateLimit != nil {
		resets = r.RateLimit.ResetsAt
	}
	if r.RateLimit.blocking() {
		return resets, true
	}
	// The result text is the session's own prose. It names the limit only
	// when the session had nothing else to report, so a session that ran
	// and reported an outcome is never read this way: a bee whose work is
	// the session limit must not pause the factory by writing about it.
	if r.HasOutcome || !r.IsError {
		return time.Time{}, false
	}
	msg := strings.ToLower(r.ResultText)
	for _, p := range sessionLimitPhrases {
		if strings.Contains(msg, p) {
			return resets, true
		}
	}
	return time.Time{}, false
}

// Runner executes sessions.
type Runner struct {
	// ClaudeBin is the claude executable. Default "claude".
	ClaudeBin string
	// CodexBin is the codex executable, run for a role whose agent is
	// codex. Default "codex".
	CodexBin string
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
	// GitHub is the identity a session acts as: the token its `gh` calls and
	// its pushes use, and the name and email its commits carry. The zero
	// value means the machine's own gh authentication and git identity,
	// which is what every configuration without [github] gets.
	GitHub config.GitHub
	// Skills prepares skill plugin dirs. Optional.
	Skills *skills.Manager
	// AddDirs are extra directories claude may access (the state dir).
	// Codex, which runs without a sandbox, needs no such list.
	AddDirs []string
	// Stream, when set, receives every stream-json line (debug output).
	Stream io.Writer
	Logger *slog.Logger
}

// Run executes the session and returns its result. An error is returned
// only when the session could not be started, produced no usable result, or
// was stopped by its context being cancelled — that one also leaves no
// result file, so the session directory reads as an interrupted session;
// a session that ran but reported failure returns a Result with IsError set.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	// The box this session runs in is the role's resolved sandbox mode.
	// Only config.SandboxNone is implemented, and it is what the command
	// built below already is; a role configured for a stronger box refuses
	// to run rather than quietly running without one. `bees run` asks the
	// same question once at startup (config.CheckSandbox), so reaching this
	// means a session started some other way — `bees exec`, `bees tick`.
	if err := config.CheckSandboxMode(req.Role.Sandbox); err != nil {
		return nil, fmt.Errorf("%s: %w", req.Role.Name, err)
	}
	be, err := backendFor(req.Role.Agent)
	if err != nil {
		return nil, err
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
	promptPath := filepath.Join(sessionDir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte(req.Prompt), 0o644); err != nil {
		return nil, err
	}

	paths := sessionPaths{dir: sessionDir, systemPrompt: systemPromptPath, prompt: promptPath}
	bin, args, stdin, err := be.command(ctx, r, req, paths)
	if err != nil {
		return nil, err
	}

	timeout := req.Role.Timeout
	if timeout <= 0 {
		timeout = config.DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = req.WorkDir
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = r.env(req, sessionDir)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the whole process group so MCP servers die with the agent.
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 10 * time.Second

	transcriptPath := filepath.Join(sessionDir, TranscriptFile)
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

	r.Logger.Info("session start", "session", req.Name, "role", req.Role.Name, "agent", req.Role.Agent, "model", req.Role.Model, "dir", req.WorkDir)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", filepath.Base(bin), err)
	}
	// Record the pid so `bees kill` can find the session after a crash.
	if err := procs.WritePID(sessionDir, cmd.Process.Pid); err != nil {
		r.Logger.Warn("write pid file", "session", req.Name, "err", err)
	}
	defer procs.RemovePID(sessionDir)

	final, limit, scanErr := be.consume(r, stdout, transcript)
	waitErr := cmd.Wait()
	res.Duration = time.Since(started)
	if scanErr != nil {
		r.Logger.Warn("session output error", "session", req.Name, "err", scanErr)
	}

	res.RateLimit = limit
	if final != nil {
		res.ClaudeID = final.SessionID
		res.ResultText = final.Result
		res.IsError = final.IsError
		res.NumTurns = final.NumTurns
		res.CostUSD = final.CostUSD
		res.CostKnown = final.CostKnown
		if final.Subtype != "success" {
			res.ErrorSubtype = final.Subtype
			res.IsError = true
		}
	} else {
		// No result event: the agent never reported how far it had got, so
		// the turns it wrote to the transcript are counted instead. A
		// session that died after four minutes of work reported zero turns
		// otherwise, which reads as a session that did nothing.
		res.NumTurns = CountTurns(transcriptPath)
	}
	var exitErr *exec.ExitError
	switch {
	case runCtx.Err() == context.DeadlineExceeded:
		res.TimedOut = true
		res.IsError = true
		res.ExitCode = -1
		res.ErrorSubtype = "timeout"
	case runCtx.Err() == context.Canceled && waitErr != nil:
		// The caller cancelled the session — Scheduler.HardStop, or an
		// interrupt around a run outside the loop — and the process group
		// was killed before it finished. No result file is written: its
		// absence is what says a session never finished, and is what lets
		// CheckInterrupted report the directory as interrupted so the next
		// run resumes the work through the ordinary crash-recovery path. A
		// process that had already exited cleanly (waitErr nil) finished
		// its work whatever the context says, and is reported as usual.
		r.Logger.Warn("session stopped", "session", req.Name, "role", req.Role.Name)
		return nil, fmt.Errorf("session stopped: %w", context.Cause(runCtx))
	case errors.As(waitErr, &exitErr):
		res.ExitCode = exitErr.ExitCode()
		res.IsError = true
		sig, signalled := terminatingSignal(exitErr.ProcessState)
		if signalled {
			res.Signal = int(sig)
		}
		if res.ErrorSubtype == "" {
			if signalled {
				res.ErrorSubtype = "signal_" + signalName(sig)
			} else {
				res.ErrorSubtype = "exit_" + strconv.Itoa(res.ExitCode)
			}
		}
	case waitErr != nil:
		return nil, fmt.Errorf("%s: %w", filepath.Base(bin), waitErr)
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
		_ = os.WriteFile(filepath.Join(sessionDir, ResultFile), data, 0o644)
	}
	r.Logger.Info("session end", "session", req.Name, "turns", res.NumTurns, "cost_usd", res.CostUSD,
		"duration", res.Duration.Round(time.Second), "error", res.IsError, "subtype", res.ErrorSubtype,
		"outcome", res.Outcome.Status)
	return res, nil
}

// terminatingSignal reports the signal that killed a process, if one did.
// Go reports an ExitCode of -1 for a signalled process, which says nothing
// about why it died: SIGKILL from the out-of-memory killer and SIGHUP from
// a closing terminal read very differently. The wait status carries the
// signal, and bees is built for darwin and linux, where it is always a
// syscall.WaitStatus.
func terminatingSignal(st *os.ProcessState) (syscall.Signal, bool) {
	ws, ok := st.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return 0, false
	}
	return ws.Signal(), true
}

// signalName renders a signal for an error subtype: the name the operating
// system gives it, in the snake case the other subtypes use ("killed",
// "hangup", "broken pipe" -> "signal_killed", "signal_hangup",
// "signal_broken_pipe").
func signalName(sig syscall.Signal) string {
	return strings.ReplaceAll(sig.String(), " ", "_")
}

// beesEnv returns the BEES_* variables that describe the session, in a stable
// order. They go both into claude's environment and, explicitly, into the
// built-in MCP server's entry in mcp.json — which is written to the session
// directory, so nothing secret may join them. The variable github.token reads
// is set in env alone for that reason.
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
	// absent rather than wrong. The one exception is the variable
	// github.token reads, put back below with the value bees resolved.
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
	// The factory's own GitHub identity, so a session's gh, pushes and
	// commits are the bot's rather than the machine owner's. It sits with
	// bees' own variables, after req.Role.Env, so a role cannot configure a
	// second identity for itself.
	if token := r.GitHub.ResolvedToken(); token != "" {
		set(EnvGHToken, token)
		// github.token may be a $VAR reference, and when the operator named a
		// BEES_* variable the strip above drops it — leaving a session whose
		// gh works but whose own `bees` commands cannot load bees.toml at all,
		// because a reference that expands to nothing is a load error. Put the
		// name back, holding the value the scheduler resolved, so no session
		// can be handed a token from a stale environment. It is deliberately
		// not one of beesEnv's variables: those are written into mcp.json in
		// the session directory, and the secret must not reach disk. claude
		// passes its own environment on to the MCP server it starts, so the
		// built-in one is served by this. Codex does not (it starts a server
		// with a fixed handful of variables plus the entry's env), so under
		// codex the built-in server's gh runs without the factory's token.
		if v := r.GitHub.TokenVar(); v != "" {
			set(v, token)
		}
	}
	for _, v := range r.gitIdentity() {
		set(v.name, v.value)
	}
	// Let sessions run a plain `git push` on a fresh branch without touching
	// the user's git configuration (git >= 2.31 reads GIT_CONFIG_* vars).
	// GIT_CONFIG_COUNT is derived from the entries and never written by hand:
	// a count that is one short silently drops the last entry.
	if os.Getenv("GIT_CONFIG_COUNT") == "" {
		entries := r.gitConfig()
		for i, e := range entries {
			n := strconv.Itoa(i)
			set("GIT_CONFIG_KEY_"+n, e.name)
			set("GIT_CONFIG_VALUE_"+n, e.value)
		}
		set("GIT_CONFIG_COUNT", strconv.Itoa(len(entries)))
	}
	for k, v := range req.Env {
		set(k, v)
	}
	return env
}

// gitIdentity is the author and committer a session's commits carry. The two
// keys are independent: config accepts git_name or git_email on its own, and
// whichever is unset stays the machine's, because a half-set identity is
// still better than a wrong one.
func (r *Runner) gitIdentity() []envVar {
	var vars []envVar
	if n := r.GitHub.GitName; n != "" {
		vars = append(vars, envVar{"GIT_AUTHOR_NAME", n}, envVar{"GIT_COMMITTER_NAME", n})
	}
	if e := r.GitHub.GitEmail; e != "" {
		vars = append(vars, envVar{"GIT_AUTHOR_EMAIL", e}, envVar{"GIT_COMMITTER_EMAIL", e})
	}
	return vars
}

// gitConfig is the git configuration a session runs with, as the key/value
// pairs env numbers into GIT_CONFIG_KEY_n and GIT_CONFIG_VALUE_n.
func (r *Runner) gitConfig() []envVar {
	entries := []envVar{
		{"push.autoSetupRemote", "true"},
		{"push.default", "current"},
	}
	if r.GitHub.ResolvedToken() != "" {
		// Push over https as the bot, without reading or writing the
		// person's stored credentials. The empty value is load-bearing:
		// git asks credential helpers in configuration order and takes the
		// first answer, and GIT_CONFIG_* entries are read last, so without
		// it the machine owner's helper (a keychain, or their own gh)
		// answers first and the push is theirs. An empty credential.helper
		// resets the list built so far.
		entries = append(entries,
			envVar{"credential.helper", ""},
			envVar{"credential.helper", "!gh auth git-credential"},
		)
	}
	return entries
}

// NewSessionDir creates a fresh per-session directory under SessionsDir.
func (r *Runner) NewSessionDir(name string) (string, error) {
	if err := os.MkdirAll(r.SessionsDir, 0o755); err != nil {
		return "", err
	}
	prefix := time.Now().Format("20060102-150405") + "-" + sanitize(name) + "-"
	return os.MkdirTemp(r.SessionsDir, prefix)
}

// tee copies every line of a session's stdout to the transcript (and to
// r.Stream when set), handing each one that is a JSON object with a "type"
// to visit along with that type. It is the read loop both backends share:
// what differs between them is what the lines mean, which is visit's
// business. A line the scanner cannot hold is the end of the stream.
func (r *Runner) tee(stdout io.Reader, transcript io.Writer, visit func(line []byte, typ string)) error {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
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
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		visit(line, probe.Type)
	}
	return sc.Err()
}

// MCPEntry is one MCP server as a session is given it: an entry of claude's
// --mcp-config file, or the source of codex's mcp_servers overrides.
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
