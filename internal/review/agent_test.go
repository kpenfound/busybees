package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/kpenfound/busybees/internal/config"
)

// fakeCLI writes a shell script standing in for claude or codex: it records
// the arguments, the prompt it was given on stdin and the directory it ran
// in, then prints body. No test in this package runs a real agent.
func fakeCLI(t *testing.T, body string) (bin, record string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "agent")
	record = filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = mcp ]; then printf '%s\\n' \"$@\" > " + record + ".mcp-args; pwd > " + record + ".mcp-dir; echo '[]'; exit 0; fi\n" +
		"printf '%s\\n' \"$@\" > " + record + ".args\n" +
		"cat > " + record + ".stdin\n" +
		"pwd > " + record + ".dir\n" +
		body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

func recorded(t *testing.T, record, what string) string {
	t.Helper()
	data, err := os.ReadFile(record + "." + what)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// args returns the arguments the fake CLI was called with, one per line, as
// one string with a separator on both ends of every argument, so a test can
// ask for an exact argument without matching a longer one.
func args(t *testing.T, record string) string {
	t.Helper()
	return "\n" + recorded(t, record, "args")
}

const claudeAnswer = `echo '{"type":"result","subtype":"success","is_error":false,` +
	`"result":"the brief","session_id":"sess-1","num_turns":3,"total_cost_usd":0.5}'`

func TestAClaudeReviewSessionIsReadOnly(t *testing.T) {
	bin, record := fakeCLI(t, claudeAnswer)
	agent := &CLIAgent{ClaudeBin: bin, Model: "opus"}
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	got := args(t, record)
	for _, want := range []string{
		"\n--disallowedTools\nBash,BashOutput,Edit,KillShell,MultiEdit,NotebookEdit,Task,WebFetch,WebSearch,Write\n",
		"\n--allowedTools\nRead,Grep,Glob,LS,NotebookRead\n",
		"\n--permission-prompts\nnone\n",
		"\n--mcp-config\n{\"mcpServers\":{}}\n",
		"\n--strict-mcp-config\n",
		"\n--model\nopus\n",
		"\n--max-turns\n40\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("claude was not given %q:\n%s", want, got)
		}
	}
	// The factory's sessions are given this; a review session never is.
	if strings.Contains(got, "--dangerously-skip-permissions") {
		t.Errorf("a review session skipped permissions:\n%s", got)
	}
	if prompt := recorded(t, record, "stdin"); prompt != "do it" {
		t.Errorf("prompt = %q, want it on stdin", prompt)
	}
}

func TestAClaudeSessionsAnswerIsRead(t *testing.T) {
	bin, _ := fakeCLI(t, claudeAnswer)
	agent := &CLIAgent{Provider: config.AgentClaude, ClaudeBin: bin}
	res, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "the brief" || res.ID != "sess-1" || res.Turns != 3 || res.CostUSD != 0.5 || !res.CostKnown {
		t.Errorf("result = %+v", res)
	}
}

func TestAFailedClaudeSessionIsAnError(t *testing.T) {
	for _, tc := range []struct{ name, answer, want string }{
		{"with a subtype", `{"type":"result","subtype":"error_max_turns","is_error":true,"result":"gave up"}`, "error_max_turns: gave up"},
		{"without one", `{"type":"result","is_error":true,"result":"gave up"}`, "failed: gave up"},
		{"without a word about it", `{"type":"result","subtype":"error_max_turns","is_error":true}`, "session: error_max_turns"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := fakeCLI(t, "echo '"+tc.answer+"'")
			agent := &CLIAgent{ClaudeBin: bin}
			_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
			if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want the failure the session reported (%s)", err, tc.want)
			}
		})
	}
}

func TestASessionThatPrintedNoResultIsAnError(t *testing.T) {
	bin, _ := fakeCLI(t, "echo 'not json'\necho 'claude: bad flag' >&2\nexit 2")
	agent := &CLIAgent{ClaudeBin: bin}
	_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "distiller session") {
		t.Fatalf("err = %v, want the session named", err)
	}
	if !strings.Contains(err.Error(), "claude: bad flag") {
		t.Errorf("err = %v, want what the CLI printed on stderr", err)
	}
}

