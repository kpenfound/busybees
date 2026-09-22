package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/vcs"
)

const restrictedClaudeAnswer = `echo '{"type":"result","subtype":"success","is_error":false,"result":"the brief","session_id":"sess-1","num_turns":3,"total_cost_usd":0.5}'`

const restrictedCodexAnswer = `echo '{"type":"thread.started","thread_id":"thread-9"}'
echo '{"type":"item.completed","item":{"type":"reasoning","text":"thinking"}}'
echo '{"type":"item.completed","item":{"type":"agent_message","text":"the brief"}}'
echo '{"type":"turn.completed"}'`

func restrictedFake(t *testing.T, name, body string) (string, string) {
	t.Helper()
	record := filepath.Join(t.TempDir(), "record")
	script := `if [ "$1" = mcp ]; then
  printf '%s\n' "$@" > "` + record + `.mcp-args"
  echo '[]'
  exit 0
fi
printf '%s\n' "$@" > "` + record + `.args"
cat > "` + record + `.stdin"
env > "` + record + `.env"
` + body
	return agenttest.Script(t, name, script), record
}

func restrictedArgs(t *testing.T, record, suffix string) []string {
	t.Helper()
	b, err := os.ReadFile(record + suffix)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func restrictedRunner(t *testing.T, claude, codex string) *Runner {
	t.Helper()
	return &Runner{ClaudeBin: claude, CodexBin: codex, SessionsDir: t.TempDir(), EnvironmentPrefix: "BEES_"}
}

func restrictedRequestFor(agentName, dir string) Request {
	return Request{
		Name:      "distiller",
		Profile:   Profile{Name: "reviewer", Agent: agentName, Model: "chosen", Effort: "max"},
		Workspace: vcs.Directory(dir),
		Prompt:    "do it",
	}
}

func TestRunRestrictedUsesTheSharedClaudeBackend(t *testing.T) {
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	res, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), restrictedRequestFor(AgentClaude, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultText != "the brief" || res.ClaudeID != "sess-1" || res.NumTurns != 3 || res.CostUSD != 0.5 || !res.CostKnown || res.Agent != AgentClaude || res.Model != "chosen" {
		t.Fatalf("result = %+v", res)
	}
	args := restrictedArgs(t, record, ".args")
	for _, want := range []string{"--permission-prompts", "none", "--setting-sources", "", "--tools", strings.Join(restrictedReadTools, ","), "--allowedTools", strings.Join(restrictedReadTools, ","), "--disallowedTools", strings.Join(restrictedDeniedTools, ","), "--strict-mcp-config", "--effort", "max", "--max-turns", "40"} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %q:\n%v", want, args)
		}
	}
	if slices.Contains(args, "--dangerously-skip-permissions") {
		t.Errorf("restricted Claude skipped permissions: %v", args)
	}
	if got, _ := os.ReadFile(record + ".stdin"); string(got) != "do it" {
		t.Errorf("stdin = %q", got)
	}
}

