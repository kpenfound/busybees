package review

import (
	"context"
	"errors"
	"os/exec"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agentbin"
)

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