func TestASessionThatFailedWithAResultReportsTheResult(t *testing.T) {
	// The exit code is not what says a session failed: claude reports the
	// failure in its result and can still exit 0.
	bin, _ := fakeCLI(t, `echo '{"is_error":true,"subtype":"error_during_execution","result":"no capacity"}'`)
	agent := &CLIAgent{ClaudeBin: bin}
	_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no capacity") {
		t.Fatalf("err = %v, want what the session said", err)
	}
}

func TestAClaudeSessionIsResumedByItsID(t *testing.T) {
	bin, record := fakeCLI(t, claudeAnswer)
	agent := &CLIAgent{ClaudeBin: bin}
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "angle", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if got := args(t, record); strings.Contains(got, "--resume") {
		t.Errorf("a fresh session was resumed:\n%s", got)
	}
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "angle", Dir: t.TempDir(), ResumeID: "sess-1"}); err != nil {
		t.Fatal(err)
	}
	if got := args(t, record); !strings.Contains(got, "\n--resume\nsess-1\n") {
		t.Errorf("session not resumed:\n%s", got)
	}
}

func TestTheSessionRunsInTheDirectoryItWasGiven(t *testing.T) {
	bin, record := fakeCLI(t, claudeAnswer)
	dir := t.TempDir()
	agent := &CLIAgent{ClaudeBin: bin}
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: dir}); err != nil {
		t.Fatal(err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(recorded(t, record, "dir")))
	if err != nil {
		t.Fatal(err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Errorf("ran in %s, want %s", got, want)
	}
}

func TestAModelTheConfigurationLeavesOutIsTheCLIsOwn(t *testing.T) {
	for _, tc := range []struct{ provider, answer string }{
		{config.AgentClaude, claudeAnswer},
		{config.AgentCodex, codexAnswer},
	} {
		t.Run(tc.provider, func(t *testing.T) {
			bin, record := fakeCLI(t, tc.answer)
			agent := &CLIAgent{Provider: tc.provider, ClaudeBin: bin, CodexBin: bin}
			if _, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			if got := args(t, record); strings.Contains(got, "--model") {
				t.Errorf("a model was chosen:\n%s", got)
			}
		})
	}
}

func TestACLIThatCouldNotBeRunIsAnError(t *testing.T) {
	agent := &CLIAgent{ClaudeBin: filepath.Join(t.TempDir(), "no-such-claude")}
	_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "no-such-claude") {
		t.Fatalf("err = %v, want the executable that could not be run", err)
	}
}

const codexAnswer = `echo '{"type":"thread.started","thread_id":"thread-9"}'
echo '{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"the brief"}}'
echo '{"type":"item.completed","item":{"type":"reasoning","text":"that will do"}}'
echo '{"type":"turn.completed"}'`

func TestACodexReviewSessionRunsInItsReadOnlySandbox(t *testing.T) {
	bin, record := fakeCLI(t, codexAnswer)
	agent := &CLIAgent{Provider: config.AgentCodex, CodexBin: bin, Model: "gpt-5"}
	res, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir(), ResumeID: "thread-1"})
	if err != nil {
		t.Fatal(err)
	}
	got := args(t, record)
	for _, want := range []string{"\nexec\n", "\n--json\n", "\n--sandbox\nread-only\n", "\n--model\ngpt-5\n", "\n-\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("codex was not given %q:\n%s", want, got)
		}
	}
	// Codex has no resume, and the id of a claude session is not one of its
	// threads: it starts a session instead of failing on the flag.
	if strings.Contains(got, "--resume") {
		t.Errorf("codex was asked to resume:\n%s", got)
	}
	if res.Text != "the brief" || res.ID != "thread-9" || res.Turns != 3 || res.CostUSD != 0 || res.CostKnown {
		t.Errorf("result = %+v", res)
	}
}

