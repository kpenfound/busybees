package review

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agentbin"
	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/internal/config"
)

// fakeCLI writes a shell script standing in for a review session's agent: it
// records the arguments, the prompt it was given on stdin, the directory it
// ran in and its environment, then prints body. The probes the shared
// restricted execution runs before an agent — codex's `mcp list` and
// opencode's search for custom tools and `debug config` — are answered with
// what a CLI honoring the inline restrictions would say: no custom tool,
// nothing inherited, everything disabled. No
// test in this package runs a real agent.
func fakeCLI(t *testing.T, body string) (bin, record string) {
	t.Helper()
	return fakeScript(t, honestProbe, body)
}

const honestProbe = `if [ "$BUN_BE_BUN" = 1 ]; then echo '` + agenttest.OpenCodeNoCustomTools + `'; exit 0; fi
if [ "$1" = mcp ]; then printf '%s\n' "$@" > RECORD.mcp-args; pwd > "RECORD.mcp-dir"; echo '[]'; exit 0; fi
if [ "$2" = debug ]; then
  if printf '%s' "$OPENCODE_CONFIG_CONTENT" | grep -q '"legacy"' && printf '%s' "$OPENCODE_CONFIG_CONTENT" | grep -q '"enabled":false'; then
    enabled=false
  else
    enabled=true
  fi
  printf '%s\n' "$OPENCODE_CONFIG_CONTENT" > "RECORD.config-inline"
  printf '{"agent":{"bees-read-only":{"mode":"primary","permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow"}}},"mcp":{"legacy":{"enabled":%s}}}\n' "$enabled" > "RECORD.config-content"
  cat "RECORD.config-content"
  exit 0
fi`

func fakeScript(t *testing.T, probe, body string) (bin, record string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "agent")
	record = filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		strings.ReplaceAll(probe, "RECORD", record) + "\n" +
		"printf '%s\\n' \"$@\" > " + record + ".args\n" +
		"cat > " + record + ".stdin\n" +
		"pwd > " + record + ".dir\n" +
		"env > " + record + ".env\n" +
		body
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, record
}

// fakeLog writes a fake whose records accumulate: two runs of one agent
// leave both command lines behind.
func fakeLog(t *testing.T, body string) (bin, record string) {
	t.Helper()
	dir := t.TempDir()
	bin = filepath.Join(dir, "agent")
	record = filepath.Join(dir, "record")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" >> " + record + ".args\n" +
		"cat >> " + record + ".stdin\n" +
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

// envOf is the environment the fake CLI ran in.
func envOf(t *testing.T, record string) string {
	t.Helper()
	return "\n" + recorded(t, record, "env")
}

// The four answers: one session of each backend, each answering "the brief".
const claudeAnswer = `echo '{"type":"result","subtype":"success","is_error":false,` +
	`"result":"the brief","session_id":"sess-1","num_turns":3,"total_cost_usd":0.5}'`

const codexAnswer = `echo '{"type":"thread.started","thread_id":"thread-9"}'
echo '{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"the brief"}}'
echo '{"type":"item.completed","item":{"type":"reasoning","text":"that will do"}}'
echo '{"type":"turn.completed"}'`

const openCodeAnswer = `echo '{"type":"text","sessionID":"open-7","part":{"type":"text","text":"the brief"}}'
echo '{"type":"step_finish","sessionID":"open-7","part":{"type":"step-finish","reason":"stop","cost":0.25}}'`

const piAnswer = `echo '{"type":"session","id":"pi-4"}'
echo '{"type":"message_end","message":{"role":"assistant","provider":"anthropic","model":"claude-sonnet","content":[{"type":"text","text":"the brief"}],"stopReason":"stop","usage":{"cost":{"total":0.75}}}}'
echo '{"type":"turn_end"}'`

// session is the fake per provider: what it answers and what a run of it
// is read back as.
type session struct {
	answer    string
	id        string
	turns     int
	cost      float64
	costKnown bool
}

func sessionsByAgent() map[string]session {
	return map[string]session{
		config.AgentClaude:   {claudeAnswer, "sess-1", 3, 0.5, true},
		config.AgentCodex:    {codexAnswer, "thread-9", 3, 0, false},
		config.AgentOpenCode: {openCodeAnswer, "open-7", 1, 0.25, true},
		config.AgentPi:       {piAnswer, "pi-4", 1, 0.75, true},
	}
}

// allProviderBins are the executables of every provider, so a case can run
// any of them through one adapter.
func allProviderBins(bin string) *CLIAgent {
	return &CLIAgent{ClaudeBin: bin, CodexBin: bin, OpenCodeBin: bin, PiBin: bin}
}

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
		"\n--tools\nRead,Grep,Glob,LS,NotebookRead\n",
		"\n--permission-prompts\nnone\n",
		"\n--setting-sources\n\n",
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
	// An MCP server is another way to run something, and the person's own
	// servers are configured for their own work, not for this: the
	// configuration the session is given holds none.
	if servers := mcpServers(t, got); len(servers) != 0 {
		t.Errorf("a review session was given MCP servers: %v", servers)
	}
	if prompt := recorded(t, record, "stdin"); prompt != "do it" {
		t.Errorf("prompt = %q, want it on stdin", prompt)
	}
}

