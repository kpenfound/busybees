package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/vcs"
)

// fakePi writes a shell script standing in for the pi binary.
func fakePi(t *testing.T, body string) string { return agenttest.Script(t, "pi", body) }

// piRole is a role resolved with agent = "pi".
func piRole(model string) Profile {
	return Profile{Name: "builder", Agent: AgentPi, Model: model, MaxTurns: 10, Timeout: time.Minute}
}

// piArgs reads the arguments a fake pi recorded, one a line.
func piArgs(t *testing.T, dir string) []string {
	t.Helper()
	return lines(t, filepath.Join(dir, "args.txt"))
}

// flagValue is the argument after flag, or "" when flag is not there.
func flagValue(args []string, flag string) string {
	if i := slices.Index(args, flag); i >= 0 && i+1 < len(args) {
		return args[i+1]
	}
	return ""
}

// extensions are the -e arguments of a command line, in order.
func extensions(args []string) []string {
	var out []string
	for i, a := range args {
		if a == "-e" && i+1 < len(args) {
			out = append(out, args[i+1])
		}
	}
	return out
}

// TestPiRunSuccess covers a session of a role whose agent is pi: the runner
// starts `pi -p --mode json` instead of `claude -p`, the task on stdin and
// the system prompt as an appended file, loads pi-mcp-adapter and the
// role's pi packages with no other extension, and hands the adapter the
// session's MCP servers, the built-in one included, in a file of the
// session's own; it reads the session id, the last text, the turns and the
// summed cost off pi's event stream.
func TestPiRunSuccess(t *testing.T) {
	bin := fakePi(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
cat > "$TASK_SESSION_DIR/stdin.txt"
env > "$TASK_SESSION_DIR/env.txt"
echo '{"type":"session","version":3,"id":"pi-ses-1","timestamp":"2026-09-19T00:00:00Z","cwd":"/w"}'
echo '{"type":"agent_start"}'
echo '{"type":"turn_start"}'
echo '{"type":"message_start","message":{"role":"assistant","content":[]}}'
echo '{"type":"message_update","usage":{},"assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"half"}}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"half way"},{"type":"toolCall","id":"c1","name":"bash","arguments":{"command":"ls"}}],"stopReason":"toolUse","usage":{"cost":{"total":0.25}}}}'
echo '{"type":"tool_execution_end","toolCallId":"c1","toolName":"bash","result":{},"isError":false}'
echo '{"type":"message_end","message":{"role":"toolResult","toolCallId":"c1","toolName":"bash","content":[{"type":"text","text":"a.go"}],"isError":false}}'
echo '{"type":"turn_end","message":{"role":"assistant"},"toolResults":[]}'
echo '{"type":"turn_start"}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"text","text":"all"},{"type":"text","text":"done"}],"stopReason":"stop","usage":{"cost":{"total":0.75}}}}'
echo '{"type":"turn_end","message":{"role":"assistant"},"toolResults":[]}'
echo '{"type":"agent_end","messages":[]}'
printf '{"status":"submitted","work":{"key":"task/12","tags":{"ticket":"twelve"}},"note":"hi"}' > "$TASK_SESSION_DIR/outcome.json"
`)
	r := newRunner(t, "")
	r.PiBin = bin
	role := piRole("anthropic/claude-sonnet-5")
	role.Fallback = &Profile{Agent: AgentClaude, Model: "sonnet"}
	role.Effort = "high"
	role.AllowedTools = []string{"Bash"}
	role.PiPackages = []string{"npm:@acme/pi-tools@1.2.3", "git:github.com/acme/pi-extras"}
	role.MCP = map[string]MCPEntry{
		"x":      {Command: "srv", Args: []string{"--port", "1"}, Env: map[string]string{"K": "$HOME"}},
		"remote": {URL: "https://x.example/mcp", Headers: map[string]string{"Authorization": "Bearer t"}},
	}
	res, err := r.Run(context.Background(), Request{Name: "p1", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, SystemPrompt: "SYS", Prompt: "TASK", Env: map[string]string{EnvIssue: "12"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "all\ndone" || res.NumTurns != 2 || res.ClaudeID != "pi-ses-1" || res.ExitCode != 0 {
		t.Fatalf("result: %+v", res)
	}
	if !res.CostKnown || res.CostUSD != 1 {
		t.Errorf("cost: known %v, %v; want the responses' sum, 1", res.CostKnown, res.CostUSD)
	}
	if !res.HasOutcome || res.Outcome.Status != "submitted" || res.Outcome.Work.Key != "task/12" {
		t.Fatalf("outcome: %+v", res.Outcome)
	}
	dir := res.SessionDir
	args := piArgs(t, dir)
	if head := strings.Join(args[:4], " "); head != "-p --mode json --no-extensions" {
		t.Errorf("args start %q", head)
	}
	// The adapter first, then the role's packages in order, and nothing else.
	if got, want := extensions(args), []string{PiMCPAdapter, "npm:@acme/pi-tools@1.2.3", "git:github.com/acme/pi-extras"}; !slices.Equal(got, want) {
		t.Errorf("-e %q, want %q", got, want)
	}
	configPath := filepath.Join(dir, PiMCPConfigFile)
	for flag, want := range map[string]string{
		"--mcp-config":           configPath,
		"--name":                 "task-p1",
		"--append-system-prompt": filepath.Join(dir, "system-prompt.md"),
		"--model":                "anthropic/claude-sonnet-5",
		"--thinking":             "high",
	} {
		if got := flagValue(args, flag); got != want {
			t.Errorf("%s %q, want %q:\n%q", flag, got, want, args)
		}
	}
	// The task is on stdin alone, not on the command line.
	if slices.Contains(args, "TASK") || slices.Contains(args, "SYS") {
		t.Errorf("a prompt is on the command line: %q", args)
	}
	if stdin, _ := os.ReadFile(filepath.Join(dir, "stdin.txt")); string(stdin) != "TASK" {
		t.Errorf("stdin: %q", stdin)
	}
	// Nothing of the other agents' command lines leaks into pi's.
	for _, gone := range []string{"--dangerously-skip-permissions", "--append-system-prompt-file", "--max-turns", "--fallback-model", "--effort", "--allowedTools", "--tools", "--strict-mcp-config", "--add-dir", "--session-id", "--resume", "--output-format", "run", "exec", "--auto"} {
		if slices.Contains(args, gone) {
			t.Errorf("args carry %s: %q", gone, args)
		}
	}
	for _, other := range []string{"mcp.json", OpenCodeConfigFile} {
		if _, err := os.Stat(filepath.Join(dir, other)); err == nil {
			t.Errorf("%s written for a pi session, which never reads it", other)
		}
	}
	env, _ := os.ReadFile(filepath.Join(dir, "env.txt"))
	for _, want := range []string{EnvPiMCPConfigMode + "=exclusive", "TASK_ROLE=builder", "TASK_ISSUE=12"} {
		if !strings.Contains(string(env), want+"\n") {
			t.Errorf("env missing %s", want)
		}
	}
	// The adapter's configuration names the built-in server by the command
	// that serves this session's role, and the role's own servers.
	var cfg piMCPConfig
	b, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%s: %v\n%s", PiMCPConfigFile, err, b)
	}
	if len(cfg.MCPServers) != 3 {
		t.Errorf("%s names %d servers, want 3:\n%s", PiMCPConfigFile, len(cfg.MCPServers), b)
	}
	tools := cfg.MCPServers["tools"]
	if tools.Command != "/usr/local/bin/task" || !slices.Equal(tools.Args, []string{"mcp", "serve"}) || tools.URL != "" {
		t.Errorf("built-in server: %+v", tools)
	}
	for k, want := range map[string]string{"TASK_ROLE": "builder", "TASK_SESSION_DIR": dir, "TASK_ISSUE": "12"} {
		if tools.Env[k] != want {
			t.Errorf("built-in server env %s = %q, want %q", k, tools.Env[k], want)
		}
	}
	if x := cfg.MCPServers["x"]; x.Command != "srv" || !slices.Equal(x.Args, []string{"--port", "1"}) || x.Env["K"] != os.Getenv("HOME") {
		t.Errorf("server x: %+v", x)
	}
	if remote := cfg.MCPServers["remote"]; remote.URL != "https://x.example/mcp" || remote.Headers["Authorization"] != "Bearer t" || remote.Command != "" {
		t.Errorf("server remote: %+v", remote)
	}
	rb, _ := os.ReadFile(filepath.Join(dir, ResultFile))
	var got Result
	if err := json.Unmarshal(rb, &got); err != nil {
		t.Fatal(err)
	}
	if !got.CostKnown || got.CostUSD != 1 || got.ClaudeID != "pi-ses-1" || got.NumTurns != 2 {
		t.Errorf("result.json: %+v", got)
	}
}

// TestPiMCPConfigContent pins the adapter configuration a session's
// built-in server is written into, byte for byte: the command that serves
// the session's role, connected at startup with its tools registered as
// pi tools of their own.
func TestPiMCPConfigContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), PiMCPConfigFile)
	err := writePiMCPConfig(path, map[string]MCPEntry{"bees": {
		Command: "/usr/local/bin/bees", Args: []string{"mcp", "serve"},
		Env: map[string]string{"BEES_ROLE": "developer", "BEES_SESSION_DIR": "/state/sessions/s1"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	want := `{
  "mcpServers": {
    "bees": {
      "command": "/usr/local/bin/bees",
      "args": [
        "mcp",
        "serve"
      ],
      "env": {
        "BEES_ROLE": "developer",
        "BEES_SESSION_DIR": "/state/sessions/s1"
      },
      "lifecycle": "eager",
      "directTools": true
    }
  }
}`
	if string(b) != want {
		t.Errorf("%s:\n%s\nwant:\n%s", PiMCPConfigFile, b, want)
	}
}

// TestPiServers pins how MCP entries become the adapter's mcpServers: a
// remote server's bearer token by reference to its variable and never by
// value, a value starting with "!" escaped so the adapter does not run it,
// and an entry with neither command nor url left out.
func TestPiServers(t *testing.T) {
	got := piServers(map[string]MCPEntry{
		"zeta":  {Type: "stdio", Command: "/bin/z", Args: []string{"a b"}, Env: map[string]string{"A": "1", "BANG": "!rm -rf /"}},
		"alpha": {Type: "http", URL: "http://host.docker.internal:1/mcp", Headers: map[string]string{"X": "!y"}, BearerTokenEnv: "TOKEN"},
		"empty": {},
	})
	if len(got) != 2 {
		t.Fatalf("servers: %+v", got)
	}
	if z := got["zeta"]; z.Command != "/bin/z" || !slices.Equal(z.Args, []string{"a b"}) || z.Env["A"] != "1" || z.Env["BANG"] != "!!rm -rf /" || z.Lifecycle != "eager" || !z.DirectTools {
		t.Errorf("zeta: %+v", z)
	}
	a := got["alpha"]
	if a.URL != "http://host.docker.internal:1/mcp" || a.Headers["Authorization"] != "Bearer ${TOKEN}" || a.Headers["X"] != "!!y" || a.Command != "" || !a.DirectTools {
		t.Errorf("alpha: %+v", a)
	}
}

// A pi role with no model, no effort and no system prompt leaves each to
// pi: no --model, no --thinking, no --append-system-prompt. The adapter is
// loaded with no pi packages configured, and a zero cost is a known cost.
func TestPiLeavesTheModelToPiWhenUnset(t *testing.T) {
	bin := fakePi(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
echo '{"type":"session","id":"pi-ses-2"}'
echo '{"type":"message_end","message":{"role":"assistant","content":[],"stopReason":"stop","usage":{"cost":{"total":0}}}}'
echo '{"type":"turn_end"}'
`)
	r := newRunner(t, "")
	r.PiBin = bin
	res, err := r.Run(context.Background(), Request{Name: "p2", Profile: piRole(""), Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	args := piArgs(t, res.SessionDir)
	for _, gone := range []string{"--model", "--thinking", "--append-system-prompt"} {
		if slices.Contains(args, gone) {
			t.Errorf("%s passed for a role with none: %q", gone, args)
		}
	}
	if got := extensions(args); !slices.Equal(got, []string{PiMCPAdapter}) {
		t.Errorf("-e %q, want the adapter alone", got)
	}
	if res.IsError || res.NumTurns != 1 || res.ResultText != "" || !res.CostKnown || res.CostUSD != 0 || res.ClaudeID != "pi-ses-2" {
		t.Errorf("result: %+v", res)
	}
}

// TestPiFailedRun: an assistant message that stopped with an error is a
// run that did not finish, whose message is the result text, even when
// the process then exits cleanly; a message naming the usage limit reads as
// the account limit; a response cut off or aborted is a failure named after
// its reason; and an error pi retried past is not the end of the run.
func TestPiFailedRun(t *testing.T) {
	msg := func(stop, text, errMsg string) string {
		return `echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"` + text + `"}],"stopReason":"` + stop + `","errorMessage":"` + errMsg + `","usage":{"cost":{"total":0.1}}}}'` + "\n"
	}
	for _, tc := range []struct {
		name, events, subtype, text string
		exit                        int
		limited, failed             bool
	}{
		{name: "error, clean exit", events: msg("error", "", "stream disconnected"), subtype: "error", text: "stream disconnected", failed: true},
		{name: "error with no message", events: msg("error", "trying", ""), subtype: "error", text: "trying", exit: 1, failed: true},
		{name: "rate limit", events: msg("error", "", "You have hit your usage limit. Try again at 3pm."), subtype: "error", text: "You have hit your usage limit. Try again at 3pm.", exit: 1, limited: true, failed: true},
		{name: "cut off", events: msg("length", "cut off", ""), subtype: "stop_length", text: "cut off", failed: true},
		{name: "aborted", events: msg("aborted", "", ""), subtype: "stop_aborted", failed: true},
		{name: "retried", events: msg("error", "", "overloaded") + msg("stop", "recovered", ""), text: "recovered"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin := fakePi(t, `echo '{"type":"session","id":"pi-ses-3"}'`+"\n"+tc.events+"exit "+strconv.Itoa(tc.exit)+"\n")
			r := newRunner(t, "")
			r.PiBin = bin
			res, err := r.Run(context.Background(), Request{Name: "p3", Profile: piRole(""), Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK"})
			if err != nil {
				t.Fatal(err)
			}
			if res.IsError != tc.failed || res.ErrorSubtype != tc.subtype || res.ResultText != tc.text || res.ExitCode != tc.exit || res.ClaudeID != "pi-ses-3" {
				t.Fatalf("result: %+v", res)
			}
			if res.RateLimit != nil {
				t.Errorf("a pi session reported a rate-limit event: %+v", res.RateLimit)
			}
			if _, limited := res.SessionLimited(); limited != tc.limited {
				t.Errorf("SessionLimited = %v, want %v", limited, tc.limited)
			}
		})
	}
}

// A pi stream that ends with no assistant message that stopped — the
// process was killed, or it crashed — is a session that never said how it
// went: no_result, and the turns counted from the transcript.
func TestPiStreamWithoutAnEnd(t *testing.T) {
	bin := fakePi(t, `
echo '{"type":"session","id":"pi-ses-4"}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"half way"}],"stopReason":"toolUse","usage":{"cost":{"total":0.1}}}}'
echo '{"type":"turn_end"}'
echo '{"type":"turn_end"}'
`)
	r := newRunner(t, "")
	r.PiBin = bin
	res, err := r.Run(context.Background(), Request{Name: "p4", Profile: piRole(""), Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || res.ErrorSubtype != "no_result" || res.NumTurns != 2 || res.CostKnown {
		t.Fatalf("result: %+v", res)
	}
}

// TestPiCommandResumes: a request naming a session to resume passes it as
// pi's --session-id, and nothing else about the command line changes; a
// fresh request passes none.
func TestPiCommandResumes(t *testing.T) {
	r := newRunner(t, "")
	paths := sessionPaths{dir: t.TempDir(), systemPrompt: "/s/system-prompt.md", mcp: map[string]MCPEntry{"tools": {Command: "task", Args: []string{"mcp", "serve"}}}}
	var got [][]string
	for _, id := range []string{"", "pi-ses-abc"} {
		_, args, stdin, env, err := piBackend{}.command(context.Background(), r.Runner, backendNamed(t, AgentPi), Request{Name: "n", Profile: piRole("m"), SystemPrompt: "SYS", Prompt: "TASK", ResumeID: id}, paths)
		if err != nil {
			t.Fatal(err)
		}
		if stdin != "TASK" {
			t.Errorf("stdin: %q", stdin)
		}
		if len(env) != 1 || env[0].name != EnvPiMCPConfigMode || env[0].value != "exclusive" {
			t.Errorf("env: %+v", env)
		}
		if flagValue(args, "--session-id") != id {
			t.Errorf("resume id %q: --session-id in %q", id, args)
		}
		for _, gone := range []string{"--resume", "--session", "--continue", "--fork"} {
			if slices.Contains(args, gone) {
				t.Errorf("%s reached pi: %q", gone, args)
			}
		}
		got = append(got, slices.DeleteFunc(args, func(a string) bool { return a == "--session-id" || a == id }))
	}
	if !slices.Equal(got[0], got[1]) {
		t.Errorf("the resume id changed more than --session-id:\n%q\n%q", got[0], got[1])
	}
}

// A pi transcript is its event stream, and its ended turns are its turns
// when no end closed it.
func TestCountTurnsPi(t *testing.T) {
	path := filepath.Join(t.TempDir(), TranscriptFile)
	stream := `{"type":"session","id":"s"}
{"type":"turn_start"}
{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse"}}
{"type":"message_end","message":{"role":"toolResult"}}
{"type":"turn_end"}
{"type":"turn_start"}
{"type":"turn_end"}
{"type":"turn_start"}
`
	if err := os.WriteFile(path, []byte(stream), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := CountTurns(path); got != 2 {
		t.Errorf("CountTurns on a pi transcript = %d, want 2", got)
	}
}

// A pi session in the container sandbox runs the ordinary pi command inside
// the engine: the built-in server stays on the host and is reached over
// HTTP, with the session's token referred to by its variable in the
// adapter's configuration, and the adapter's mode is handed to the
// container by name like the session's own variables.
func TestPiContainerSession(t *testing.T) {
	t.Setenv("OPENROUTER_API_KEY", "sk-or")
	fakeContainerHost(t, "darwin")
	bin := fakePi(t, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
cat > /dev/null
echo '{"type":"session","id":"pi-boxed"}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"boxed"}],"stopReason":"stop","usage":{"cost":{"total":0.1}}}}'
`)
	metadata, worktree := workspaceFixture(t)
	r := newRunner(t, "")
	r.PiBin = bin
	r.DockerBin = fakeDocker(t, "ghcr.io/acme/pi:1")
	r.ServerBin = fakeBees(t)
	r.StateDir = t.TempDir()
	r.ContainerListen = "127.0.0.1:0"
	role := piRole("openrouter/x")
	role.Sandbox, role.SandboxImage = SandboxContainer, "ghcr.io/acme/pi:1"
	res, err := r.Run(context.Background(), Request{Name: "boxed", Profile: role, Workspace: fakeWorkspace{dir: worktree, access: &vcs.Access{Mounts: []string{metadata}}}, SystemPrompt: "SYS", Prompt: "TASK"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || res.ResultText != "boxed" || res.ClaudeID != "pi-boxed" {
		t.Fatalf("result: %+v", res)
	}
	dir := res.SessionDir
	docker := strings.Join(lines(t, filepath.Join(dir, "docker-args.txt")), " ") + " "
	for _, want := range []string{"--env " + EnvPiMCPConfigMode + " ", "--env OPENROUTER_API_KEY ", " ghcr.io/acme/pi:1 " + bin + " -p --mode json "} {
		if !strings.Contains(docker, want) {
			t.Errorf("docker args missing %q:\n%s", want, docker)
		}
	}
	if !slices.Contains(lines(t, filepath.Join(dir, "docker-env.txt")), EnvPiMCPConfigMode+"=exclusive") {
		t.Errorf("the engine client was not given %s", EnvPiMCPConfigMode)
	}
	var cfg piMCPConfig
	b, _ := os.ReadFile(filepath.Join(dir, PiMCPConfigFile))
	if err := json.Unmarshal(b, &cfg); err != nil {
		t.Fatalf("%v\n%s", err, b)
	}
	tools := cfg.MCPServers["tools"]
	if tools.URL != "http://host.docker.internal:45678/mcp" || tools.Command != "" || tools.Headers["Authorization"] != "Bearer ${"+EnvMCPToken+"}" {
		t.Errorf("built-in server entry: %+v", tools)
	}
	var token string
	for _, kv := range lines(t, filepath.Join(dir, "docker-env.txt")) {
		if value, ok := strings.CutPrefix(kv, EnvMCPToken+"="); ok {
			token = value
		}
	}
	if token == "" {
		t.Fatal("the container was given no bearer token")
	}
	if strings.Contains(string(b), token) {
		t.Errorf("%s carries the bearer token itself:\n%s", PiMCPConfigFile, b)
	}
	if got := flagValue(piArgs(t, dir), "--mcp-config"); got != filepath.Join(dir, PiMCPConfigFile) {
		t.Errorf("--mcp-config %q", got)
	}
}

// Pi runs under no sandbox of its own, so claude's is refused for it before
// anything starts, and a pi turn that restricts built-in tools is refused as
// it is for codex and opencode.
func TestPiRefusals(t *testing.T) {
	boxed := piRole("")
	boxed.Sandbox = SandboxClaude
	if err := boxed.Validate(); err == nil || !strings.Contains(err.Error(), `"pi"`) {
		t.Errorf("pi under claude's sandbox: %v", err)
	}
	for _, mode := range []string{"", SandboxNone} {
		p := piRole("")
		p.Sandbox = mode
		if err := p.Validate(); err != nil {
			t.Errorf("pi with sandbox %q: %v", mode, err)
		}
	}
	r := &Runner{PiBin: fakePi(t, "exit 0\n"), SessionsDir: t.TempDir()}
	req := grantAll(Request{Name: "p5", Profile: piRole(""), Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK"})
	req.Grants.Tools = []string{"Read"}
	if _, err := r.Run(context.Background(), req); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), `"pi"`) {
		t.Errorf("a pi turn restricting built-in tools: %v", err)
	}
}

// TestARealPiNeverRunsFromATestBinary: a runner pointed at a pi no test made
// refuses before anything starts, the guard a forgotten fake falls to.
func TestARealPiNeverRunsFromATestBinary(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	r := newRunner(t, "")
	r.PiBin = sh
	_, err = r.Run(context.Background(), Request{Name: "real", Profile: piRole(""), Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK"})
	if !errors.Is(err, agentbin.ErrRealAgent) {
		t.Fatalf("err = %v, want ErrRealAgent", err)
	}
	entries, _ := os.ReadDir(r.SessionsDir)
	for _, e := range entries {
		if _, err := os.Stat(filepath.Join(r.SessionsDir, e.Name(), TranscriptFile)); err == nil {
			t.Error("a transcript written for a session refused to start")
		}
	}
}
