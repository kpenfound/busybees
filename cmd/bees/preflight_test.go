package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/doctor"
	"github.com/kpenfound/busybees/internal/versions"
)

// checkFor builds a check that records that it ran and reports status.
func checkFor(name string, group string, status doctor.Status, ran *bool, expensive bool) doctor.Check {
	return doctor.Check{
		Expensive: expensive,
		Run: func(context.Context) doctor.Result {
			*ran = true
			return doctor.Result{Name: name, Group: group, Status: status, Detail: "detail of " + name,
				Remediation: "fix " + name}
		},
	}
}

func TestPreflightRefusesToStartOnAFailure(t *testing.T) {
	var cheapRan, roleRan bool
	checks := []doctor.Check{
		// The acceptance case: `bees labels sync` was never run.
		checkFor("workflow labels", doctor.GroupGitHub, doctor.Fail, &cheapRan, false),
		checkFor("developer skills", doctor.GroupRoles, doctor.Pass, &roleRan, true),
	}
	out := captureStdout(t, func() {
		err := preflight(context.Background(), checks)
		if err == nil {
			t.Error("a failing cheap check must stop bees run")
			return
		}
		if !strings.Contains(err.Error(), "--skip-doctor") {
			t.Errorf("the error must say how to start anyway: %v", err)
		}
	})
	if !cheapRan {
		t.Error("the cheap check did not run")
	}
	// The expensive per-role checks clone skills and start MCP servers: adding
	// that to every `bees run` is exactly what the split exists to avoid.
	if roleRan {
		t.Error("bees run must not run the expensive role checks")
	}
	if !strings.Contains(out, "workflow labels") || !strings.Contains(out, "fix workflow labels") {
		t.Errorf("preflight must print the doctor table on failure:\n%s", out)
	}
}

func TestPreflightIsQuietWhenOnlyWarningsAreLeft(t *testing.T) {
	var ran bool
	checks := []doctor.Check{checkFor("filter matches issues", doctor.GroupGitHub, doctor.Warn, &ran, false)}
	out := captureStdout(t, func() {
		if err := preflight(context.Background(), checks); err != nil {
			t.Errorf("a warning must not stop bees run: %v", err)
		}
	})
	if !ran {
		t.Error("the check did not run")
	}
	if out != "" {
		t.Errorf("a start that works prints nothing:\n%s", out)
	}
}

// TestRunCommandFlags pins the flag the preflight is bypassed with, and that
// the debugging commands never offer it at all.
func TestRunCommandFlags(t *testing.T) {
	cmd := newRunCmd(&globalFlags{})
	if cmd.Flags().Lookup("skip-doctor") == nil {
		t.Fatal("bees run must offer --skip-doctor to bypass the preflight")
	}
	for _, name := range []string{"tick", "exec"} {
		var sub *cobra.Command
		switch name {
		case "tick":
			sub = newTickCmd(&globalFlags{})
		default:
			sub = newExecCmd(&globalFlags{})
		}
		// The debugging commands must stay usable on a half-configured
		// machine, so they neither run the preflight nor offer the flag.
		if sub.Flags().Lookup("skip-doctor") != nil {
			t.Errorf("bees %s must not run the preflight", name)
		}
	}
}

// `bees run` refuses to start while a role in the rotation asks for a sandbox
// bees cannot build here, and does so whatever --skip-doctor says: falling
// back to running that role unboxed would hand it exactly what it was
// configured to be kept away from. The developer role here asks for Claude
// Code's own sandbox with the codex agent, which cfg.CheckSandbox rejects
// without needing a container engine or any other host dependency, so the
// scenario is reached the same way with and without --skip-doctor.
func TestRunRefusesAnUnbuildableSandboxRegardlessOfSkipDoctor(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	path := writeProject(t, "acme/a", "[roles.developer]\nsandbox = \"claude\"\nagent = \"codex\"\n")
	const want = `sandbox "claude" is Claude Code's sandbox and agent "codex" does not run under it`

	for _, skipDoctor := range []bool{false, true} {
		t.Run(fmt.Sprintf("skip-doctor=%t", skipDoctor), func(t *testing.T) {
			args := []string{"run", "--config", path, "--no-tui", "--once"}
			if skipDoctor {
				args = append(args, "--skip-doctor")
			}
			// Bounded in case of a regression that lets CheckSandbox pass:
			// the run would then try to poll acme/a for real, which this
			// sandboxed test environment cannot reach.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			root := newRoot()
			root.SetArgs(args)
			root.SetOut(&bytes.Buffer{})
			root.SetErr(&bytes.Buffer{})
			err := root.ExecuteContext(ctx)
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("run: err = %v, want it to contain %q", err, want)
			}
		})
	}
}
