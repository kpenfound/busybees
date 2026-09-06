package scheduler

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/state"
)

// A session's box is a property of the session, so it rides on the
// session-started event the live view reads and on the worker `bees status`
// prints — not on the configuration a reader would have to go and resolve.
// Every mode but "none" refuses to run today (config.CheckSandboxMode), so
// "none" is the only one a session can be observed in; what this pins is
// that the resolved mode is carried at all, rather than left empty.
func TestASessionReportsTheSandboxItRunsIn(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	sub := h.sched.Subscribe()
	_, release, cancel, done := startHeldSession(t, h, func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "args.txt"))
		return err == nil
	}, "the developer session to start")
	defer cancel()

	// While it runs: the worker `bees status` reads names the mode.
	var worker state.Worker
	waitFor(t, 30*time.Second, "the worker to record its sandbox", func() bool {
		st, err := h.store.LoadStatus()
		if err != nil || len(st.Workers) == 0 {
			return false
		}
		worker = st.Workers[0]
		return worker.Sandbox != ""
	})
	if worker.Sandbox != config.SandboxNone {
		t.Errorf("worker sandbox: got %q, want %q", worker.Sandbox, config.SandboxNone)
	}

	cancel()
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}

	start, ok := find(drain(sub), EventSessionStarted, config.RoleDeveloper)
	if !ok {
		t.Fatal("no developer session-started event")
	}
	if start.Sandbox != config.SandboxNone {
		t.Errorf("session-started sandbox: got %q, want %q", start.Sandbox, config.SandboxNone)
	}
}

// setWorkerSandbox follows the session, not the worker: the stages of one
// worker run different roles, and a role's sandbox is its own, so the second
// session's mode replaces the first's rather than being ignored.
func TestSetWorkerSandboxFollowsTheRunningSession(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	w := &state.Worker{Name: "dev-1", Issue: 1, Stage: "develop"}
	h.sched.setWorkerSandbox(w, config.SandboxContainer)
	if w.Sandbox != config.SandboxContainer {
		t.Errorf("first session's sandbox: got %q, want %q", w.Sandbox, config.SandboxContainer)
	}
	h.sched.setWorkerSandbox(w, config.SandboxNone)
	if w.Sandbox != config.SandboxNone {
		t.Errorf("the next session's sandbox did not replace it: got %q", w.Sandbox)
	}
	// A session with no worker (a singleton, a requested review) is not one.
	h.sched.setWorkerSandbox(nil, config.SandboxNone)
}
