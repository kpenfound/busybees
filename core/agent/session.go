// Package agent runs one headless coding-agent session. Callers supply execution
// profiles, environment, MCP servers and outcome policy.
package agent

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

	"github.com/kpenfound/busybees/core/agent/agentbin"
	"github.com/kpenfound/busybees/core/agent/procs"
	"github.com/kpenfound/busybees/core/vcs"
)

// Request describes one session to run.
type Request struct {
	// Name identifies the session in logs, e.g. "builder-task-12-r1".
	Name    string
	Profile Profile
	// ValidOutcomes is nil to accept any status; an empty non-nil set accepts none.
	ValidOutcomes []string
	// HostMCP optionally starts a caller-owned stdio server on the host for container sessions.
	HostMCP *HostMCP
	// ContainerEnv overrides Env only inside the container.
	ContainerEnv map[string]string
	// Workspace supplies the working directory and optional VCS resources.
	Workspace vcs.Workspace
	// VCSEnv and VCSContainerEnv are caller-owned identity, credentials and
	// configuration, injected only when the profile allows VCS access.
	VCSEnv          map[string]string
	VCSContainerEnv map[string]string
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
	// Grants are the session's complete capabilities. Run verifies the
	// request against them before it starts anything and refuses a request
	// without them.
	Grants *Grants
	// ResumeID, when set, is the agent's own id of an earlier session
	// (Result.ClaudeID) whose conversation this one continues, so a later
	// round of the same role starts with the previous round's context
	// instead of relearning the codebase. Claude and opencode can; codex
	// has no resume, and ignores it. The caller owns the id's lifetime; one that
	// the agent no longer knows makes the launch fail before it says
	// anything, which the caller's retry runs fresh.
	ResumeID string
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
	// OpenCode's session id, or codex's thread id. The JSON name is kept for
	// readers of result.json that predate codex.
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
	// and reported an outcome is never read this way: an agent whose work is
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
	// OpenCodeBin is the opencode executable, run for a role whose agent
	// is opencode. Default "opencode".
	OpenCodeBin string
	// DockerBin is the container engine a container session is run with.
	// Default ContainerEngine.
	DockerBin string
	// ContainerListen is the address the caller-supplied MCP server listens on
	// for a container session. Empty picks the address the container
	// reaches the host by (see containerListen).
	ContainerListen string
	SessionsDir     string
	// EnvironmentPrefix is removed from the inherited host environment.
	EnvironmentPrefix string
	// NamePrefix prefixes backend titles and container names.
	NamePrefix string
	// ContainerLabel identifies containers during orphan inspection.
	ContainerLabel         string
	ContainerHome          string
	ContainerUseRepository string
	// MountDirs are directories a container session writes; each must be
	// granted read-write.
	MountDirs []string
	// Skills prepares generic skill plugin directories.
	Skills SkillPreparer
	// SkillMountDirs hold the prepared skills; a container session or a
	// confined host session with skills must be granted them.
	SkillMountDirs []string
	// AddDirs are extra directories claude may write (the state dir). Each
	// must be granted read-write.
	// Codex, which runs without a sandbox, needs no such list, and neither
	// does opencode, whose --auto approves writing outside the worktree.
	AddDirs []string
	// Confiner enforces a confined host session (Profile.Confine); nil
	// selects this platform's. SystemPaths are what such a session reaches
	// beyond its mounts; nil selects DefaultSystemPaths.
	Confiner    Confiner
	SystemPaths []Mount
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
	runner := *r
	r = &runner // per-run defaults must not mutate a shared runner
	if r.Logger == nil {
		r.Logger = slog.Default()
	}
	if err := req.Profile.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", req.Profile.Name, err)
	}
	be, err := backendFor(req.Profile.Agent)
	if err != nil {
		return nil, err
	}
	// Grants are verified before anything is written or started: a request
	// that asks for more than it was granted never runs.
	turn, err := r.Verify(req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.Profile.Name, err)
	}
	started := time.Now()
	sessionDir := req.SessionDir
	if sessionDir == "" {
		var err error
		sessionDir, err = r.NewSessionDir(req.Name)
		if err != nil {
			return nil, err
		}
		// Verified again now that the directory the container is given exists.
		req.SessionDir = sessionDir
		if turn, err = r.Verify(req); err != nil {
			_ = os.RemoveAll(sessionDir)
			return nil, fmt.Errorf("%s: %w", req.Profile.Name, err)
		}
	}
	res := &Result{Name: req.Name, Role: req.Profile.Name, SessionDir: sessionDir, StartedAt: started}

	systemPromptPath := filepath.Join(sessionDir, "system-prompt.md")
	if err := os.WriteFile(systemPromptPath, []byte(req.SystemPrompt), 0o644); err != nil {
		return nil, err
	}
	promptPath := filepath.Join(sessionDir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte(req.Prompt), 0o644); err != nil {
		return nil, err
	}

	paths := sessionPaths{dir: sessionDir, systemPrompt: systemPromptPath, prompt: promptPath}
	var box *container
	if req.Profile.Sandbox == SandboxContainer {
		box, err = r.startContainer(ctx, req, sessionDir, turn)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.Profile.Name, err)
		}
		defer box.close()
		paths.mcp = maps.Clone(req.Profile.MCP)
		if req.HostMCP != nil {
			if paths.mcp == nil {
				paths.mcp = map[string]MCPEntry{}
			}
			paths.mcp[req.HostMCP.Name] = box.builtin
		}
	} else {
		paths.mcp = maps.Clone(req.Profile.MCP)
	}
	paths.turn = turn
	bin, args, stdin, extra, err := be.command(ctx, r, req, paths)
	if err != nil {
		return nil, err
	}
	env := turn.Env
	if box == nil {
		if env, err = denyExecutables(filepath.Join(sessionDir, deniedBinDir), turn.DeniedExecutables, env); err != nil {
			return nil, err
		}
	}
	for _, v := range extra {
		env = append(env, v.name+"="+v.value)
	}
	if box != nil {
		// A fake engine runs the backend on the host. Check that it cannot
		// launch a real agent in tests, while leaving the executable name
		// unchanged for resolution inside a production container.
		if _, err := agentbin.Resolve(bin); err != nil {
			return nil, err
		}
		// The backend's variables reach the container the way the
		// session's own do: by name on the engine's command line, with the
		// value in the client's environment.
		box.vars = append(box.vars, extra...)
		bin, args, err = box.command(ctx, bin, args)
		if err != nil {
			return nil, err
		}
		env = box.clientEnv()
		if bin, err = agentbin.Resolve(bin); err != nil {
			return nil, err
		}
	} else {
		// The agent runs on this host: resolved here, where a test binary
		// is kept from running a real one (see agentbin).
		if bin, err = agentbin.Resolve(bin); err != nil {
			return nil, fmt.Errorf("%s: %w", req.Profile.Name, err)
		}
	}

	timeout := req.Profile.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, bin, args...)
	cmd.Dir = req.workDir()
	cmd.Stdin = strings.NewReader(stdin)
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		// Kill the whole process group so MCP servers die with the agent.
		// A container outlives its engine client, so it is removed first.
		if box != nil {
			box.remove()
		}
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

	// A confined turn is started by what confines it, and by nothing else:
	// a confiner that cannot enforce the turn leaves it unstarted.
	start := cmd.Start
	if box == nil && turn.Confinement != nil {
		confinement, err := turn.Confinement.withExecutable(bin)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", req.Profile.Name, err)
		}
		confiner := r.Confiner
		if confiner == nil {
			confiner = platformConfiner()
		}
		start = func() error { return confiner.Start(cmd, confinement) }
	}

	r.Logger.Info("session start", "session", req.Name, "role", req.Profile.Name, "agent", req.Profile.Agent, "model", req.Profile.Model, "sandbox", req.Profile.Sandbox, "confined", turn.Confinement != nil, "dir", req.workDir())
	if err := start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", filepath.Base(bin), err)
	}
	// Record the pid so orphan cleanup can find the session after a crash.
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
		// No closing event: the agent never reported how far it had got, so
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
		// The caller cancelled the session — a hard stop, or an
		// interrupt around a run outside the loop — and the process group
		// was killed before it finished. No result file is written: its
		// absence is what says a session never finished, and is what lets
		// CheckInterrupted report the directory as interrupted so the next
		// run resumes the work through the ordinary crash-recovery path. A
		// process that had already exited cleanly (waitErr nil) finished
		// its work whatever the context says, and is reported as usual.
		r.Logger.Warn("session stopped", "session", req.Name, "role", req.Profile.Name)
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
	if ok && ValidateOutcome(req.Profile.Name, o.Status, req.ValidOutcomes) != nil {
		ok = false
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
// signal, and the runner supports darwin and linux, where it is always a
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

// envVar is one name/value pair.
type envVar struct{ name, value string }

func (r *Runner) env(req Request, _ string) []string {
	env := hostEnv(r.EnvironmentPrefix)
	for _, v := range sessionVars(req, req.Profile.VCSAccess) {
		env = append(env, v.name+"="+v.value)
	}
	return env
}

func hostEnv(prefix string) []string {
	var env []string
	for _, kv := range os.Environ() {
		if prefix == "" || !strings.HasPrefix(kv, prefix) {
			env = append(env, kv)
		}
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

// tee copies every line of a session's stdout to the transcript (and to
// r.Stream when set), handing each one that is a JSON object with a "type"
// to visit along with that type. It is the read loop every backend shares:
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
// --mcp-config file, the source of codex's mcp_servers overrides, or a
// server of opencode's configuration file.
type MCPEntry struct {
	Type    string            `json:"type,omitempty"`
	Command string            `json:"command,omitempty"`
	Args    []string          `json:"args,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	URL     string            `json:"url,omitempty"`
	Headers map[string]string `json:"headers,omitempty"`
	// EnvVars names variables Codex must inherit from its own environment.
	// Claude and OpenCode inherit it already, so this is not a file entry.
	EnvVars []string `json:"-"`
	// BearerTokenEnv names the process variable holding an HTTP bearer token.
	// Backend writers render an environment reference, never the secret value.
	BearerTokenEnv string `json:"-"`
}

// MCPEntries converts configured servers into file entries, expanding $VAR
// references in their environment and headers. Call this once before handing
// prepared entries to the runner.
func MCPEntries(servers map[string]MCPEntry) map[string]MCPEntry {
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
		out[name] = MCPEntry{Type: typ, Command: s.Command, Args: s.Args, Env: env, URL: s.URL, Headers: headers, EnvVars: slices.Clone(s.EnvVars), BearerTokenEnv: s.BearerTokenEnv}
	}
	return out
}

// bearerHeaders preserves public headers and adds a backend-specific token reference.
func bearerHeaders(entry MCPEntry, reference string) map[string]string {
	headers := maps.Clone(entry.Headers)
	if headers == nil {
		headers = map[string]string{}
	}
	headers["Authorization"] = "Bearer " + reference
	return headers
}

// WriteMCPConfig writes the file claude is given as --mcp-config.
func WriteMCPConfig(path string, entries map[string]MCPEntry) error {
	out := struct {
		MCPServers map[string]MCPEntry `json:"mcpServers"`
	}{MCPServers: maps.Clone(entries)}
	for name, entry := range out.MCPServers {
		if entry.BearerTokenEnv != "" {
			entry.Headers = bearerHeaders(entry, "${"+entry.BearerTokenEnv+"}")
			out.MCPServers[name] = entry
		}
	}
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

func (r *Runner) namePrefix() string {
	if r.NamePrefix != "" {
		return r.NamePrefix
	}
	return "agent-"
}
func (r *Runner) containerLabel() string {
	if r.ContainerLabel != "" {
		return r.ContainerLabel
	}
	return procs.ContainerLabel
}

func (req Request) workDir() string {
	if req.Workspace == nil {
		return ""
	}
	return req.Workspace.Directory()
}