// mcpServers reads the MCP configuration the session was pointed at and
// names the servers it holds.
func mcpServers(t *testing.T, argv string) []string {
	t.Helper()
	_, path, _ := strings.Cut(argv, "\n--mcp-config\n")
	path, _, _ = strings.Cut(path, "\n")
	var cfg struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	data, err := os.ReadFile(strings.TrimSpace(path))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	var names []string
	for name := range cfg.MCPServers {
		names = append(names, name)
	}
	return names
}

func TestASessionRunsInTheDirectoryItWasGiven(t *testing.T) {
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

func TestEveryProviderAnswersThroughTheAdapter(t *testing.T) {
	for name, tc := range sessionsByAgent() {
		t.Run(name, func(t *testing.T) {
			bin, _ := fakeCLI(t, tc.answer)
			agent := allProviderBins(bin)
			agent.Provider = name
			res, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			if res.Text != "the brief" || res.ID != tc.id || res.Turns != tc.turns || res.CostUSD != tc.cost || res.CostKnown != tc.costKnown {
				t.Errorf("result = %+v, want %q with %d turns, cost %.2f known %v", res, tc.id, tc.turns, tc.cost, tc.costKnown)
			}
			if res.Provider != name {
				t.Errorf("the result named provider %q, want the agent that answered", res.Provider)
			}
		})
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
	// failure in its result event and can still exit 0.
	bin, _ := fakeCLI(t, `echo '{"type":"result","is_error":true,"subtype":"error_during_execution","result":"no capacity"}'`)
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

func TestAModelTheConfigurationLeavesOutIsTheAgentsOwn(t *testing.T) {
	for name, tc := range sessionsByAgent() {
		t.Run(name, func(t *testing.T) {
			bin, record := fakeCLI(t, tc.answer)
			agent := allProviderBins(bin)
			agent.Provider = name
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

func TestACodexReviewSessionRunsInItsReadOnlySandbox(t *testing.T) {
	bin, record := fakeCLI(t, codexAnswer)
	agent := &CLIAgent{Provider: config.AgentCodex, CodexBin: bin, Model: "gpt-5", Effort: "max"}
	res, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	got := args(t, record)
	for _, want := range []string{"\nexec\n", "\n--json\n", "\n--sandbox\nread-only\n", "\n--model\ngpt-5\n",
		"\nmodel_reasoning_effort=\"high\"\n", "\nfeatures.shell_tool=false\n", "\nfeatures.plugins=false\n",
		"\nweb_search=\"disabled\"\n", "\napproval_policy=\"never\"\n", "\n-\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("codex was not given %q:\n%s", want, got)
		}
	}
	// Codex has no resume, and the id of a claude session is not one of its
	// threads: the shared execution refuses one before the launch rather
	// than start a session that had read nothing.
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir(), ResumeID: "thread-1"}); err == nil || !strings.Contains(err.Error(), "does not support follow-up") {
		t.Errorf("a resume id for codex: %v, want it refused", err)
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
	if err == nil || !strings.Contains(err.Error(), "no_result") {
		t.Fatalf("err = %v, want a stream that ended without saying", err)
	}
}

func TestAnOpenCodeReviewSessionRunsItsOwnReadOnlyAgent(t *testing.T) {
	bin, record := fakeCLI(t, openCodeAnswer)
	agent := &CLIAgent{Provider: config.AgentOpenCode, OpenCodeBin: bin, Model: "anthropic/claude-sonnet", Effort: "high"}
	res, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir(), ResumeID: "open-1"})
	if err != nil {
		t.Fatal(err)
	}
	got := args(t, record)
	for _, want := range []string{"\n--pure\n", "\nrun\n", "\n--format\njson\n", "\n--agent\nbees-read-only\n", "\n--model\nanthropic/claude-sonnet\n", "\n--session\nopen-1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("opencode was not given %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "--auto") {
		t.Errorf("a review session auto-approved its tools:\n%s", got)
	}
	// The read-only agent's exact permissions, the disabled sharing and
	// plugins, are inline configuration the CLI is probed on having taken;
	// the resolved configuration names every inherited server disabled.
	inline := recorded(t, record, "config-inline")
	for _, want := range []string{`"bees-read-only"`, `"*":"deny"`, `"read":"allow"`, `"grep":"allow"`, `"glob":"allow"`, `"share":"disabled"`, `"plugin":[]`} {
		if !strings.Contains(inline, want) {
			t.Errorf("the restricted configuration lacked %q:\n%s", want, inline)
		}
	}
	if resolved := recorded(t, record, "config-content"); !strings.Contains(resolved, `"legacy":{"enabled":false}`) {
		t.Errorf("the resolved configuration did not have the inherited server disabled:\n%s", resolved)
	}
	if env := envOf(t, record); !strings.Contains(env, "OPENCODE_DISABLE_DEFAULT_PLUGINS=true") {
		t.Errorf("discovered plugins were not disabled:\n%s", env)
	}
	if res.Text != "the brief" || res.ID != "open-7" || res.CostUSD != 0.25 || !res.CostKnown {
		t.Errorf("result = %+v", res)
	}
}

// TestAnOpenCodeConfigurationThatIgnoredTheFloorIsRefused runs a stand-in
// whose `debug config` reports the inherited server still enabled: the
// launch is refused before the model runs, and no session starts.
func TestAnOpenCodeConfigurationThatIgnoredTheFloorIsRefused(t *testing.T) {
	// The honest probe resolves "legacy" as disabled once the inline
	// configuration disables it; this one reports it enabled whatever it is
	// given, so the condition is false and the else branch always answers.
	liar := strings.Replace(honestProbe, "if printf", "if false && printf", 1)
	bin, record := fakeScript(t, liar, openCodeAnswer)
	agent := &CLIAgent{Provider: config.AgentOpenCode, OpenCodeBin: bin}
	_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "not disabled") {
		t.Fatalf("err = %v, want the launch refused", err)
	}
	if _, err := os.Stat(record + ".args"); !os.IsNotExist(err) {
		t.Fatalf("the session ran after the configuration was refused: %v", err)
	}
}

func TestAPiReviewSessionRunsItsReadOnlyTools(t *testing.T) {
	bin, record := fakeCLI(t, piAnswer)
	agent := &CLIAgent{Provider: config.AgentPi, PiBin: bin, Model: "anthropic/claude-sonnet", Effort: "max"}
	res, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir(), ResumeID: "pi-1"})
	if err != nil {
		t.Fatal(err)
	}
	got := args(t, record)
	for _, want := range []string{
		"\n-p\n", "\n--mode\njson\n", "\n--no-extensions\n", "\n--no-tools\n",
		"\n--tools\nread,grep,find,ls\n", "\n--no-skills\n", "\n--no-prompt-templates\n",
		"\n--no-context-files\n", "\n--model\nanthropic/claude-sonnet\n", "\n--thinking\nmax\n",
		"\n--session-id\npi-1\n", "\n--name\nagent-distiller\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("pi was not given %q:\n%s", want, got)
		}
	}
	// The ordinary pi session loads the MCP adapter and hands it the
	// session's servers; a review session loads no extension at all.
	if strings.Contains(got, "\n-e\n") || strings.Contains(got, "--mcp-config") {
		t.Errorf("a review session loaded an extension or an MCP configuration:\n%s", got)
	}
	if env := envOf(t, record); strings.Contains(env, "PI_MCP_CONFIG_MODE") {
		t.Errorf("a review session was configured for the MCP adapter:\n%s", env)
	}
	if res.Text != "the brief" || res.ID != "pi-4" || res.Turns != 1 || res.CostUSD != 0.75 || !res.CostKnown {
		t.Errorf("result = %+v", res)
	}
}

