package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// afterTheCap is what a fake prints once it has slept past the point a
// stopped turn is killed at: a transcript that holds it ran to its end.
const afterTheCap = `sleep 30
echo '{"type":"after-the-cap"}'`

// capRun runs one session of the named agent, its fake's stream the body,
// held to a cap of capUSD.
func capRun(t *testing.T, agentName, body string, capUSD float64) *Result {
	t.Helper()
	r := newRunner(t, "")
	var role Profile
	switch agentName {
	case AgentClaude:
		r.ClaudeBin = fakeClaude(t, body)
		role = Profile{Name: "builder", Model: "opus", MaxTurns: 10, Timeout: time.Minute}
	case AgentOpenCode:
		r.OpenCodeBin = fakeOpenCode(t, body)
		role = opencodeRole("")
	case AgentPi:
		r.PiBin = fakePi(t, body)
		role = piRole("")
	case AgentCodex:
		r.CodexBin = fakeCodex(t, body)
		role = codexRole("")
	}
	res, err := r.Run(context.Background(), Request{Name: "cap", Profile: role, Workspace: fakeWorkspace{dir: t.TempDir()}, Prompt: "TASK", CostCapUSD: capUSD})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// assertCapped checks a session the cap ended: an error with the cap's own
// subtype, the cost observed until then, and a result file saying the same.
func assertCapped(t *testing.T, res *Result, usd float64) {
	t.Helper()
	if !res.CostCapped || !res.IsError || res.ErrorSubtype != SubtypeCostCap || res.TimedOut {
		t.Errorf("capped %v error %v subtype %q timed out %v, want a cost-cap stop", res.CostCapped, res.IsError, res.ErrorSubtype, res.TimedOut)
	}
	if !res.CostKnown || res.CostUSD != usd {
		t.Errorf("cost: known %v, %v; want %v", res.CostKnown, res.CostUSD, usd)
	}
	b, err := os.ReadFile(filepath.Join(res.SessionDir, ResultFile))
	if err != nil {
		t.Fatalf("a capped session wrote no result file: %v", err)
	}
	var got Result
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	if !got.CostCapped || got.ErrorSubtype != SubtypeCostCap || got.CostUSD != usd {
		t.Errorf("result.json: %+v", got)
	}
}

// assertStoppedInFlight checks the turn was killed rather than left to run
// to the end of its script.
func assertStoppedInFlight(t *testing.T, res *Result) {
	t.Helper()
	if res.Duration > 20*time.Second {
		t.Errorf("the turn ran %v: it was not stopped at the cap", res.Duration)
	}
	b, err := os.ReadFile(res.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "after-the-cap") {
		t.Error("the turn went on past the cap")
	}
}

// Every backend reports cost its own way, and each is stopped in flight at
// the event that takes the known cost to the cap: opencode and pi report
// each step's cost, which add up, and claude a running total in its result
// event, which is held open here the way background subagents hold it.
func TestCostCapStopsATurnInFlight(t *testing.T) {
	tests := []struct {
		agent string
		body  string
		usd   float64
	}{
		{AgentOpenCode, `
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":0.25}}'
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":0.5}}'
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":0.5}}'
` + afterTheCap, 1.25},
		{AgentPi, `
echo '{"type":"session","id":"pi-1"}'
echo '{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse","usage":{"cost":{"total":0.5}}}}'
echo '{"type":"turn_end"}'
echo '{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse","usage":{"cost":{"total":0.75}}}}'
echo '{"type":"turn_end"}'
` + afterTheCap, 1.25},
		{AgentClaude, `
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"one"}]}}'
echo '{"type":"result","subtype":"success","result":"done","session_id":"s1","num_turns":1,"total_cost_usd":1.5}'
` + afterTheCap, 1.5},
	}
	for _, tc := range tests {
		t.Run(tc.agent, func(t *testing.T) {
			res := capRun(t, tc.agent, tc.body, 1)
			assertCapped(t, res, tc.usd)
			assertStoppedInFlight(t, res)
		})
	}
}

// Below the cap nothing is stopped: a step's cost alone, or the sum of
// the steps so far, short of it runs on. Claude's result events carry a
// running total, so two of them are not added up.
func TestCostCapBelowTheCapRunsToTheEnd(t *testing.T) {
	tests := []struct {
		agent string
		body  string
		usd   float64
	}{
		{AgentOpenCode, `
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":0.25}}'
echo '{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"done"}}'
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","cost":0.5}}'
`, 0.75},
		{AgentPi, `
echo '{"type":"session","id":"pi-1"}'
echo '{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse","usage":{"cost":{"total":0.25}}}}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop","usage":{"cost":{"total":0.5}}}}'
`, 0.75},
		{AgentClaude, `
echo '{"type":"result","subtype":"success","result":"first","session_id":"s1","num_turns":1,"total_cost_usd":0.5}'
echo '{"type":"result","subtype":"success","result":"done","session_id":"s1","num_turns":2,"total_cost_usd":0.75}'
`, 0.75},
	}
	for _, tc := range tests {
		t.Run(tc.agent, func(t *testing.T) {
			res := capRun(t, tc.agent, tc.body, 1)
			if res.CostCapped || res.IsError || res.ErrorSubtype != "" || !res.CostKnown || res.CostUSD != tc.usd {
				t.Fatalf("result: %+v", res)
			}
		})
	}
}

// The final cost is checked too: a turn that ends on its own at or over
// the cap is capped, whether its last event reached it as the process
// exited or claude stopped itself at its --max-budget-usd.
func TestCostCapChecksTheFinalCost(t *testing.T) {
	tests := []struct {
		name  string
		agent string
		body  string
		usd   float64
	}{
		{"opencode last step", AgentOpenCode, `
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":0.5}}'
echo '{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"done"}}'
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","cost":0.5}}'
`, 1},
		{"pi last response", AgentPi, `
echo '{"type":"session","id":"pi-1"}'
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"done"}],"stopReason":"stop","usage":{"cost":{"total":1}}}}'
`, 1},
		{"claude result", AgentClaude, `
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"one"}]}}'
echo '{"type":"result","subtype":"success","result":"done","session_id":"s1","num_turns":1,"total_cost_usd":1.25}'
`, 1.25},
		{"claude budget stop", AgentClaude, `
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"one"}]}}'
echo '{"type":"result","subtype":"error_max_budget_usd","is_error":true,"session_id":"s1","num_turns":1,"total_cost_usd":1.03}'
`, 1.03},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assertCapped(t, capRun(t, tc.agent, tc.body, 1), tc.usd)
		})
	}
}

