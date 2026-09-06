package session

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// fakeClaude writes a shell script standing in for the claude binary.
func fakeClaude(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nset -e\n" + body
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func newRunner(t *testing.T, bin string) *Runner {
	t.Helper()
	return &Runner{ClaudeBin: bin, SessionsDir: t.TempDir(), StateDir: "/state", Repo: "a/b", Label: "bees", BeesBin: "/usr/local/bin/bees"}
}

func TestRunSuccess(t *testing.T) {
	bin := fakeClaude(t, `
# record what we were given
printf '%s' "$@" > "$BEES_SESSION_DIR/args.txt"
cat > "$BEES_SESSION_DIR/stdin.txt"
env > "$BEES_SESSION_DIR/env.txt"
echo '{"type":"system","subtype":"init"}'
echo '{"type":"result","subtype":"success","is_error":false,"result":"all done","session_id":"abc","num_turns":4,"total_cost_usd":0.25}'
printf '{"status":"pr-opened","pr":12,"note":"hi"}' > "$BEES_SESSION_DIR/outcome.json"
`)
	r := newRunner(t, bin)
	role := config.ResolvedRole{Name: "developer", Model: "opus", FallbackModel: "sonnet", MaxTurns: 10, Timeout: time.Minute,
		MCP:   map[string]config.MCPServer{"x": {Command: "srv", Env: map[string]string{"K": "$HOME"}}},
		Shell: "/bin/sh", Env: map[string]string{"FACTORY_TOKEN": "abc", "CACHE": "$HOME/cache"}}
	res, err := r.Run(context.Background(), Request{Name: "t1", Role: role, WorkDir: t.TempDir(), SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{"EXTRA": "1", EnvIssue: "12"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "all done" || res.NumTurns != 4 || res.CostUSD != 0.25 || res.ClaudeID != "abc" {
		t.Fatalf("result: %+v", res)
	}
	if !res.HasOutcome || res.Outcome.Status != "pr-opened" || res.Outcome.PR != 12 {
		t.Fatalf("outcome: %+v", res.Outcome)
	}
	args, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	for _, want := range []string{"-p", "--model", "opus", "--fallback-model", "sonnet", "--max-turns", "10", "--dangerously-skip-permissions", "--mcp-config", "--strict-mcp-config", "--append-system-prompt-file"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %s: %s", want, args)
		}
	}
	stdin, _ := os.ReadFile(filepath.Join(res.SessionDir, "stdin.txt"))
	if string(stdin) != "TASK" {
		t.Fatalf("stdin: %q", stdin)
	}
	env, _ := os.ReadFile(filepath.Join(res.SessionDir, "env.txt"))
	for _, want := range []string{"BEES_ROLE=developer", "BEES_STATE_DIR=/state", "BEES_REPO=a/b", "EXTRA=1", "BEES_BIN=/usr/local/bin/bees", "SHELL=/bin/sh", "FACTORY_TOKEN=abc", "CACHE=" + os.Getenv("HOME") + "/cache"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env missing %s", want)
		}
	}
	sys, _ := os.ReadFile(filepath.Join(res.SessionDir, "system-prompt.md"))
	if string(sys) != "SYS" {
		t.Fatalf("system prompt: %q", sys)
	}
	servers := readMCPConfig(t, res.SessionDir)
	if servers["x"].Command != "srv" || servers["x"].Env["K"] != os.Getenv("HOME") {
		t.Fatalf("configured server: %+v", servers["x"])
	}
	// The built-in server sits next to the configured one.
	builtin := servers[config.BuiltinMCPServer]
	if builtin.Command != "/usr/local/bin/bees" || strings.Join(builtin.Args, " ") != "mcp serve" {
		t.Fatalf("built-in server: %+v", builtin)
	}
	for k, want := range map[string]string{
		"BEES_ROLE": "developer", "BEES_STATE_DIR": "/state", "BEES_SESSION_DIR": res.SessionDir,
		"BEES_REPO": "a/b", "BEES_LABEL": "bees", "BEES_ISSUE": "12", "BEES_BIN": "/usr/local/bin/bees",
	} {
		if builtin.Env[k] != want {
			t.Errorf("built-in server env %s = %q, want %q", k, builtin.Env[k], want)
		}
	}
	if _, ok := builtin.Env["EXTRA"]; ok {
		t.Errorf("built-in server env leaked a non-BEES variable: %v", builtin.Env)
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "transcript.jsonl")); err != nil {
		t.Fatal("transcript missing")
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "result.json")); err != nil {
		t.Fatal("result.json missing")
	}
}