func TestAFailedPiResponseIsAnError(t *testing.T) {
	for _, tc := range []struct{ name, event, want string }{
		{"an error", `{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"no capacity"}}`, "error: no capacity"},
		{"cut short", `{"type":"message_end","message":{"role":"assistant","stopReason":"length"}}`, "stop_length"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bin, _ := fakeCLI(t, "echo '{\"type\":\"session\",\"id\":\"pi\"}'\necho '"+tc.event+"'")
			agent := &CLIAgent{Provider: config.AgentPi, PiBin: bin}
			_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
			if err == nil || !strings.HasSuffix(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAnUnknownProviderIsRefused(t *testing.T) {
	agent := &CLIAgent{Provider: "gemini"}
	_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller"})
	if err == nil || !strings.Contains(err.Error(), `unknown provider "gemini"`) ||
		!strings.Contains(err.Error(), "claude, codex, opencode, pi") {
		t.Fatalf("err = %v, want the unknown provider and the ones there are", err)
	}
}

// A backend that declares no restricted capability is refused, however it
// is named: the declaration is what the floor is derived from, not the
// name. The descriptor is borrowed for the length of the test and taken
// back after it.
func TestABackendWithoutTheFloorIsRefused(t *testing.T) {
	agent.Backends = append(agent.Backends, agent.Backend{Name: "gemini"})
	defer func() { agent.Backends = agent.Backends[:len(agent.Backends)-1] }()
	a := &CLIAgent{Provider: "gemini"}
	_, err := a.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), `review: agent "gemini" cannot establish the read-only restriction`) {
		t.Fatalf("err = %v, want the launch refused by the adapter", err)
	}
	// And the derivation a review's configuration is validated against
	// leaves it out: only a declared floor makes a provider.
	if slices.Contains(supportedProviders(), "gemini") {
		t.Error("a backend without the floor is one of the providers a review session can run as")
	}
}

func TestAFallbackChainThatNeverEndsIsRefused(t *testing.T) {
	a := &CLIAgent{Provider: config.AgentClaude}
	b := &CLIAgent{Provider: config.AgentCodex, Fallback: a}
	a.Fallback = b
	_, err := a.Run(context.Background(), AgentRequest{Name: "distiller"})
	if err == nil || !strings.Contains(err.Error(), "cycle") {
		t.Fatalf("err = %v, want the cycle refused", err)
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
	t.Setenv("REVIEW_TEST_INHERITED", "from the caller")
	bin, record := fakeCLI(t, claudeAnswer)
	agent := &CLIAgent{ClaudeBin: bin, Env: map[string]string{
		"REVIEW_TEST_ROLE":      "from the role",
		"REVIEW_TEST_INHERITED": "from the role too",
		"REVIEW_TEST_EXPANDED":  "$REVIEW_TEST_INHERITED and more",
	}}
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	env := envOf(t, record)
	if !strings.Contains(env, "\nREVIEW_TEST_ROLE=from the role\n") {
		t.Errorf("the role's variable did not reach the session:\n%s", env)
	}
	// A variable the role sets wins over the caller's own, as it does for a
	// factory session, and a $VAR reference in it is expanded.
	if !strings.Contains(env, "\nREVIEW_TEST_INHERITED=from the role too\n") {
		t.Errorf("the role's value of an inherited variable did not win:\n%s", env)
	}
	if !strings.Contains(env, "\nREVIEW_TEST_EXPANDED=from the caller and more\n") {
		t.Errorf("a $VAR reference was not expanded:\n%s", env)
	}
	// Nothing of the factory or of Git reaches the session, whatever the
	// role or the caller named.
	for _, leak := range []string{"BEES_ROLE", "BEES_SESSION_DIR", "GIT_DIR", "GIT_CONFIG_COUNT"} {
		if strings.Contains(env, leak+"=") {
			t.Errorf("%s reached the session:\n%s", leak, env)
		}
	}
}

// A review must not inherit factory identity or Git access overrides, even
// when its caller is itself a factory session or the role's env names
// those keys.
func TestTheSessionIsNotGivenFactoryOrVCSVariables(t *testing.T) {
	t.Setenv("BEES_ROLE", "reviewer")
	t.Setenv("BEES_SESSION_DIR", "/session")
	t.Setenv("GIT_DIR", "/shared")
	t.Setenv("REVIEW_TEST_KEPT", "yes")
	bin, record := fakeCLI(t, claudeAnswer)
	agent := &CLIAgent{ClaudeBin: bin, Env: map[string]string{"BEES_REPO": "secret", "GIT_WORK_TREE": "/repo", "REVIEW_TEST_ROLE": "ok"}}
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	env := envOf(t, record)
	for _, leak := range []string{"BEES_ROLE", "BEES_SESSION_DIR", "BEES_REPO", "GIT_DIR", "GIT_WORK_TREE"} {
		if strings.Contains(env, leak+"=") {
			t.Errorf("%s leaked into the session:\n%s", leak, env)
		}
	}
	if !strings.Contains(env, "REVIEW_TEST_KEPT=yes") || !strings.Contains(env, "REVIEW_TEST_ROLE=ok") {
		t.Errorf("the caller's own variables were not kept:\n%s", env)
	}
}

func TestReviewExecutionSettingsAndSafety(t *testing.T) {
	for name, tc := range sessionsByAgent() {
		t.Run(name, func(t *testing.T) {
			bin, record := fakeCLI(t, tc.answer)
			a := allProviderBins(bin)
			a.Provider, a.Model, a.Effort = name, "chosen", "max"
			a.Fallback = &CLIAgent{Provider: config.AgentClaude, Model: "fallback"}
			if _, err := a.Run(context.Background(), AgentRequest{Dir: t.TempDir()}); err != nil {
				t.Fatal(err)
			}
			got := args(t, record)
			wants := []string{"\n--model\nchosen\n"}
			switch name {
			case config.AgentClaude:
				// Claude can switch to another claude model itself; the
				// claude fallback's model goes as --fallback-model.
				wants = append(wants, "\n--fallback-model\nfallback\n", "\n--effort\nmax\n", "\n--setting-sources\n\n")
			case config.AgentCodex:
				// Codex's levels stop at high, and there is no fallback
				// model flag: the fallback is a session of its own.
				wants = append(wants, "\nmodel_reasoning_effort=\"high\"\n", "\nfeatures.shell_tool=false\n", "\n--sandbox\nread-only\n")
				if strings.Contains(got, "--fallback-model") {
					t.Errorf("codex was given claude's fallback flag:\n%s", got)
				}
			case config.AgentOpenCode:
				wants = append(wants, "\n--agent\nbees-read-only\n")
				// opencode takes effort as the read-only agent's variant,
				// in the inline configuration, not on the command line.
				if !strings.Contains(recorded(t, record, "config-inline"), `"variant":"max"`) {
					t.Errorf("the read-only agent ran without the effort:\n%s", recorded(t, record, "config-inline"))
				}
			case config.AgentPi:
				wants = append(wants, "\n--thinking\nmax\n", "\n--no-tools\n")
			}
			for _, want := range wants {
				if !strings.Contains(got, want) {
					t.Errorf("missing %q in %s", want, got)
				}
			}
		})
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
			Enabled *bool  `toml:"enabled"`
			URL     string `toml:"url"`
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
	// The fourth is the read server, the session's only way to read a file.
	if len(overrides.Servers) != 4 || !strings.HasPrefix(overrides.Servers[agent.ReadServerName].URL, "http://127.0.0.1:") {
		t.Fatalf("MCP servers = %v, want the three configured servers disabled and the read server", overrides.Servers)
	}
	for _, name := range []string{"bees", "server.with.dots", `server "quoted"`} {
		if server := overrides.Servers[name]; server.Enabled == nil || *server.Enabled {
			t.Errorf("server %q was not disabled", name)
		}
	}
}

func TestCodexReviewMCPInventoryFailsClosed(t *testing.T) {
	for _, tc := range []struct{ name, response, want string }{
		{"failed command", "exit 1", "list MCP servers"},
		{"invalid JSON", "echo invalid", "decode MCP servers"},
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

// A session refused for want of capacity runs again as the fallback agent,
// down the chain until one answers, and the one that answers is a review
// session like any other: every attempt is held to the same read-only
// floor, a cross-agent fallback starts its own session, and the run is
// recorded under the agent and model that answered. Any other failure is
// the session's own, and the fallback never runs.
func TestAReviewSessionWithoutCapacityRunsAsItsFallback(t *testing.T) {
	limited, limitedRecord := fakeCLI(t, `echo '{"type":"result","subtype":"error","is_error":true,"result":"Rate limit reached for opus","session_id":"sess-0","num_turns":0}'`)
	overloaded, overloadedRecord := fakeCLI(t, `echo '{"type":"error","message":"the model is overloaded"}'`)
	answering, answeringRecord := fakeCLI(t, openCodeAnswer)
	a := &CLIAgent{ClaudeBin: limited, Model: "opus", Fallback: &CLIAgent{
		Provider: config.AgentCodex, CodexBin: overloaded, Model: "gpt-first", Fallback: &CLIAgent{
			Provider: config.AgentOpenCode, OpenCodeBin: answering, Model: "gpt-last"}}}
	res, err := a.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Text != "the brief" || res.ID != "open-7" || res.Provider != config.AgentOpenCode || res.Model != "gpt-last" {
		t.Errorf("answer: %+v, want the last fallback's answer, named as its own", res)
	}
	if got := args(t, limitedRecord); strings.Contains(got, "--fallback-model") {
		t.Errorf("the claude session was told a fallback model it cannot switch to:%s", got)
	}
	// Every attempt is held to its own backend's floor.
	for _, tc := range []struct {
		record string
		want   []string
	}{
		{overloadedRecord, []string{"\nexec\n", "\n--sandbox\nread-only\n", "\nfeatures.shell_tool=false\n"}},
		{answeringRecord, []string{"\n--pure\n", "\n--agent\nbees-read-only\n"}},
	} {
		got := args(t, tc.record)
		for _, want := range tc.want {
			if !strings.Contains(got, want) {
				t.Errorf("the fallback escaped the floor, missing %q:%s", want, got)
			}
		}
	}
	if got := args(t, answeringRecord); !strings.Contains(got, "\n--model\ngpt-last\n") {
		t.Errorf("the last fallback ran another model:%s", got)
	}

	// A CLI that says so on stderr alone and exits without a result event
	// is caught the same way. One agent plays both: it rate-limits the
	// model it is first asked for and answers for the fallback's, and its
	// records accumulate, so both command lines are read back.
	flip, flipRecord := fakeLog(t, `case " $* " in
  *" --model opus "*) echo 'API Error: 429 rate limit reached' >&2; exit 1 ;;
  *) `+claudeAnswer+` ;;
esac`)
	a = &CLIAgent{ClaudeBin: flip, Model: "opus", Fallback: &CLIAgent{ClaudeBin: flip, Model: "sonnet"}}
	res, err = a.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()})
	if err != nil || res.Text != "the brief" || res.Provider != config.AgentClaude || res.Model != "sonnet" {
		t.Fatalf("a capacity failure on stderr: %v %+v", err, res)
	}
	if got := args(t, flipRecord); !strings.Contains(got, "\n--fallback-model\nsonnet\n") {
		t.Errorf("a claude fallback's model was not passed to claude:%s", got)
	}
	if got := args(t, flipRecord); !strings.Contains(got, "\n--model\nsonnet\n") {
		t.Errorf("the fallback ran another model:%s", got)
	}

	failing, _ := fakeCLI(t, `echo '{"type":"result","subtype":"error","is_error":true,"result":"the prompt was refused","session_id":"sess-0","num_turns":1}'`)
	never, neverRecord := fakeCLI(t, claudeAnswer)
	a = &CLIAgent{ClaudeBin: failing, Model: "opus", Fallback: &CLIAgent{ClaudeBin: never, Model: "sonnet"}}
	if _, err := a.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()}); err == nil || !strings.Contains(err.Error(), "the prompt was refused") {
		t.Fatalf("a failure that is not about capacity: %v", err)
	}
	if _, err := os.Stat(neverRecord + ".args"); err == nil {
		t.Error("the fallback ran for a failure that is not about capacity")
	}
}

// A review session goes through the same guard as a factory session: an
// agent no test made is refused from a test binary before it is started.
func TestARealAgentNeverRunsFromATestBinary(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	agent := &CLIAgent{ClaudeBin: sh}
	_, err = agent.Run(context.Background(), AgentRequest{Name: "distiller", Prompt: "do it", Dir: t.TempDir()})
	if !errors.Is(err, agentbin.ErrRealAgent) {
		t.Fatalf("err = %v, want ErrRealAgent", err)
	}
}