// Unset, the cap is no cap: a session costing any amount runs to its end,
// and claude is given no budget of its own.
func TestCostCapUnsetIsNoCap(t *testing.T) {
	res := capRun(t, AgentOpenCode, `
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":500}}'
echo '{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"done"}}'
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","cost":500}}'
`, 0)
	if res.CostCapped || res.IsError || res.CostUSD != 1000 {
		t.Fatalf("result: %+v", res)
	}
	res = capRun(t, AgentClaude, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","result":"done","session_id":"s1","num_turns":1,"total_cost_usd":1000}'
`, 0)
	if res.CostCapped || res.IsError || res.CostUSD != 1000 {
		t.Fatalf("result: %+v", res)
	}
	if args := readArgs(t, res.SessionDir); slices.Contains(args, "--max-budget-usd") {
		t.Errorf("claude was given a budget with no cap: %q", args)
	}
}

// Claude is held to the cap by its own --max-budget-usd too, because its
// stream carries no cost before the turn's result.
func TestCostCapIsClaudesBudget(t *testing.T) {
	res := capRun(t, AgentClaude, `
printf '%s\n' "$@" > "$TASK_SESSION_DIR/args.txt"
echo '{"type":"result","subtype":"success","result":"done","session_id":"s1","num_turns":1,"total_cost_usd":0.1}'
`, 2.5)
	args := readArgs(t, res.SessionDir)
	if i := slices.Index(args, "--max-budget-usd"); i < 0 || i+1 >= len(args) || args[i+1] != "2.5" {
		t.Errorf("budget not passed: %q", args)
	}
}

func readArgs(t *testing.T, dir string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

// An unknown cost never reaches the cap: codex reports tokens and no cost,
// and a claude turn killed before its result event reported none.
func TestCostCapUnknownCostIsNotTheCap(t *testing.T) {
	res := capRun(t, AgentCodex, `
echo '{"type":"thread.started","thread_id":"thread-1"}'
echo '{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"done"}}'
echo '{"type":"turn.completed","usage":{"input_tokens":900000,"cached_input_tokens":0,"output_tokens":900000}}'
`, 0.01)
	if res.CostCapped || res.IsError || res.CostKnown || res.ResultText != "done" {
		t.Fatalf("codex result: %+v", res)
	}
	res = capRun(t, AgentClaude, `
echo '{"type":"assistant","message":{"content":[{"type":"text","text":"one"}],"usage":{"input_tokens":900000}}}'
exit 1
`, 0.01)
	if res.CostCapped || res.CostKnown || res.ErrorSubtype != "exit_1" {
		t.Fatalf("claude result: %+v", res)
	}
	// A stream value that is no cost says nothing about spend either.
	res = capRun(t, AgentOpenCode, `
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":-5}}'
echo '{"type":"text","sessionID":"ses_1","part":{"type":"text","text":"done"}}'
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"stop","cost":0.25}}'
`, 0.1)
	if !res.CostCapped || res.CostUSD != 0.25 {
		t.Fatalf("opencode result: %+v", res)
	}
}

// A stream that ends part way keeps the cost it reported until then: the
// steps an opencode or pi session finished before it was stopped.
func TestCostCapKeepsThePartialCost(t *testing.T) {
	res := capRun(t, AgentPi, `
