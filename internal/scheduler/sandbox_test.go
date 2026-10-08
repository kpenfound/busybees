package scheduler

// The scheduler harness's fakegh stands in for the GitHub API, and
// FAKE_CLAUDE's scripted developer, reviewer and singleton roles stand in
// for a model session, throughout this file's tests. Both leave real
// GitHub behaviour and a real model's judgment out of scope: what is
// proven here is only that the scheduler reacts correctly to what the
// harness is scripted to do.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/state"
)

// A session's box is a property of the session, so it rides on the
// session-started event the live view reads and on the worker `bees status`
// prints — not on the configuration a reader would have to go and resolve.
// This session runs with sandbox "none"; what this pins is that the
// resolved mode is carried at all, rather than left empty.
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
//
// setWorkerSandbox is a maintained contract: it is the sole writer of a live
// state.Worker's Sandbox field, and TestASessionReportsTheSandboxItRunsIn
// above only reaches the single-session case through a real dispatch. A
// second session replacing the first's mode on the same worker needs two
// roles racing one issue to reach through a full run; calling the writer
// directly is the economical way to pin that it replaces rather than merges.
func TestSetWorkerSandboxFollowsTheRunningSession(t *testing.T) {
	h := newHarness(t, devOnlyTOML)
	w := &state.Worker{Name: "dev-1", Stage: "develop", Work: ghwork.New(1, 0)}
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
