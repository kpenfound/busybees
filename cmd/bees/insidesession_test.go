package main

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/session"
)

// TestMain clears the variables a bees session exports, the way
// internal/scheduler's does: `go test` run from inside a session must behave
// like one run from a plain shell, or the commands that refuse to run
// inside one would refuse here.
func TestMain(m *testing.M) {
	for _, k := range []string{session.EnvRole, session.EnvSessionDir, session.EnvStateDir, session.EnvRepo,
		session.EnvLabel, session.EnvIssue, session.EnvPR, session.EnvBranch,
		session.EnvConfig, session.EnvBin} {
		if err := os.Unsetenv(k); err != nil {
			fmt.Fprintln(os.Stderr, "unset:", err)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}

// The commands that start sessions refuse to do so from inside one: a
// factory a bee started would run agents nothing watches.
func TestRunTickAndExecRefuseInsideASession(t *testing.T) {
	t.Setenv(session.EnvSessionDir, "/state/sessions/developer-issue-1-r1")
	for _, args := range [][]string{
		{"run"},
		{"run", "--no-tui", "--skip-doctor", "--config", "/nowhere/bees.toml"},
		{"tick"},
		{"exec", "developer", "--issue", "1"},
	} {
		err := runRoot(t, args...)
		if err == nil || !strings.Contains(err.Error(), "inside a bee's session") {
			t.Errorf("%v: err = %v, want the refusal", args, err)
			continue
		}
		for _, want := range []string{"bees " + args[0], session.EnvSessionDir + "=/state/sessions/developer-issue-1-r1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%v: error %q does not name %q", args, err, want)
			}
		}
	}
}