echo '{"type":"session","id":"pi-1"}'
echo '{"type":"message_end","message":{"role":"assistant","stopReason":"toolUse","usage":{"cost":{"total":0.25}}}}'
echo '{"type":"turn_end"}'
`, 1)
	if res.CostCapped || res.ErrorSubtype != "no_result" || !res.CostKnown || res.CostUSD != 0.25 {
		t.Fatalf("result: %+v", res)
	}
}

// The caller's cancellation is not a cost-cap stop: it returns the
// cancellation and leaves no result file, cap or no cap.
func TestCostCapIsNotTheCallersCancellation(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := newRunner(t, "")
	r.OpenCodeBin = fakeOpenCode(t, `
echo '{"type":"step_finish","sessionID":"ses_1","part":{"type":"step-finish","reason":"tool-calls","cost":0.25}}'
exec sleep 60`)
	ended := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, Request{Name: "cancel", SessionDir: dir, Profile: opencodeRole(""), Workspace: fakeWorkspace{dir: t.TempDir()}, CostCapUSD: 1})
		ended <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for CountTurns(filepath.Join(dir, TranscriptFile)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err := <-ended
	if !errors.Is(err, context.Canceled) || errors.Is(err, errCostCap) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ResultFile)); !os.IsNotExist(err) {
		t.Fatalf("result persisted after cancellation: %v", err)
	}
}

// A cap that is no amount is refused before anything starts.
func TestCostCapMustBeAnAmount(t *testing.T) {
	for _, usd := range []float64{-1, math.NaN(), math.Inf(1)} {
		marker := filepath.Join(t.TempDir(), "started")
		r := newRunner(t, fakeClaude(t, `touch `+marker))
		_, err := r.Run(context.Background(), Request{Name: "bad", Profile: Profile{Name: "builder", Model: "opus", MaxTurns: 1, Timeout: time.Minute}, Workspace: fakeWorkspace{dir: t.TempDir()}, CostCapUSD: usd})
		if err == nil || !strings.Contains(err.Error(), "cost cap") {
			t.Errorf("cap %v: %v", usd, err)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Errorf("cap %v: the agent started", usd)
		}
	}
}

// A restricted turn is held to the cap the same way.
func TestRunRestrictedHoldsTheCostCap(t *testing.T) {
	bin, record := restrictedFake(t, "claude", `echo '{"type":"result","subtype":"success","result":"the brief","session_id":"s1","num_turns":1,"total_cost_usd":1.5}'`)
	req := restrictedRequestFor(AgentClaude, t.TempDir())
	req.CostCapUSD = 1
	_, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), req)
	if err == nil || !strings.Contains(err.Error(), SubtypeCostCap) {
		t.Fatalf("restricted turn over the cap: %v", err)
	}
	if args := restrictedArgs(t, record, ".args"); !slices.Contains(args, "--max-budget-usd") {
		t.Errorf("budget not passed: %q", args)
	}
}