// A session started from inside another session must not inherit its BEES_*
// variables: only the ones the runner sets for it are visible.
func TestEnvDropsInheritedBeesVariables(t *testing.T) {
	bin := fakeClaude(t, `
env > "$BEES_SESSION_DIR/env.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute}
	run := func(t *testing.T, roleEnv, reqEnv map[string]string) []string {
		t.Helper()
		t.Setenv(EnvPR, "54")
		t.Setenv(EnvIssue, "99")
		t.Setenv(EnvStateDir, "/inherited")
		r := role
		r.Env = roleEnv
		res, err := newRunner(t, bin).Run(context.Background(), Request{Name: "t", Role: r, WorkDir: t.TempDir(), Env: reqEnv})
		if err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(filepath.Join(res.SessionDir, "env.txt"))
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	}
	has := func(lines []string, want string) bool { return slices.Contains(lines, want) }
	// lookup returns every line setting name, so a failure names the offender
	// instead of dumping the whole environment.
	lookup := func(lines []string, name string) []string {
		var got []string
		for _, l := range lines {
			if strings.HasPrefix(l, name+"=") {
				got = append(got, l)
			}
		}
		return got
	}

	t.Run("absent when the session has none", func(t *testing.T) {
		lines := run(t, nil, nil)
		for _, name := range []string{EnvPR, EnvIssue, EnvBranch} {
			if got := lookup(lines, name); got != nil {
				t.Errorf("%s leaked from the parent process: %v", name, got)
			}
		}
		// The runner's own variables are still there, with its values.
		if !has(lines, EnvStateDir+"=/state") || !has(lines, EnvRole+"=developer") {
			t.Errorf("runner variables missing: %v", lines)
		}
	})

	t.Run("the session's own value wins", func(t *testing.T) {
		lines := run(t, nil, map[string]string{EnvPR: "12"})
		if got := lookup(lines, EnvPR); len(got) != 1 || got[0] != EnvPR+"=12" {
			t.Errorf("%s lines = %v, want exactly [%s=12]", EnvPR, got, EnvPR)
		}
		if got := lookup(lines, EnvIssue); got != nil {
			t.Errorf("%s leaked from the parent process: %v", EnvIssue, got)
		}
	})

	// The strip is namespace-wide, so it also drops operator knobs like
	// BEES_CLAUDE_BIN that are not session state. Configured role env is the
	// documented way to give them to sessions: it is applied after the strip.
	t.Run("configured role env reaches the session", func(t *testing.T) {
		t.Setenv("BEES_CACHE_DIR", "/inherited-cache")
		lines := run(t, map[string]string{"BEES_CACHE_DIR": "/configured-cache"}, nil)
		if got := lookup(lines, "BEES_CACHE_DIR"); len(got) != 1 || got[0] != "BEES_CACHE_DIR=/configured-cache" {
			t.Errorf("BEES_CACHE_DIR lines = %v, want exactly [BEES_CACHE_DIR=/configured-cache]", got)
		}
	})
}

func TestRunErrorAndNoOutcome(t *testing.T) {
	bin := fakeClaude(t, `
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"ran out","num_turns":10}'
exit 1
`)
	r := newRunner(t, bin)
	res, err := r.Run(context.Background(), Request{Name: "t2", Role: config.ResolvedRole{Name: "qa", Model: "opus", MaxTurns: 10}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.ErrorSubtype != "error_max_turns" || res.ExitCode != 1 || res.HasOutcome {
		t.Fatalf("result: %+v", res)
	}
}

func TestRunTimeout(t *testing.T) {
	bin := fakeClaude(t, `sleep 5`)
	r := newRunner(t, bin)
	res, err := r.Run(context.Background(), Request{Name: "t3", Role: config.ResolvedRole{Name: "qa", Model: "opus", MaxTurns: 1, Timeout: 200 * time.Millisecond}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if !res.TimedOut || !res.IsError || res.ErrorSubtype != "timeout" {
		t.Fatalf("result: %+v", res)
	}
}

func TestOutcomeRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if _, ok, err := ReadOutcome(dir); ok || err != nil {
		t.Fatal("expected no outcome")
	}
	if err := WriteOutcome(dir, Outcome{Status: "approved", Note: "n"}); err != nil {
		t.Fatal(err)
	}
	o, ok, err := ReadOutcome(dir)
	if err != nil || !ok || o.Status != "approved" {
		t.Fatalf("%+v %v %v", o, ok, err)
	}
}

// readMCPConfig parses the mcp.json a session was given.
func readMCPConfig(t *testing.T, sessionDir string) map[string]MCPEntry {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sessionDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		MCPServers map[string]MCPEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(b, &file); err != nil {
		t.Fatalf("mcp.json: %v: %s", err, b)
	}
	return file.MCPServers
}

// TestRunAlwaysWritesMCPConfig covers a role with no configured servers: it
// still gets the built-in one, so --mcp-config is unconditional.
func TestRunAlwaysWritesMCPConfig(t *testing.T) {
	bin := fakeClaude(t, `
printf '%s' "$@" > "$BEES_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	role := config.ResolvedRole{Name: "reviewer", Model: "opus", MaxTurns: 10, Timeout: time.Minute}
	res, err := r.Run(context.Background(), Request{Name: "t5", Role: role, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	args, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	for _, want := range []string{"--mcp-config", "--strict-mcp-config"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args missing %s: %s", want, args)
		}
	}
	servers := readMCPConfig(t, res.SessionDir)
	if len(servers) != 1 {
		t.Fatalf("servers: %+v", servers)
	}
	builtin := servers[config.BuiltinMCPServer]
	if builtin.Command != "/usr/local/bin/bees" || strings.Join(builtin.Args, " ") != "mcp serve" {
		t.Fatalf("built-in server: %+v", builtin)
	}
	for _, k := range []string{EnvStateDir, EnvSessionDir, EnvRole} {
		if builtin.Env[k] == "" {
			t.Errorf("built-in server env is missing %s: %v", k, builtin.Env)
		}
	}
}

// TestRateLimitEventIsKept: a session's stream carries the account's
// capacity reports, and the last one reaches the Result. The blocking one
// is what pauses the factory, so the trap in the data is asserted too — an
// "allowed" event whose overageStatus is "rejected" must not read as
// blocked, because status is parsed as a field and never matched as a
// substring of the line.
func TestRateLimitEventIsKept(t *testing.T) {
	resets := time.Now().Add(37 * time.Minute).Truncate(time.Second)
	cases := []struct {
		name    string
		events  string
		blocked bool
	}{
		{
			name: "allowed",
			events: `echo '{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","overageStatus":"rejected","resetsAt":RESETS,"rateLimitType":"five_hour","utilization":0.1}}'
`,
		},
		{
			name: "allowed_warning",
			events: `echo '{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":RESETS,"rateLimitType":"five_hour","utilization":0.91}}'
`,
		},
		{
			name: "blocked",
			events: `echo '{"type":"rate_limit_event","rate_limit_info":{"status":"blocked","resetsAt":RESETS,"rateLimitType":"five_hour","utilization":1}}'
`,
			blocked: true,
		},
		{
			// The last event wins: a warning that turned into a block.
			name: "warning then blocked",
			events: `echo '{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":RESETS,"rateLimitType":"five_hour"}}'
echo '{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":RESETS,"rateLimitType":"five_hour"}}'
`,
			blocked: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			events := strings.ReplaceAll(c.events, "RESETS", strconv.FormatInt(resets.Unix(), 10))
			bin := fakeClaude(t, events+`
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"abc","num_turns":1,"total_cost_usd":0.01}'
`)
			r := newRunner(t, bin)
			res, err := r.Run(context.Background(), Request{Name: "t", Role: config.ResolvedRole{Name: "developer", Timeout: time.Minute}, WorkDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if res.RateLimit == nil {
				t.Fatal("no rate limit event kept")
			}
			if res.RateLimit.Type != "five_hour" {
				t.Errorf("rateLimitType = %q, want five_hour", res.RateLimit.Type)
			}
			if !res.RateLimit.ResetsAt.Equal(resets) {
				t.Errorf("resetsAt = %s, want %s", res.RateLimit.ResetsAt, resets)
			}
			at, limited := res.SessionLimited()
			if limited != c.blocked {
				t.Errorf("SessionLimited = %v (status %q), want %v", limited, res.RateLimit.Status, c.blocked)
			}
			if c.blocked && !at.Equal(resets) {
				t.Errorf("SessionLimited reset time = %s, want %s", at, resets)
			}
		})
	}
}

// TestSessionLimitedFromResultText: the second trigger. A session whose
// stream carried no rate-limit event at all still reads as limited when it
// reported the sentence a person sees, and then has no reset time to give.
func TestSessionLimitedFromResultText(t *testing.T) {
	bin := fakeClaude(t, `
echo '{"type":"result","subtype":"error_during_execution","is_error":true,"result":"You'"'"'ve hit your session limit · resets 11:50pm (America/Detroit)","session_id":"abc","num_turns":0}'
`)
	r := newRunner(t, bin)
	res, err := r.Run(context.Background(), Request{Name: "t", Role: config.ResolvedRole{Name: "developer", Timeout: time.Minute}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.RateLimit != nil {
		t.Fatalf("no event was emitted, got %+v", res.RateLimit)
	}
	at, limited := res.SessionLimited()
	if !limited || !at.IsZero() {
		t.Errorf("SessionLimited = %v at %s, want true with no reset time", limited, at)
	}
	// An ordinary failure is not the account limit.
	other := &Result{IsError: true, ResultText: "the tests do not pass"}
	if _, limited := other.SessionLimited(); limited {
		t.Error("an unrelated failure read as the session limit")
	}
	// The result text is the session's own prose, so it is read as a
	// capacity report only from a session that failed with nothing else to
	// say. A bee whose work is the session limit writes those words while
	// doing its job, and must not stop the factory by describing it.
	for _, c := range []struct {
		name string
		res  Result
	}{
		{"reported an outcome", Result{IsError: true, HasOutcome: true, ResultText: "Opened a PR for the session limit issue"}},
		{"finished cleanly", Result{ResultText: "Reviewed the session limit pull request"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if _, limited := c.res.SessionLimited(); limited {
				t.Errorf("a session that %s paused the factory by writing about the limit", c.name)
			}
		})
	}
}

// TestSignalledSessionReportsTheSignal covers a claude process that dies
// from a signal rather than exiting. Go reports an ExitCode of -1 for one,
// which says nothing about why it died, and no result event is emitted, so
// the session used to be reported as "exit_-1" with 0 turns and $0.00 even
// when it had worked for minutes. The signal names the cause, and the turns
// are recovered from the transcript.
func TestSignalledSessionReportsTheSignal(t *testing.T) {
	tests := []struct {
		name    string
		kill    string
		subtype string
		signal  int
	}{
		{name: "SIGKILL", kill: "kill -9 $$", subtype: "signal_killed", signal: 9},
		{name: "SIGHUP", kill: "kill -1 $$", subtype: "signal_hangup", signal: 1},
		// The only signal whose name is two words, so the one that pins
		// the snake case the other subtypes are written in.
		{name: "SIGPIPE", kill: "kill -13 $$", subtype: "signal_broken_pipe", signal: 13},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeClaude(t, `
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"one"}]}}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"two"}]}}'
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"three"}]}}'
`+tc.kill+`
`)
			r := newRunner(t, bin)
			res, err := r.Run(context.Background(), Request{Name: "sig", Role: config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 10, Timeout: time.Minute}, WorkDir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if res.ErrorSubtype != tc.subtype || res.Signal != tc.signal || res.ExitCode != -1 || !res.IsError {
				t.Errorf("subtype %q signal %d exit %d is_error %v, want %q %d -1 true",
					res.ErrorSubtype, res.Signal, res.ExitCode, res.IsError, tc.subtype, tc.signal)
			}
			// The session did work; a report of 0 turns says it did none.
			if res.NumTurns != 3 {
				t.Errorf("turns: got %d, want 3", res.NumTurns)
			}
			if res.CostKnown {
				t.Error("cost reported as known for a session that emitted no result event")
			}
			// result.json is what a person reads afterwards.
			b, err := os.ReadFile(filepath.Join(res.SessionDir, ResultFile))
			if err != nil {
				t.Fatal(err)
			}
			var got Result
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatal(err)
			}
			if got.Signal != tc.signal || got.ErrorSubtype != tc.subtype || got.NumTurns != 3 || got.CostKnown {
				t.Errorf("result.json: %+v", got)
			}
		})
	}
}

// TestRealExitCodeKeepsItsSubtype pins the other half: a process that
// exited of its own accord still reports exit_<n> and no signal.
func TestRealExitCodeKeepsItsSubtype(t *testing.T) {
	bin := fakeClaude(t, "exit 3\n")
	r := newRunner(t, bin)
	res, err := r.Run(context.Background(), Request{Name: "exit3", Role: config.ResolvedRole{Name: "qa", Model: "opus", MaxTurns: 10, Timeout: time.Minute}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorSubtype != "exit_3" || res.ExitCode != 3 || res.Signal != 0 {
		t.Fatalf("subtype %q exit %d signal %d", res.ErrorSubtype, res.ExitCode, res.Signal)
	}
}

// TestASubtypeFromTheStreamWins pins that a subtype claude itself reported
// is not overwritten, whether the process then exits or is signalled.
func TestASubtypeFromTheStreamWins(t *testing.T) {
	bin := fakeClaude(t, `
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"ran out","num_turns":10,"total_cost_usd":1.5}'
kill -9 $$
`)
	r := newRunner(t, bin)
	res, err := r.Run(context.Background(), Request{Name: "streamwins", Role: config.ResolvedRole{Name: "qa", Model: "opus", MaxTurns: 10, Timeout: time.Minute}, WorkDir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorSubtype != "error_max_turns" || res.Signal != 9 || res.NumTurns != 10 || !res.CostKnown || res.CostUSD != 1.5 {
		t.Fatalf("result: %+v", res)
	}
}

// The runner reads the role's resolved sandbox mode. Only "none" is
// implemented, and a role configured for a stronger box refuses to run: a
// session that started anyway would run with everything bees was told to
// keep it away from, and nothing downstream would say so.
func TestRunRefusesASandboxItCannotProvide(t *testing.T) {
	bin := fakeClaude(t, `
touch "$BEES_SESSION_DIR/ran"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	sessions := r.SessionsDir
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 5, Timeout: time.Minute, Sandbox: config.SandboxContainer}
	_, err := r.Run(context.Background(), Request{Name: "boxed", Role: role, WorkDir: t.TempDir(), SystemPrompt: "SYS", Prompt: "TASK"})
	if err == nil {
		t.Fatal("a container session ran without a container")
	}
	for _, want := range []string{"developer", "container"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// Refused before claude was started, not after.
	entries, _ := os.ReadDir(sessions)
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(sessions, e.Name(), "ran")); err == nil {
			t.Error("claude ran for a session bees refused to box")
		}
	}
}

// The mode every session runs in today: "none" runs, and so does a role
// whose mode was never set at all.
func TestRunAcceptsNoSandbox(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'`)
	for _, mode := range []string{"", config.SandboxNone} {
		r := newRunner(t, bin)
		role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 5, Timeout: time.Minute, Sandbox: mode}
		if _, err := r.Run(context.Background(), Request{Name: "plain", Role: role, WorkDir: t.TempDir(), SystemPrompt: "SYS", Prompt: "TASK"}); err != nil {
			t.Errorf("sandbox %q: %v", mode, err)
		}
	}
}

// A role in claude mode runs under Claude Code's sandbox: the permission
// flags change, the settings block goes inline on the command line and a
// copy of it is kept in the session directory.
func TestClaudeSandboxSessionIsBoxed(t *testing.T) {
	bin := fakeClaude(t, `
printf '%s\n' "$@" > "$BEES_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 5, Timeout: time.Minute, Sandbox: config.SandboxClaude,
		MCP: map[string]config.MCPServer{"x": {Command: "srv"}}}
	res, err := r.Run(context.Background(), Request{Name: "boxed", Role: role, WorkDir: t.TempDir(), SystemPrompt: "SYS", Prompt: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	flag := func(name string) string {
		t.Helper()
		i := slices.Index(args, name)
		if i < 0 || i+1 >= len(args) {
			t.Fatalf("args lack %s: %q", name, args)
		}
		return args[i+1]
	}
	if got := flag("--permission-mode"); got != "acceptEdits" {
		t.Errorf("--permission-mode %q, want acceptEdits", got)
	}
	if got := flag("--permission-prompts"); got != "none" {
		t.Errorf("--permission-prompts %q, want none", got)
	}
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Error("a boxed session skips permissions: every question the box leaves to the permission layer is answered yes")
	}
	settings := flag("--settings")
	if strings.HasPrefix(settings, "/") || strings.HasPrefix(settings, res.SessionDir) {
		t.Errorf("--settings is a path, %q: a file in the session directory is one the session can rewrite", settings)
	}
	var got claudeSettings
	if err := json.Unmarshal([]byte(settings), &got); err != nil {
		t.Fatalf("--settings is not JSON: %v\n%s", err, settings)
	}
	sb := got.Sandbox
	if !sb.Enabled || !sb.AutoAllowBashIfSandboxed || sb.AllowUnsandboxedCommands || !sb.FailIfUnavailable || !sb.Network.StrictAllowlist {
		t.Errorf("sandbox block: %+v", sb)
	}
	if !slices.Equal(sb.Network.AllowedDomains, ClaudeSandboxDomains) {
		t.Errorf("allowed domains %q, want %q", sb.Network.AllowedDomains, ClaudeSandboxDomains)
	}
	// Claude Code defaults allowUnsandboxedCommands to true, so the key
	// must be present and false, not absent.
	if !strings.Contains(settings, `"allowUnsandboxedCommands":false`) {
		t.Errorf("allowUnsandboxedCommands is not written as false: %s", settings)
	}
	for _, want := range []string{"Bash", "Read", "WebFetch(domain:github.com)", "WebFetch(domain:*.github.com)", "mcp__bees", "mcp__x"} {
		if !slices.Contains(got.Permissions.Allow, want) {
			t.Errorf("allow rules %q lack %q", got.Permissions.Allow, want)
		}
	}
	for _, rule := range got.Permissions.Allow {
		if strings.HasPrefix(rule, "Edit") || strings.HasPrefix(rule, "Write") || rule == "WebFetch" || rule == "WebSearch" {
			t.Errorf("allow rule %q opens what the box is meant to hold", rule)
		}
	}
	copied, err := os.ReadFile(filepath.Join(res.SessionDir, sandboxFile))
	if err != nil {
		t.Fatalf("no copy of the settings in the session directory: %v", err)
	}
	if string(copied) != settings {
		t.Errorf("the copy differs from what claude was passed:\n%s\n%s", copied, settings)
	}
}

// A session in none mode is the command it always was: permissions skipped,
// no settings block, nothing about a box in the session directory.
func TestUnboxedSessionSkipsPermissions(t *testing.T) {
	bin := fakeClaude(t, `
printf '%s\n' "$@" > "$BEES_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	for _, mode := range []string{"", config.SandboxNone} {
		r := newRunner(t, bin)
		role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 5, Timeout: time.Minute, Sandbox: mode}
		res, err := r.Run(context.Background(), Request{Name: "plain", Role: role, WorkDir: t.TempDir(), SystemPrompt: "SYS", Prompt: "TASK"})
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
		args := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if !slices.Contains(args, "--dangerously-skip-permissions") {
			t.Errorf("sandbox %q: permissions are not skipped: %q", mode, args)
		}
		for _, flag := range []string{"--settings", "--permission-mode", "--permission-prompts"} {
			if slices.Contains(args, flag) {
				t.Errorf("sandbox %q: %s passed to an unboxed session", mode, flag)
			}
		}
		if _, err := os.Stat(filepath.Join(res.SessionDir, sandboxFile)); err == nil {
			t.Errorf("sandbox %q: %s written for an unboxed session", mode, sandboxFile)
		}
	}
}

// The one key that differs by operating system: macOS needs the trust
// daemon reachable for gh to verify TLS, Linux does not have the key.
func TestClaudeSandboxSettingsPerOS(t *testing.T) {
	darwin, err := claudeSandboxSettings([]string{"b", "a"}, "darwin")
	if err != nil {
		t.Fatal(err)
	}
	linux, err := claudeSandboxSettings([]string{"b", "a"}, "linux")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(darwin), `"enableWeakerNetworkIsolation":true`) {
		t.Errorf("macOS settings do not reach the trust daemon: %s", darwin)
	}
	if strings.Contains(string(linux), "enableWeakerNetworkIsolation") {
		t.Errorf("Linux settings name a macOS key: %s", linux)
	}
	// Servers are listed in name order, so the block is the same run to run.
	var got claudeSettings
	if err := json.Unmarshal(linux, &got); err != nil {
		t.Fatal(err)
	}
	if i, j := slices.Index(got.Permissions.Allow, "mcp__a"), slices.Index(got.Permissions.Allow, "mcp__b"); i < 0 || j < 0 || i > j {
		t.Errorf("MCP allow rules %q are not in name order", got.Permissions.Allow)
	}
}
