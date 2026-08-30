package main

import (
	"context"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/doctor"
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

// TestRunCommandFlags pins the flag the preflight is bypassed with: the
// command's RunE cannot be exercised in a test (newApp needs a repository, a
// real gh and a real claude), so the flag itself is what is guarded here.
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