func TestAFailedCodexTurnIsAnError(t *testing.T) {
	// A failed turn says why under "error", a codex that gave up on the
	// session says it in the event itself.
	for _, tc := range []struct{ name, event, want string }{
		{"a failed turn", `{"type":"turn.failed","error":{"message":"context window"}}`, "turn_failed: context window"},
		{"an error", `{"type":"error","message":"no capacity"}`, "error: no capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := fakeCLI(t, "echo '{\"type\":\"thread.started\",\"thread_id\":\"t\"}'\necho '"+tc.event+"'")
			agent := &CLIAgent{Provider: config.AgentCodex, CodexBin: bin}
			_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
			if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestACodexStreamThatNeverEndsTheTurnIsAnError(t *testing.T) {
	bin, _ := fakeCLI(t, `echo '{"type":"thread.started","thread_id":"t"}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"half a brief"}}'`)
	agent := &CLIAgent{Provider: config.AgentCodex, CodexBin: bin}
	_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "without finishing the turn") {
		t.Fatalf("err = %v, want an unfinished turn", err)
	}
}

func TestAnUnknownProviderIsRefused(t *testing.T) {
	agent := &CLIAgent{Provider: "gemini"}
	_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller"})
	if err == nil || !strings.Contains(err.Error(), `"gemini"`) || !strings.Contains(err.Error(), "claude, codex") {
		t.Fatalf("err = %v, want the provider and the ones there are", err)
	}
}

func TestASessionIsBoundedByItsTimeout(t *testing.T) {
	bin, _ := fakeCLI(t, "sleep 5")
	agent := &CLIAgent{ClaudeBin: bin, Timeout: 50 * time.Millisecond}
	start := time.Now()
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()}); err == nil {
		t.Fatal("a session that ran past its timeout succeeded")
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Errorf("the session ran for %s, want it stopped at its timeout", elapsed)
	}
}

func TestTheAgentIsTheConfiguredProviderAndModel(t *testing.T) {
	cfg, err := ParseConfig("provider = \"codex\"\nmodel = \"gpt-5\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if agent := NewAgent(cfg); agent.Provider != "codex" || agent.Model != "gpt-5" {
		t.Errorf("agent = %+v, want the configured provider and model", agent)
	}
	if agent := NewAgent(nil); agent.Provider != DefaultProvider || agent.Model != DefaultModel {
		t.Errorf("agent = %+v, want the defaults", agent)
	}
}

// The session runs in the caller's environment with Env on top of it: a
// role's own variables from bees.toml when the factory runs the review.
func TestTheSessionIsGivenTheAgentsEnvironment(t *testing.T) {
	envFile := filepath.Join(t.TempDir(), "env")
	bin, _ := fakeCLI(t, "env > "+envFile+"\n"+claudeAnswer)
	t.Setenv("REVIEW_TEST_INHERITED", "from the caller")
	agent := &CLIAgent{ClaudeBin: bin, Env: map[string]string{"REVIEW_TEST_ROLE": "from the role", "REVIEW_TEST_INHERITED": "from the role too"}}
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	got := "\n" + string(env)
	if !strings.Contains(got, "\nREVIEW_TEST_ROLE=from the role\n") {
		t.Errorf("the role's variable did not reach the session:\n%s", env)
	}
	// A variable the role sets wins over the caller's own, as it does for a
	// factory session.
	if !strings.Contains(got, "\nREVIEW_TEST_INHERITED=from the role too\n") {
		t.Errorf("the role's value of an inherited variable did not win:\n%s", env)
	}
	// Nothing is set on a session whose agent has no Env.
	envFile = filepath.Join(t.TempDir(), "env")
	bin, _ = fakeCLI(t, "env > "+envFile+"\n"+claudeAnswer)
	if _, err := (&CLIAgent{ClaudeBin: bin}).Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	env, err = os.ReadFile(envFile)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(env), "REVIEW_TEST_ROLE") || !strings.Contains(string(env), "REVIEW_TEST_INHERITED=from the caller") {
		t.Errorf("an agent with no Env changed the environment:\n%s", env)
	}
}

