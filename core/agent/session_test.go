package agent

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

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

// fakeClaude writes a shell script standing in for the claude binary.
func fakeClaude(t *testing.T, body string) string { return agenttest.Script(t, "claude", body) }

func newRunner(t *testing.T, bin string) *testRunner {
	t.Helper()
	return &testRunner{Runner: &Runner{ClaudeBin: bin, SessionsDir: t.TempDir(), EnvironmentPrefix: "TASK_", NamePrefix: "task-", ContainerLabel: "task.session", ContainerHome: "/home/task"}, StateDir: "/state", ServerBin: "/usr/local/bin/task"}
}

// A session started from inside another session must not inherit its TASK_*
// variables: only the ones the runner sets for it are visible.

func TestRunErrorAndNoOutcome(t *testing.T) {
	bin := fakeClaude(t, `
echo '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"ran out","num_turns":10}'
exit 1
`)
	r := newRunner(t, bin)
	res, err := r.Run(context.Background(), Request{Name: "t2", Profile: Profile{Name: "auditor", Model: "opus", MaxTurns: 10}, Workspace: fakeWorkspace{dir: t.TempDir()}})
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
	res, err := r.Run(context.Background(), Request{Name: "t3", Profile: Profile{Name: "auditor", Model: "opus", MaxTurns: 1, Timeout: 200 * time.Millisecond}, Workspace: fakeWorkspace{dir: t.TempDir()}})
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
	if err := WriteOutcome(dir, Outcome{Status: "accepted", Note: "n"}); err != nil {
		t.Fatal(err)
	}
	o, ok, err := ReadOutcome(dir)
	if err != nil || !ok || o.Status != "accepted" {
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
			res, err := r.Run(context.Background(), Request{Name: "t", Profile: Profile{Name: "builder", Timeout: time.Minute}, Workspace: fakeWorkspace{dir: t.TempDir()}})
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
	res, err := r.Run(context.Background(), Request{Name: "t", Profile: Profile{Name: "builder", Timeout: time.Minute}, Workspace: fakeWorkspace{dir: t.TempDir()}})
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
			res, err := r.Run(context.Background(), Request{Name: "sig", Profile: Profile{Name: "builder", Model: "opus", MaxTurns: 10, Timeout: time.Minute}, Workspace: fakeWorkspace{dir: t.TempDir()}})
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
	res, err := r.Run(context.Background(), Request{Name: "exit3", Profile: Profile{Name: "auditor", Model: "opus", MaxTurns: 10, Timeout: time.Minute}, Workspace: fakeWorkspace{dir: t.TempDir()}})
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
	res, err := r.Run(context.Background(), Request{Name: "streamwins", Profile: Profile{Name: "auditor", Model: "opus", MaxTurns: 10, Timeout: time.Minute}, Workspace: fakeWorkspace{dir: t.TempDir()}})
	if err != nil {
		t.Fatal(err)
	}
	if res.ErrorSubtype != "error_max_turns" || res.Signal != 9 || res.NumTurns != 10 || !res.CostKnown || res.CostUSD != 1.5 {
		t.Fatalf("result: %+v", res)
	}
}

// fakeCodex writes a shell script standing in for the codex binary.
func fakeCodex(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "codex")
	script := "#!/bin/sh\nset -e\n" + body
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// codexRole is a role resolved with agent = "codex".
func codexRole(model string) Profile {
	return Profile{Name: "builder", Agent: AgentCodex, Model: model, MaxTurns: 10, Timeout: time.Minute}
}

// TestCodexRunSuccess covers a session of a role whose agent is codex: the
// runner starts `codex exec --json` instead of `claude -p`, with codex's
// own switch for running unattended, the system prompt ahead of the task
// on stdin, and every MCP server — the built-in one included — passed as
// configuration overrides; and it reads the thread id, the last agent
// message and the turn count off codex's event stream, with no cost.
func TestCodexRunSuccess(t *testing.T) {
	bin := fakeCodex(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
cat > "$TASK_SESSION_DIR/stdin.txt"
env > "$TASK_SESSION_DIR/env.txt"
echo '{"type":"thread.started","thread_id":"thread-7"}'
echo '{"type":"turn.started"}'
echo '{"type":"item.started","item":{"id":"item_0","type":"command_execution","command":"ls","status":"in_progress"}}'
echo '{"type":"item.completed","item":{"id":"item_0","type":"command_execution","command":"ls","aggregated_output":"a b","exit_code":0,"status":"completed"}}'
echo '{"type":"item.completed","item":{"id":"item_1","type":"mcp_tool_call","server":"task","tool":"done","status":"completed"}}'
echo '{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"all done"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":120,"cached_input_tokens":0,"output_tokens":30}}'
printf '{"status":"submitted","work":{"key":"task/12","tags":{"ticket":"twelve"}},"note":"hi"}' > "$TASK_SESSION_DIR/outcome.json"
`)
	r := newRunner(t, "")
	r.CodexBin = bin
	role := codexRole("gpt-5-codex")
	role.Fallback = &Profile{Agent: AgentClaude, Model: "sonnet"}
	role.Effort = "max"
	role.AllowedTools = []string{"Bash"}
	role.MCP = map[string]MCPEntry{"x": {Command: "srv", Args: []string{"--port", "1"}, Env: map[string]string{"K": "$HOME"}}}
	res, err := r.Run(context.Background(), Request{Name: "c1", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{EnvIssue: "12"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "all done" || res.NumTurns != 3 || res.ClaudeID != "thread-7" || res.ExitCode != 0 {
		t.Fatalf("result: %+v", res)
	}
	if res.CostKnown || res.CostUSD != 0 {
		t.Errorf("a codex session reported a cost: known %v, %v", res.CostKnown, res.CostUSD)
	}
	if !res.HasOutcome || res.Outcome.Status != "submitted" || res.Outcome.Work.Key != "task/12" || res.Outcome.Work.Tags["ticket"] != "twelve" {
		t.Fatalf("outcome: %+v", res.Outcome)
	}
	b, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	args := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if head := strings.Join(args[:4], " "); head != "exec --json --dangerously-bypass-approvals-and-sandbox --skip-git-repo-check" {
		t.Errorf("args start %q", head)
	}
	if args[len(args)-1] != "-" {
		t.Errorf("the prompt argument is %q, want - (read stdin)", args[len(args)-1])
	}
	for _, want := range []string{
		"--model", "gpt-5-codex",
		`model_reasoning_effort="high"`,
		`mcp_servers.tools.command="/usr/local/bin/task"`,
		`mcp_servers.tools.args=["mcp","serve"]`,
		`mcp_servers.tools.env.TASK_ROLE="builder"`,
		`mcp_servers.tools.env.TASK_SESSION_DIR="` + res.SessionDir + `"`,
		`mcp_servers.tools.env.TASK_ISSUE="12"`,
		`mcp_servers.x.command="srv"`,
		`mcp_servers.x.args=["--port","1"]`,
		`mcp_servers.x.env.K="` + os.Getenv("HOME") + `"`,
	} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %q:\n%s", want, b)
		}
	}
	// Every -c override is its own argument, after a -c of its own.
	for i, a := range args {
		if strings.HasPrefix(a, "mcp_servers.") || strings.HasPrefix(a, "model_reasoning_effort=") {
			if i == 0 || args[i-1] != "-c" {
				t.Errorf("override %q is not preceded by -c", a)
			}
		}
	}
	// Nothing of claude's command line leaks into codex's.
	for _, gone := range []string{"-p", "--dangerously-skip-permissions", "--append-system-prompt-file", "--max-turns", "--fallback-model", "--effort", "--allowedTools", "--mcp-config", "--strict-mcp-config", "--add-dir", "--name"} {
		if slices.Contains(args, gone) {
			t.Errorf("args carry claude's %s:\n%s", gone, b)
		}
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "mcp.json")); err == nil {
		t.Error("mcp.json written for a codex session, which never reads it")
	}
	stdin, _ := os.ReadFile(filepath.Join(res.SessionDir, "stdin.txt"))
	if string(stdin) != "SYS\n\n---\n\nTASK" {
		t.Errorf("stdin: %q", stdin)
	}
	// The two prompts are still written apart, for whoever reads the
	// session directory.
	for name, want := range map[string]string{"system-prompt.md": "SYS", "prompt.md": "TASK"} {
		got, _ := os.ReadFile(filepath.Join(res.SessionDir, name))
		if string(got) != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	env, _ := os.ReadFile(filepath.Join(res.SessionDir, "env.txt"))
	for _, want := range []string{"TASK_ROLE=builder", "TASK_STATE_DIR=/state", "TASK_ISSUE=12", "TASK_BIN=/usr/local/bin/task"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("env missing %s", want)
		}
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, TranscriptFile)); err != nil {
		t.Fatal("transcript missing")
	}
	// result.json says the cost is unknown, as it does for a signalled
	// claude session.
	rb, _ := os.ReadFile(filepath.Join(res.SessionDir, ResultFile))
	var got Result
	if err := json.Unmarshal(rb, &got); err != nil {
		t.Fatal(err)
	}
	if got.CostKnown || got.ClaudeID != "thread-7" || got.NumTurns != 3 {
		t.Errorf("result.json: %+v", got)
	}
}

// A codex role with no model leaves the choice to codex: no --model at all,
// rather than claude's default alias, and a plain effort level goes through
// unchanged. The system prompt alone is stdin when the task is empty.
func TestCodexLeavesTheModelToCodexWhenUnset(t *testing.T) {
	bin := fakeCodex(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
echo '{"type":"turn.completed","usage":{}}'
`)
	r := newRunner(t, "")
	r.CodexBin = bin
	role := codexRole("")
	role.Effort = "low"
	res, err := r.Run(context.Background(), Request{Name: "c2", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	args := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if slices.Contains(args, "--model") {
		t.Errorf("--model passed for a role with no model:\n%s", b)
	}
	if !slices.Contains(args, `model_reasoning_effort="low"`) {
		t.Errorf("effort low not passed through:\n%s", b)
	}
	if res.IsError || res.NumTurns != 0 || res.ResultText != "" {
		t.Errorf("result: %+v", res)
	}
}

// TestCodexFailedTurn: codex ends a turn it could not finish with
// "turn.failed", whose message is the result text when the session said
// nothing else, and that is an error even when the process then exits
// cleanly; and a message naming the usage limit reads as the account
// limit, through the same phrase check a claude session's result text
// goes through.
func TestCodexFailedTurn(t *testing.T) {
	for _, tc := range []struct {
		name, events, subtype, text string
		exit                        int
		limited                     bool
	}{
		{
			name: "turn.failed, clean exit",
			events: `echo '{"type":"thread.started","thread_id":"t"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"trying"}}'
echo '{"type":"turn.failed","error":{"message":"stream disconnected"}}'
`,
			subtype: "turn_failed", text: "stream disconnected", exit: 0,
		},
		{
			name: "error event",
			events: `echo '{"type":"error","message":"You have hit your usage limit. Try again at 3pm."}'
`,
			subtype: "error", text: "You have hit your usage limit. Try again at 3pm.", exit: 1, limited: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeCodex(t, tc.events+"exit "+strconv.Itoa(tc.exit)+"\n")
			r := newRunner(t, "")
			r.CodexBin = bin
			res, err := r.Run(context.Background(), Request{Name: "c3", Profile: codexRole(""), Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK"})
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || res.ErrorSubtype != tc.subtype || res.ResultText != tc.text || res.ExitCode != tc.exit || res.HasOutcome {
				t.Fatalf("result: %+v", res)
			}
			if res.RateLimit != nil {
				t.Errorf("a codex session reported a rate-limit event: %+v", res.RateLimit)
			}
			if _, limited := res.SessionLimited(); limited != tc.limited {
				t.Errorf("SessionLimited = %v, want %v", limited, tc.limited)
			}
		})
	}
}

// A codex stream that ends with no turn end — the process was killed, or
// it crashed — is a session that never said how it went, like a claude
// stream with no result event: no_result, and the turns counted from the
// transcript's completed items.
func TestCodexStreamWithoutATurnEnd(t *testing.T) {
	bin := fakeCodex(t, `
echo '{"type":"thread.started","thread_id":"t"}'
echo '{"type":"item.completed","item":{"type":"command_execution","command":"go test ./..."}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"half way"}}'
`)
	r := newRunner(t, "")
	r.CodexBin = bin
	res, err := r.Run(context.Background(), Request{Name: "c4", Profile: codexRole(""), Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.ErrorSubtype != "no_result" || res.NumTurns != 2 || res.CostKnown {
		t.Fatalf("result: %+v", res)
	}
}

// The agent setting is validated when task.toml loads, so a value the
// runner does not know is a role that never went through config; it is
// refused rather than run as claude.
func TestUnknownAgentIsRefused(t *testing.T) {
	bin := fakeClaude(t, `touch "$TASK_SESSION_DIR/ran"`)
	r := newRunner(t, bin)
	role := Profile{Name: "builder", Agent: "gpt", Timeout: time.Minute}
	_, err := r.Run(context.Background(), Request{Name: "c5", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK"})
	if err == nil || !strings.Contains(err.Error(), `"gpt"`) {
		t.Fatalf("err = %v, want one naming the agent", err)
	}
}

// TestCodexMCPOverrides pins how MCP entries become codex configuration:
// one dotted override per key, servers in name order, a remote server by
// its url and headers, and every value a JSON string literal — so a value
// with a quote or a newline in it stays one argument codex can parse
// whether it reads overrides as JSON or as TOML.
func TestCodexMCPOverrides(t *testing.T) {
	got := codexMCPOverrides(map[string]MCPEntry{
		"zeta":  {Type: "stdio", Command: "/bin/z", Args: []string{"a b", `q"uote`}, Env: map[string]string{"B": "2", "A": "line\nbreak"}, EnvVars: []string{"API_TOKEN"}},
		"alpha": {Type: "http", URL: "https://x.example/mcp", Headers: map[string]string{"Authorization": "Bearer t"}},
		"empty": {},
	})
	want := []string{
		`mcp_servers.alpha.url="https://x.example/mcp"`,
		`mcp_servers.alpha.http_headers.Authorization="Bearer t"`,
		`mcp_servers.zeta.command="/bin/z"`,
		`mcp_servers.zeta.args=["a b","q\"uote"]`,
		`mcp_servers.zeta.env_vars=["API_TOKEN"]`,
		`mcp_servers.zeta.env.A="line\nbreak"`,
		`mcp_servers.zeta.env.B="2"`,
	}
	if !slices.Equal(got, want) {
		t.Errorf("overrides:\n got %q\nwant %q", got, want)
	}
	// Every value parses as the JSON it claims to be.
	for _, o := range got {
		_, v, _ := strings.Cut(o, "=")
		var any any
		if err := json.Unmarshal([]byte(v), &any); err != nil {
			t.Errorf("%s: value is not JSON: %v", o, err)
		}
	}
}

// The runner reads the role's resolved sandbox mode. Only "none" is
// implemented, and a role configured for a stronger box refuses to run: a
// session that started anyway would run with everything task was told to
// keep it away from, and nothing downstream would say so.
func TestRunRefusesASandboxItCannotProvide(t *testing.T) {
	bin := fakeClaude(t, `
touch "$TASK_SESSION_DIR/ran"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	sessions := r.SessionsDir
	role := Profile{Name: "builder", Model: "opus", MaxTurns: 5, Timeout: time.Minute, Sandbox: SandboxContainer}
	_, err := r.Run(context.Background(), Request{Name: "boxed", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, SystemPrompt: "SYS", Prompt: "TASK"})
	if err == nil {
		t.Fatal("a container session ran without a container")
	}
	for _, want := range []string{"builder", "container"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	// Refused before claude was started, not after.
	entries, _ := os.ReadDir(sessions)
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(sessions, e.Name(), "ran")); err == nil {
			t.Error("claude ran for a session task refused to box")
		}
	}
}

// The mode every session runs in today: "none" runs, and so does a role
// whose mode was never set at all.
func TestRunAcceptsNoSandbox(t *testing.T) {
	bin := fakeClaude(t, `echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'`)
	for _, mode := range []string{"", SandboxNone} {
		r := newRunner(t, bin)
		role := Profile{Name: "builder", Model: "opus", MaxTurns: 5, Timeout: time.Minute, Sandbox: mode}
		if _, err := r.Run(context.Background(), Request{Name: "plain", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, SystemPrompt: "SYS", Prompt: "TASK"}); err != nil {
			t.Errorf("sandbox %q: %v", mode, err)
		}
	}
}

// A role in claude mode runs under Claude Code's sandbox: the permission
// flags change, the settings block goes inline on the command line and a
// copy of it is kept in the session directory.
func TestClaudeSandboxSessionIsBoxed(t *testing.T) {
	bin := fakeClaude(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	role := Profile{Name: "builder", Model: "opus", MaxTurns: 5, Timeout: time.Minute, Sandbox: SandboxClaude,
		MCP: map[string]MCPEntry{"x": {Command: "srv"}}}
	res, err := r.Run(context.Background(), Request{Name: "boxed", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, SystemPrompt: "SYS", Prompt: "TASK"})
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
	if want := testDomains; !slices.Equal(sb.Network.AllowedDomains, want) {
		t.Errorf("allowed domains %q, want %q", sb.Network.AllowedDomains, want)
	}
	// Claude Code defaults allowUnsandboxedCommands to true, so the key
	// must be present and false, not absent.
	if !strings.Contains(settings, `"allowUnsandboxedCommands":false`) {
		t.Errorf("allowUnsandboxedCommands is not written as false: %s", settings)
	}
	for _, want := range []string{"Bash", "Read", "WebFetch(domain:example.com)", "WebFetch(domain:*.example.com)", "mcp__tools", "mcp__x"} {
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
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	for _, mode := range []string{"", SandboxNone} {
		r := newRunner(t, bin)
		role := Profile{Name: "builder", Model: "opus", MaxTurns: 5, Timeout: time.Minute, Sandbox: mode}
		res, err := r.Run(context.Background(), Request{Name: "plain", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, SystemPrompt: "SYS", Prompt: "TASK"})
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
	darwin, err := claudeSandboxSettings([]string{"b", "a"}, "darwin", testDomains, nil)
	if err != nil {
		t.Fatal(err)
	}
	linux, err := claudeSandboxSettings([]string{"b", "a"}, "linux", testDomains, nil)
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

// A codex role asking for the claude box is refused before anything starts:
// codex would run with its own sandbox switched off and nothing boxing it.
func TestRunRefusesTheClaudeSandboxForCodex(t *testing.T) {
	bin := fakeClaude(t, `
touch "$TASK_SESSION_DIR/ran"
echo '{"type":"turn.completed"}'
`)
	r := newRunner(t, bin)
	r.CodexBin = bin
	sessions := r.SessionsDir
	role := Profile{Name: "auditor", Agent: AgentCodex, Timeout: time.Minute, Sandbox: SandboxClaude}
	_, err := r.Run(context.Background(), Request{Name: "boxed-codex", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, SystemPrompt: "SYS", Prompt: "TASK"})
	if err == nil {
		t.Fatal("a codex session ran in a claude box")
	}
	for _, want := range []string{"auditor", "codex", "claude"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	entries, _ := os.ReadDir(sessions)
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(sessions, e.Name(), "ran")); err == nil {
			t.Error("codex ran for a session task refused to box")
		}
	}
}

// TestClaudeCommandResumes: a request naming a session to resume launches
// claude with --resume and the system-prompt snapshot off, so the system
// prompt rendered for this round is the one it reads rather than the
// recording of round 1's; a request without one passes neither flag.
func TestClaudeCommandResumes(t *testing.T) {
	r := newRunner(t, "claude")
	role := Profile{Name: "builder", Model: "opus", MaxTurns: 5, Timeout: time.Minute}
	for _, id := range []string{"", "abc-123"} {
		dir := t.TempDir()
		paths := sessionPaths{dir: dir, systemPrompt: filepath.Join(dir, "system-prompt.md"), prompt: filepath.Join(dir, "prompt.md"),
			mcp: map[string]MCPEntry{"tools": {Command: "task", Args: []string{"mcp", "serve"}}}}
		_, args, _, _, err := claudeBackend{}.command(context.Background(), r.Runner, backendNamed(t, AgentClaude), Request{Name: "n", Profile: role, Prompt: "TASK", ResumeID: id}, paths)
		if err != nil {
			t.Fatal(err)
		}
		i := slices.Index(args, "--resume")
		j := slices.Index(args, "--system-prompt-snapshot")
		if id == "" {
			if i >= 0 || j >= 0 {
				t.Errorf("no resume id, but the flags were passed: %q", args)
			}
			continue
		}
		if i < 0 || i+1 >= len(args) || args[i+1] != id {
			t.Errorf("resume id %q not passed: %q", id, args)
		}
		if j < 0 || j+1 >= len(args) || args[j+1] != "off" {
			t.Errorf("resumed launch keeps the system-prompt snapshot: %q", args)
		}
		// The round's own system prompt still goes along: the snapshot flag
		// is what makes claude read it.
		if k := slices.Index(args, "--append-system-prompt-file"); k < 0 || args[k+1] != paths.systemPrompt {
			t.Errorf("system prompt file not passed on the resumed launch: %q", args)
		}
	}
}

// TestCodexCommandIgnoresResume: codex exec has no resume, so a request
// naming a session to resume builds the same codex command line as one
// that does not.
func TestCodexCommandIgnoresResume(t *testing.T) {
	r := newRunner(t, "")
	paths := sessionPaths{dir: t.TempDir(), mcp: map[string]MCPEntry{"tools": {Command: "task", Args: []string{"mcp", "serve"}}}}
	var got [][]string
	for _, id := range []string{"", "abc-123"} {
		_, args, stdin, _, err := codexBackend{}.command(context.Background(), r.Runner, backendNamed(t, AgentCodex), Request{Name: "n", Profile: codexRole("gpt-5"), SystemPrompt: "SYS", Prompt: "TASK", ResumeID: id}, paths)
		if err != nil {
			t.Fatal(err)
		}
		if stdin != "SYS\n\n---\n\nTASK" {
			t.Errorf("stdin: %q", stdin)
		}
		for _, a := range args {
			if a == "--resume" || strings.Contains(a, "resume") || a == "--system-prompt-snapshot" || a == id {
				t.Errorf("resume id %q reached codex as %q: %q", id, a, args)
			}
		}
		got = append(got, args)
	}
	if !slices.Equal(got[0], got[1]) {
		t.Errorf("the resume id changed codex's command line:\n%q\n%q", got[0], got[1])
	}
}

// A claude session is told the model of a claude fallback, so claude can
// switch to it itself within the session; a fallback on another agent is a
// new session, the caller's to start (ops.SelectProfile), and claude is
// told nothing of it.
func TestClaudeFallbackModelIsAClaudeFallbacksAlone(t *testing.T) {
	for _, tc := range []struct {
		name     string
		fallback *Profile
		want     string
	}{
		{"none", nil, ""},
		{"claude", &Profile{Agent: AgentClaude, Model: "sonnet"}, "sonnet"},
		{"unnamed agent", &Profile{Model: "haiku"}, "haiku"},
		{"same model", &Profile{Agent: AgentClaude, Model: "opus"}, ""},
		{"no model", &Profile{Agent: AgentClaude}, ""},
		{"codex", &Profile{Agent: AgentCodex, Model: "gpt"}, ""},
		{"opencode", &Profile{Agent: AgentOpenCode, Model: "ollama/x"}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeClaude(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"s","num_turns":1}'
`)
			r := newRunner(t, bin)
			res, err := r.Run(context.Background(), Request{Name: "f", Profile: Profile{Name: "auditor", Model: "opus", MaxTurns: 1, Fallback: tc.fallback}, Workspace: fakeWorkspace{dir: t.TempDir()}})
			if err != nil {
				t.Fatal(err)
			}
			b, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
			args := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
			got := ""
			if i := slices.Index(args, "--fallback-model"); i >= 0 && i+1 < len(args) {
				got = args[i+1]
			}
			if got != tc.want {
				t.Errorf("--fallback-model %q, want %q:\n%s", got, tc.want, b)
			}
		})
	}
}
