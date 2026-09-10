package review

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	if res.Text != "the brief" || res.ID != "sess-1" || res.Turns != 3 || res.CostUSD != 0.5 {
		t.Errorf("result = %+v", res)
	}
}

func TestAFailedClaudeSessionIsAnError(t *testing.T) {
	bin, _ := fakeCLI(t, `echo '{"type":"result","subtype":"error_max_turns","is_error":true,"result":"gave up"}'`)
	agent := &CLIAgent{ClaudeBin: bin}
	_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "error_max_turns: gave up") {
		t.Fatalf("err = %v, want the failure the session reported", err)
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
	bin, record := fakeCLI(t, claudeAnswer)
	agent := &CLIAgent{ClaudeBin: bin}
	if _, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if got := args(t, record); strings.Contains(got, "--model") {
		t.Errorf("a model was chosen:\n%s", got)
	}
}

const codexAnswer = `echo '{"type":"thread.started","thread_id":"thread-9"}'
echo '{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"the brief"}}'
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
	if res.Text != "the brief" || res.ID != "thread-9" || res.Turns != 2 || res.CostUSD != 0 {
		t.Errorf("result = %+v", res)
	}
}

func TestAFailedCodexTurnIsAnError(t *testing.T) {
	bin, _ := fakeCLI(t, `echo '{"type":"thread.started","thread_id":"t"}'
echo '{"type":"turn.failed","error":{"message":"context window"}}'`)
	agent := &CLIAgent{Provider: config.AgentCodex, CodexBin: bin}
	_, err := agent.Run(context.Background(), AgentRequest{Name: "distiller", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "turn_failed: context window") {
		t.Fatalf("err = %v, want the failed turn", err)
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
