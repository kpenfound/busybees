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
)

// fakeOpenCode writes a shell script standing in for the opencode binary.
func fakeOpenCode(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "opencode")
	script := "#!/bin/sh\nset -e\n" + body
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// opencodeRole is a role resolved with agent = "opencode".
func opencodeRole(model string) Profile {
	return Profile{Name: "builder", Agent: AgentOpenCode, Model: model, MaxTurns: 10, Timeout: time.Minute}
}

// TestOpenCodeRunSuccess covers a session of a role whose agent is
// opencode: the runner starts `opencode run --format json --auto` instead
// of `claude -p`, the task on stdin, and hands it a configuration file of
// its own through OPENCODE_CONFIG — the system prompt as an instruction
// file and every MCP server, the built-in one included, as a server — and
// reads the session id, the last text, the finished steps and their
// summed cost off opencode's event stream.
func TestOpenCodeRunSuccess(t *testing.T) {
	bin := fakeOpenCode(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
cat > "$TASK_SESSION_DIR/stdin.txt"
env > "$TASK_SESSION_DIR/env.txt"
echo '{"type":"step_start","timestamp":1,"sessionID":"ses_1","part":{"type":"step-start"}}'
echo '{"type":"tool_use","timestamp":2,"sessionID":"ses_1","part":{"type":"tool","tool":"bash","state":{"status":"completed"}}}'
echo '{"type":"step_finish","timestamp":3,"sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":0.25,"tokens":{"input":1,"output":1}}}'
echo '{"type":"text","timestamp":4,"sessionID":"ses_1","part":{"type":"text","text":"half way"}}'
echo '{"type":"step_finish","timestamp":5,"sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":0.5}}'
echo '{"type":"text","timestamp":6,"sessionID":"ses_1","part":{"type":"text","text":"all done"}}'
echo '{"type":"step_finish","timestamp":7,"sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","cost":0.25}}'
printf '{"status":"submitted","pr":12,"note":"hi"}' > "$TASK_SESSION_DIR/outcome.json"
`)
	r := newRunner(t, "")
	r.OpenCodeBin = bin
	role := opencodeRole("ollama/llama3")
	role.FallbackModel = "sonnet"
	role.Effort = "max"
	role.AllowedTools = []string{"Bash"}
	role.MCP = map[string]MCPEntry{
		"x":      {Command: "srv", Args: []string{"--port", "1"}, Env: map[string]string{"K": "$HOME"}},
		"remote": {URL: "https://x.example/mcp", Headers: map[string]string{"Authorization": "Bearer t"}},
	}
	res, err := r.Run(context.Background(), Request{Name: "o1", Profile: role, WorkDir: t.TempDir(), SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{EnvIssue: "12"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "all done" || res.NumTurns != 3 || res.ClaudeID != "ses_1" || res.ExitCode != 0 {
		t.Fatalf("result: %+v", res)
	}
	if !res.CostKnown || res.CostUSD != 1 {
		t.Errorf("cost: known %v, %v; want the steps' sum, 1", res.CostKnown, res.CostUSD)
	}
	if !res.HasOutcome || res.Outcome.Status != "submitted" || res.Outcome.PR != 12 {
		t.Fatalf("outcome: %+v", res.Outcome)
	}
	b, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	args := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
	if head := strings.Join(args[:4], " "); head != "run --format json --auto" {
		t.Errorf("args start %q", head)
	}
	for _, want := range [][2]string{{"--title", "task-o1"}, {"--model", "ollama/llama3"}} {
		if i := slices.Index(args, want[0]); i < 0 || i+1 >= len(args) || args[i+1] != want[1] {
			t.Errorf("args missing %s %s:\n%s", want[0], want[1], b)
		}
	}
	// The task is on stdin alone, not on the command line: the system
	// prompt reaches opencode as an instruction file.
	if args[len(args)-1] == "TASK" || slices.Contains(args, "SYS") {
		t.Errorf("a prompt is on the command line:\n%s", b)
	}
	stdin, _ := os.ReadFile(filepath.Join(res.SessionDir, "stdin.txt"))
	if string(stdin) != "TASK" {
		t.Errorf("stdin: %q", stdin)
	}
	// Nothing of claude's or codex's command line leaks into opencode's.
	for _, gone := range []string{"-p", "--dangerously-skip-permissions", "--append-system-prompt-file", "--max-turns", "--fallback-model", "--effort", "--variant", "--allowedTools", "--mcp-config", "--strict-mcp-config", "--add-dir", "--name", "--session", "exec", "--json", "-c"} {
		if slices.Contains(args, gone) {
			t.Errorf("args carry %s:\n%s", gone, b)
		}
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "mcp.json")); err == nil {
		t.Error("mcp.json written for an opencode session, which never reads it")
	}
	// OPENCODE_CONFIG names the session's own file, and the file holds the
	// system prompt as an instruction and both kinds of server.
	env, _ := os.ReadFile(filepath.Join(res.SessionDir, "env.txt"))
	configPath := filepath.Join(res.SessionDir, OpenCodeConfigFile)
	for _, want := range []string{EnvOpenCodeConfig + "=" + configPath, "TASK_ROLE=builder", "TASK_STATE_DIR=/state", "TASK_ISSUE=12", "TASK_BIN=/usr/local/bin/task"} {
		if !strings.Contains(string(env), want+"\n") {
			t.Errorf("env missing %s", want)
		}
	}
	cb, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Schema       string                   `json:"$schema"`
		Instructions []string                 `json:"instructions"`
		Agent        map[string]opencodeAgent `json:"agent"`
		MCP          map[string]opencodeMCP   `json:"mcp"`
	}
	if err := json.Unmarshal(cb, &cfg); err != nil {
		t.Fatalf("opencode.json: %v\n%s", err, cb)
	}
	if cfg.Schema == "" || !slices.Equal(cfg.Instructions, []string{filepath.Join(res.SessionDir, "system-prompt.md")}) {
		t.Errorf("opencode.json: %s", cb)
	}
	if cfg.Agent["build"].Variant != "max" {
		t.Errorf("opencode.json build variant = %q, want max:\n%s", cfg.Agent["build"].Variant, cb)
	}
	if len(cfg.MCP) != 3 {
		t.Errorf("opencode.json names %d servers, want 3:\n%s", len(cfg.MCP), cb)
	}
	task := cfg.MCP["tools"]
	if task.Type != "local" || !slices.Equal(task.Command, []string{"/usr/local/bin/task", "mcp", "serve"}) || !task.Enabled {
		t.Errorf("built-in server: %+v", task)
	}
	for k, want := range map[string]string{"TASK_ROLE": "builder", "TASK_SESSION_DIR": res.SessionDir, "TASK_ISSUE": "12"} {
		if task.Environment[k] != want {
			t.Errorf("built-in server environment %s = %q, want %q", k, task.Environment[k], want)
		}
	}
	if x := cfg.MCP["x"]; x.Type != "local" || !slices.Equal(x.Command, []string{"srv", "--port", "1"}) || x.Environment["K"] != os.Getenv("HOME") {
		t.Errorf("server x: %+v", x)
	}
	if remote := cfg.MCP["remote"]; remote.Type != "remote" || remote.URL != "https://x.example/mcp" || remote.Headers["Authorization"] != "Bearer t" || len(remote.Command) != 0 {
		t.Errorf("server remote: %+v", remote)
	}
	// The two prompts are still written apart.
	for name, want := range map[string]string{"system-prompt.md": "SYS", "prompt.md": "TASK"} {
		got, _ := os.ReadFile(filepath.Join(res.SessionDir, name))
		if string(got) != want {
			t.Errorf("%s: %q, want %q", name, got, want)
		}
	}
	rb, _ := os.ReadFile(filepath.Join(res.SessionDir, ResultFile))
	var got Result
	if err := json.Unmarshal(rb, &got); err != nil {
		t.Fatal(err)
	}
	if !got.CostKnown || got.CostUSD != 1 || got.ClaudeID != "ses_1" || got.NumTurns != 3 {
		t.Errorf("result.json: %+v", got)
	}
}

// An opencode role with no model leaves the choice to opencode: no --model
// at all. A session with no system prompt lists no instruction file, and
// a local model's zero cost is a known cost, not an unknown one.
func TestOpenCodeLeavesTheModelToOpenCodeWhenUnset(t *testing.T) {
	bin := fakeOpenCode(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
echo '{"type":"step_finish","timestamp":1,"sessionID":"ses_2","part":{"type":"step-finish","reason":"stop","cost":0}}'
`)
	r := newRunner(t, "")
	r.OpenCodeBin = bin
	res, err := r.Run(context.Background(), Request{Name: "o2", Profile: opencodeRole(""), WorkDir: t.TempDir(), Prompt: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(filepath.Join(res.SessionDir, "args.txt"))
	if strings.Contains(string(b), "--model") {
		t.Errorf("--model passed for a role with no model:\n%s", b)
	}
	if res.IsError || res.NumTurns != 1 || res.ResultText != "" || !res.CostKnown || res.CostUSD != 0 {
		t.Errorf("result: %+v", res)
	}
	cb, _ := os.ReadFile(filepath.Join(res.SessionDir, OpenCodeConfigFile))
	if strings.Contains(string(cb), "instructions") {
		t.Errorf("an instruction file listed with no system prompt:\n%s", cb)
	}
	if strings.Contains(string(cb), "variant") {
		t.Errorf("a variant listed with no effort:\n%s", cb)
	}
}

// TestOpenCodeFailedRun: an "error" event is a run that did not finish,
// whose message is the result text when the session said nothing else,
// even when the process then exits cleanly; a message naming the usage
// limit reads as the account limit; and a step the model ended for a
// reason of its own is a failure named after it.
func TestOpenCodeFailedRun(t *testing.T) {
	for _, tc := range []struct {
		name, events, subtype, text string
		exit                        int
		limited                     bool
	}{
		{
			name: "error event, clean exit",
			events: `echo '{"type":"text","sessionID":"ses_3","part":{"type":"text","text":"trying"}}'
echo '{"type":"error","sessionID":"ses_3","error":{"name":"APIError","data":{"message":"stream disconnected"}}}'
`,
			subtype: "error", text: "stream disconnected", exit: 0,
		},
		{
			name: "error with no message",
			events: `echo '{"type":"error","sessionID":"ses_3","error":{"name":"ProviderAuthError"}}'
`,
			subtype: "error", text: "ProviderAuthError", exit: 1,
		},
		{
			name: "rate limit",
			events: `echo '{"type":"error","sessionID":"ses_3","error":{"name":"APIError","data":{"message":"You have hit your usage limit. Try again at 3pm.","statusCode":429}}}'
`,
			subtype: "error", text: "You have hit your usage limit. Try again at 3pm.", exit: 1, limited: true,
		},
		{
			name: "step ended by length",
			events: `echo '{"type":"text","sessionID":"ses_3","part":{"type":"text","text":"cut off"}}'
echo '{"type":"step_finish","sessionID":"ses_3","part":{"type":"step-finish","reason":"length","cost":0.1}}'
`,
			subtype: "step_length", text: "cut off", exit: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakeOpenCode(t, tc.events+"exit "+strconv.Itoa(tc.exit)+"\n")
			r := newRunner(t, "")
			r.OpenCodeBin = bin
			res, err := r.Run(context.Background(), Request{Name: "o3", Profile: opencodeRole(""), WorkDir: t.TempDir(), Prompt: "TASK"})
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError || res.ErrorSubtype != tc.subtype || res.ResultText != tc.text || res.ExitCode != tc.exit || res.HasOutcome || res.ClaudeID != "ses_3" {
				t.Fatalf("result: %+v", res)
			}
			if res.RateLimit != nil {
				t.Errorf("an opencode session reported a rate-limit event: %+v", res.RateLimit)
			}
			if _, limited := res.SessionLimited(); limited != tc.limited {
				t.Errorf("SessionLimited = %v, want %v", limited, tc.limited)
			}
		})
	}
}

// An opencode stream that ends with no final step and no error — the
// process was killed, or it crashed — is a session that never said how it
// went: no_result, the turns counted from the transcript's finished steps,
// and no cost.
func TestOpenCodeStreamWithoutAnEnd(t *testing.T) {
	bin := fakeOpenCode(t, `
echo '{"type":"step_finish","sessionID":"ses_4","part":{"type":"step-finish","reason":"tool-calls","cost":0.1}}'
echo '{"type":"text","sessionID":"ses_4","part":{"type":"text","text":"half way"}}'
echo '{"type":"step_finish","sessionID":"ses_4","part":{"type":"step-finish","reason":"tool-calls","cost":0.1}}'
`)
	r := newRunner(t, "")
	r.OpenCodeBin = bin
	res, err := r.Run(context.Background(), Request{Name: "o4", Profile: opencodeRole(""), WorkDir: t.TempDir(), Prompt: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ErrorSubtype != "" || res.NumTurns != 2 || !res.CostKnown || res.CostUSD != 0.2 || res.ResultText != "half way" {
		t.Fatalf("result: %+v", res)
	}
}

// TestOpenCodeCommandResumes: a request naming a session to resume passes
// it as opencode's --session, and nothing else about the command line
// changes; a fresh request passes no --session.
func TestOpenCodeCommandResumes(t *testing.T) {
	r := newRunner(t, "")
	paths := sessionPaths{dir: t.TempDir(), systemPrompt: "/s/system-prompt.md", mcp: map[string]MCPEntry{"tools": {Command: "task", Args: []string{"mcp", "serve"}}}}
	var got [][]string
	for _, id := range []string{"", "ses_abc"} {
		_, args, stdin, env, err := opencodeBackend{}.command(context.Background(), r.Runner, Request{Name: "n", Profile: opencodeRole("m"), SystemPrompt: "SYS", Prompt: "TASK", ResumeID: id}, paths)
		if err != nil {
			t.Fatal(err)
		}
		if stdin != "TASK" {
			t.Errorf("stdin: %q", stdin)
		}
		if len(env) != 1 || env[0].name != EnvOpenCodeConfig || env[0].value != filepath.Join(paths.dir, OpenCodeConfigFile) {
			t.Errorf("env: %+v", env)
		}
		i := slices.Index(args, "--session")
		if (id == "") != (i < 0) || (i >= 0 && args[i+1] != id) {
			t.Errorf("resume id %q: --session in %q", id, args)
		}
		if slices.Contains(args, "--resume") || slices.Contains(args, "--system-prompt-snapshot") {
			t.Errorf("claude's resume flags reached opencode: %q", args)
		}
		got = append(got, slices.DeleteFunc(args, func(a string) bool { return a == "--session" || a == id }))
	}
	if !slices.Equal(got[0], got[1]) {
		t.Errorf("the resume id changed more than --session:\n%q\n%q", got[0], got[1])
	}
}

// TestOpenCodeServers pins how MCP entries become opencode's mcp table: a
// stdio entry a local server with its executable first in the command
// list, a remote one by url and headers, an empty one left out, and every
// server enabled.
func TestOpenCodeServers(t *testing.T) {
	got := opencodeServers(map[string]MCPEntry{
		"zeta":  {Type: "stdio", Command: "/bin/z", Args: []string{"a b"}, Env: map[string]string{"A": "1"}},
		"alpha": {Type: "http", URL: "https://x.example/mcp", Headers: map[string]string{"Authorization": "Bearer t"}},
		"empty": {},
	})
	if len(got) != 2 {
		t.Fatalf("servers: %+v", got)
	}
	if z := got["zeta"]; z.Type != "local" || !slices.Equal(z.Command, []string{"/bin/z", "a b"}) || z.Environment["A"] != "1" || !z.Enabled {
		t.Errorf("zeta: %+v", z)
	}
	if a := got["alpha"]; a.Type != "remote" || a.URL != "https://x.example/mcp" || a.Headers["Authorization"] != "Bearer t" || !a.Enabled {
		t.Errorf("alpha: %+v", a)
	}
	// What is written is what opencode reads: the command is one list, the
	// type is spelled opencode's way.
	path := filepath.Join(t.TempDir(), OpenCodeConfigFile)
	if err := writeOpenCodeConfig(path, "", map[string]MCPEntry{"zeta": {Command: "/bin/z", Args: []string{"a"}}}, ""); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	for _, want := range []string{`"$schema": "https://opencode.ai/config.json"`, `"type": "local"`, `"command": [`, `"/bin/z",`, `"enabled": true`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("opencode.json lacks %q:\n%s", want, b)
		}
	}
	for _, gone := range []string{"mcpServers", `"stdio"`, `"args"`, "instructions"} {
		if strings.Contains(string(b), gone) {
			t.Errorf("opencode.json carries %q:\n%s", gone, b)
		}
	}
}

// An opencode transcript is its event stream, and the finished steps are
// its turns when no end closed it.
func TestCountTurnsOpenCode(t *testing.T) {
	path := filepath.Join(t.TempDir(), TranscriptFile)
	stream := `{"type":"step_start","sessionID":"s"}
{"type":"tool_use","sessionID":"s","part":{"type":"tool"}}
{"type":"step_finish","sessionID":"s","part":{"reason":"tool-calls"}}
{"type":"text","sessionID":"s","part":{"text":"x"}}
{"type":"step_finish","sessionID":"s","part":{"reason":"tool-calls"}}
`
	if err := os.WriteFile(path, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CountTurns(path); got != 2 {
		t.Errorf("CountTurns on an opencode transcript = %d, want 2", got)
	}
}