func TestRunRestrictedUsesTheSharedCodexBackend(t *testing.T) {
	bin, record := restrictedFake(t, "codex", restrictedCodexAnswer)
	res, err := restrictedRunner(t, "", bin).RunRestricted(context.Background(), restrictedRequestFor(AgentCodex, t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	if res.ResultText != "the brief" || res.ClaudeID != "thread-9" || res.NumTurns != 2 || res.CostKnown || res.Agent != AgentCodex || res.Model != "chosen" {
		t.Fatalf("result = %+v", res)
	}
	args := restrictedArgs(t, record, ".args")
	for _, want := range []string{"exec", "--json", "--sandbox", "read-only", `approval_policy="never"`, `web_search="disabled"`, "features.shell_tool=false", "features.plugins=false", `model_reasoning_effort="high"`} {
		if !slices.Contains(args, want) {
			t.Errorf("args missing %q:\n%v", want, args)
		}
	}
	if slices.Contains(args, "--dangerously-bypass-approvals-and-sandbox") {
		t.Errorf("restricted Codex bypassed its sandbox: %v", args)
	}
	probe := restrictedArgs(t, record, ".mcp-args")
	for _, want := range []string{"mcp", "list", "--json", "features.plugins=false", "orchestrator.mcp.enabled=false"} {
		if !slices.Contains(probe, want) {
			t.Errorf("MCP inventory missing %q: %v", want, probe)
		}
	}
}

func TestRunRestrictedDeniesInheritedIdentityConfigurationAndMCP(t *testing.T) {
	t.Setenv("BEES_ROLE", "reviewer")
	t.Setenv("GIT_DIR", "/shared/repo")
	t.Setenv("REVIEW_SAFE", "inherited")
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	req := restrictedRequestFor(AgentClaude, t.TempDir())
	req.Env = map[string]string{"BEES_SESSION_DIR": "/factory", "GIT_WORK_TREE": "/repo", "ROLE_SAFE": "yes"}
	res, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	env, err := os.ReadFile(record + ".env")
	if err != nil {
		t.Fatal(err)
	}
	for _, gone := range []string{"BEES_ROLE=", "BEES_SESSION_DIR=", "GIT_DIR=", "GIT_WORK_TREE="} {
		if strings.Contains(string(env), gone) {
			t.Errorf("restricted environment contains %q:\n%s", gone, env)
		}
	}
	for _, want := range []string{"REVIEW_SAFE=inherited", "ROLE_SAFE=yes"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("restricted environment missing %q:\n%s", want, env)
		}
	}
	args := restrictedArgs(t, record, ".args")
	i := slices.Index(args, "--mcp-config")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("no MCP config in %v", args)
	}
	data, err := os.ReadFile(args[i+1])
	if err != nil {
		t.Fatal(err)
	}
	var config struct {
		Servers map[string]MCPEntry `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &config); err != nil || len(config.Servers) != 0 {
		t.Fatalf("MCP config = %s, err %v", data, err)
	}
	if res.HasOutcome {
		t.Error("restricted session reported a factory outcome")
	}
}

func TestRunRestrictedDisablesEveryInheritedCodexMCPServer(t *testing.T) {
	bin, record := restrictedFake(t, "codex", restrictedCodexAnswer)
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	script := strings.Replace(string(data), "echo '[]'", `echo '[{"name":"bees"},{"name":"server.with.dots"},{"name":"server \"quoted\""}]'`, 1)
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := restrictedRunner(t, "", bin).RunRestricted(context.Background(), restrictedRequestFor(AgentCodex, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(restrictedArgs(t, record, ".args"), "\n")
	want := `mcp_servers={"bees"={enabled=false},"server.with.dots"={enabled=false},"server \"quoted\""={enabled=false}}`
	if !strings.Contains(joined, want) {
		t.Errorf("configured MCP servers were not disabled:\n%s", joined)
	}
}

func TestRunRestrictedFailsClosedBeforeCodexLaunch(t *testing.T) {
	bin, record := restrictedFake(t, "codex", restrictedCodexAnswer)
	data, err := os.ReadFile(bin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte(strings.Replace(string(data), "echo '[]'", "echo invalid", 1)), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err = restrictedRunner(t, "", bin).RunRestricted(context.Background(), restrictedRequestFor(AgentCodex, t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "decode MCP servers") {
		t.Fatalf("error = %v", err)
	}
	if _, err := os.Stat(record + ".args"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("model launched after restriction setup failed: %v", err)
	}
}

func TestRunRestrictedFallsBackUnderTheSameFloor(t *testing.T) {
	limited, limitedRecord := restrictedFake(t, "claude", `echo '{"type":"result","subtype":"error","is_error":true,"result":"rate limit reached","session_id":"s0"}'`)
	answering, answerRecord := restrictedFake(t, "codex", restrictedCodexAnswer)
	r := restrictedRunner(t, limited, answering)
	req := restrictedRequestFor(AgentClaude, t.TempDir())
	req.Profile.Model = "opus"
	req.Profile.Fallback = &Profile{Name: "reviewer", Agent: AgentCodex, Model: "gpt-last"}
	res, err := r.RunRestricted(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if res.Agent != AgentCodex || res.Model != "gpt-last" || res.ClaudeID != "thread-9" {
		t.Fatalf("fallback result = %+v", res)
	}
	if slices.Contains(restrictedArgs(t, limitedRecord, ".args"), "--dangerously-skip-permissions") {
		t.Error("primary escaped the restriction")
	}
	args := restrictedArgs(t, answerRecord, ".args")
	if !slices.Contains(args, "read-only") || !slices.Contains(args, "features.shell_tool=false") {
		t.Errorf("fallback escaped the restriction: %v", args)
	}
}

func TestRunRestrictedPreservesClaudeFollowUp(t *testing.T) {
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	req := restrictedRequestFor(AgentClaude, t.TempDir())
	req.ResumeID = "sess-before"
	if _, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	args := restrictedArgs(t, record, ".args")
	i := slices.Index(args, "--resume")
	if i < 0 || i+1 >= len(args) || args[i+1] != "sess-before" || !slices.Contains(args, "--system-prompt-snapshot") {
		t.Errorf("follow-up was not resumed: %v", args)
	}
}

func TestRunRestrictedReportsMalformedAndFailedOutput(t *testing.T) {
	t.Run("malformed claude", func(t *testing.T) {
		bin, _ := restrictedFake(t, "claude", "echo not-json")
		_, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), restrictedRequestFor(AgentClaude, t.TempDir()))
		if err == nil || !strings.Contains(err.Error(), "no_result") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("failed codex", func(t *testing.T) {
		bin, _ := restrictedFake(t, "codex", `echo '{"type":"turn.failed","error":{"message":"context window"}}'`)
		_, err := restrictedRunner(t, "", bin).RunRestricted(context.Background(), restrictedRequestFor(AgentCodex, t.TempDir()))
		if err == nil || !strings.Contains(err.Error(), "turn_failed: context window") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestRunRestrictedCancellationKillsTheProcessGroup(t *testing.T) {
	bin, _ := restrictedFake(t, "claude", "sleep 60 &\nwait")
	r := restrictedRunner(t, bin, "")
	req := restrictedRequestFor(AgentClaude, t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	start := time.Now()
	go func() {
		_, err := r.RunRestricted(ctx, req)
		done <- err
	}()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "session stopped") {
			t.Fatalf("error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 2*time.Second {
			t.Fatalf("cancellation took %s; a child likely kept the pipes open", elapsed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("cancellation did not stop the agent and its child")
	}
}

func TestRunRestrictedRefusesUnsupportedCapabilitiesBeforeLaunch(t *testing.T) {
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	r := restrictedRunner(t, bin, "")
	for _, tc := range []struct {
		name   string
		change func(*Request)
	}{
		{"unsupported backend", func(req *Request) { req.Profile.Agent = AgentOpenCode }},
		{"MCP", func(req *Request) { req.Profile.MCP = map[string]MCPEntry{"tools": {Command: "server"}} }},
		{"writable VCS", func(req *Request) { req.Profile.VCSAccess = true }},
		{"caller grants", func(req *Request) { req.Grants = &Grants{} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := restrictedRequestFor(AgentClaude, t.TempDir())
			tc.change(&req)
			if _, err := r.RunRestricted(context.Background(), req); err == nil {
				t.Fatal("unsafe request was accepted")
			}
		})
	}
	if _, err := os.Stat(record + ".args"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("agent launched for a refused request: %v", err)
	}
}
