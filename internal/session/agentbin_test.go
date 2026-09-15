package session

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/agentbin"
	"github.com/kpenfound/busybees/internal/config"
)

// TestARealAgentNeverRunsFromATestBinary pins the guard every session goes
// through: a runner pointed at an executable no test made, which is what a
// forgotten fake resolves to, refuses before anything starts. The suite
// itself is what a bee runs inside its session, so this is what keeps the
// suite from starting the real claude on the host.
func TestARealAgentNeverRunsFromATestBinary(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	r := newRunner(t, sh)
	sessions := r.SessionsDir
	role := config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 5, Timeout: time.Minute}
	_, err = r.Run(context.Background(), Request{Name: "real", Role: role, WorkDir: t.TempDir(), SystemPrompt: "SYS", Prompt: "TASK"})
	if !errors.Is(err, agentbin.ErrRealAgent) {
		t.Fatalf("err = %v, want ErrRealAgent", err)
	}
	// Refused before the session was started: no transcript, no pid file.
	entries, _ := os.ReadDir(sessions)
	for _, e := range entries {
		for _, f := range []string{TranscriptFile, "pid"} {
			if _, err := os.Stat(filepath.Join(sessions, e.Name(), f)); err == nil {
				t.Errorf("%s written for a session bees refused to start", f)
			}
		}
	}
}