func TestReviewExecutionSettingsAndSafety(t *testing.T) {
	for _, provider := range []string{config.AgentClaude, config.AgentCodex} {
		t.Run(provider, func(t *testing.T) {
			answer := claudeAnswer
			if provider == config.AgentCodex {
				answer = codexAnswer
			}
			bin, record := fakeCLI(t, answer)
			a := &CLIAgent{Provider: provider, ClaudeBin: bin, CodexBin: bin, Model: "chosen", FallbackModel: "fallback", Effort: "max"}
			if _, err := a.Run(context.Background(), AgentRequest{Dir: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			got := args(t, record)
			wants := []string{"\n--model\nchosen\n"}
			if provider == config.AgentClaude {
				wants = append(wants, "\n--fallback-model\nfallback\n", "\n--effort\nmax\n", "\n--tools\nRead,Grep,Glob,LS,NotebookRead\n", "\n--setting-sources\n\n")
			} else {
				wants = append(wants, "\nmodel_reasoning_effort=\"high\"\n", "\nfeatures.shell_tool=false\n", "\nfeatures.plugins=false\n", "\nweb_search=\"disabled\"\n", "\n--sandbox\nread-only\n")
				if strings.Contains(got, "--fallback-model") {
					t.Fatal("Codex received Claude fallback flag")
				}
			}
			for _, want := range wants {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %s", want, got)
				}
			}
		})
	}
	env := reviewEnvironment([]string{"BEES_ROLE=reviewer", "BEES_SESSION_DIR=/session", "GIT_DIR=/shared", "GIT_CONFIG_COUNT=1", "KEEP=yes"}, map[string]string{"BEES_REPO": "secret", "GIT_WORK_TREE": "/repo", "ROLE_ENV": "ok"})
	if strings.Join(env, " ") != "KEEP=yes ROLE_ENV=ok" {
		t.Fatalf("identity leaked: %v", env)
	}
}

func TestCodexReviewDisablesInheritedMCP(t *testing.T) {
	bin, record := fakeCLI(t, codexAnswer)
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	// Dynamically supplied servers disappear only when the inventory receives
	// the same feature restrictions as the review session.
	inventory := `case " $* " in
  *" features.plugins=false "*) echo '[{"name":"bees"},{"name":"server.with.dots"},{"name":"server \"quoted\""}]' ;;
  *) echo '[{"name":"codex_app"}]' ;;
esac`
	script := strings.Replace(string(data), "echo '[]'", inventory, 1)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	a := &CLIAgent{Provider: config.AgentCodex, CodexBin: bin, Model: "chosen", Effort: "medium"}
	dir := t.TempDir()
	if _, err := a.Run(context.Background(), AgentRequest{Dir: dir}); err != nil {
		t.Fatal(err)
	}
	probeArgs := "\n" + recorded(t, record, "mcp-args")
	wantDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(recorded(t, record, "mcp-dir")); got != wantDir {
		t.Errorf("inventory directory = %q, want %q", got, wantDir)
	}
	for _, want := range []string{
		`orchestrator.mcp.enabled=false`, `features.apps=false`, `features.plugins=false`,
		`features.shell_tool=false`, `features.unified_exec=false`, `features.js_repl=false`,
		`web_search="disabled"`, `approval_policy="never"`,
	} {
		if !strings.Contains(probeArgs, "\n"+want+"\n") || !strings.Contains(args(t, record), "\n"+want+"\n") {
			t.Errorf("inventory and session must both receive %s", want)
		}
	}
	// Codex splits override paths on dots without interpreting quotes. Keep
	// server names inside the TOML value so their punctuation stays literal.
	var overrides struct {
		Servers map[string]struct {
			Enabled *bool `toml:"enabled"`
		} `toml:"mcp_servers"`
	}
	for _, arg := range strings.Split(recorded(t, record, "args"), "\n") {
		key, _, _ := strings.Cut(arg, "=")
		if strings.HasPrefix(key, "mcp_servers.") {
			t.Fatalf("server name placed in a dotted override path: %s", arg)
		}
		if key == "mcp_servers" {
			if _, err := toml.Decode(arg, &overrides); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(overrides.Servers) != 3 {
		t.Fatalf("disabled servers = %v, want the three configured servers", overrides.Servers)
	}
	for _, name := range []string{"bees", "server.with.dots", `server "quoted"`} {
		if server := overrides.Servers[name]; server.Enabled == nil || *server.Enabled {
			t.Errorf("server %q was not disabled", name)
		}
	}
}

func TestCodexReviewMCPInventoryFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, response, want string }{
		{"failed command", "exit 1", "list codex MCP"},
		{"invalid JSON", "echo invalid", "decode codex MCP"},
		{"null", "echo null", "must be an array"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, record := fakeCLI(t, codexAnswer)
			data, err := os.ReadFile(bin)
			if err != nil {
				t.Fatal(err)
			}
			script := strings.Replace(string(data), "echo '[]'", tc.response, 1)
			if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
				t.Fatal(err)
			}
			a := &CLIAgent{Provider: config.AgentCodex, CodexBin: bin}
			if _, err := a.Run(context.Background(), AgentRequest{Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("did not fail closed: %v", err)
			}
			if _, err := os.Stat(record + ".args"); !os.IsNotExist(err) {
				t.Fatalf("session ran after inventory failure: %v", err)
			}
		})
	}
}
