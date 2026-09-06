package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
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

// `bees run` refuses to start while a role in the rotation asks for a sandbox
// bees cannot build, and asks before the doctor and whatever --skip-doctor
// says: falling back to running that role unboxed would hand it exactly what
// it was configured to be kept away from. RunE cannot be exercised here (see
// TestRunCommandFlags), so the guard is read out of the command's own source.
func TestRunChecksTheSandboxAheadOfTheDoctor(t *testing.T) {
	body := funcSource(t, "commands.go", "newRunCmd")
	check := strings.Index(body, "cfg.CheckSandbox()")
	if check < 0 {
		t.Fatal("bees run does not call Config.CheckSandbox: a role configured for a sandbox bees cannot build would run unboxed")
	}
	skip := strings.Index(body, "if !skipDoctor")
	if skip < 0 {
		t.Fatal("bees run no longer guards the doctor preflight with --skip-doctor; this test reads that line to place the sandbox check")
	}
	if check > skip {
		t.Error("the sandbox check runs after the doctor preflight, so --skip-doctor bypasses it too")
	}
}

// funcSource returns the source text of the named top-level function in the
// named file of this package.
func funcSource(t *testing.T, file, name string) string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		return string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
	}
	t.Fatalf("no func %s in %s", name, file)
	return ""
}
