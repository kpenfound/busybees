package agentbin

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain lets the test binary be re-executed as a command of its own, the
// way a test of `bees run` that starts the real command would: with
// AGENTBIN_CHILD set it resolves the agent named there and prints the
// verdict instead of running the tests.
func TestMain(m *testing.M) {
	if bin := os.Getenv("AGENTBIN_CHILD"); bin != "" {
		if _, err := Resolve(bin); err != nil {
			fmt.Println("refused:", err)
			os.Exit(0)
		}
		fmt.Println("resolved")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// realBinary is an executable on this machine that no test made: what a
// forgotten fake resolves to.
func realBinary(t *testing.T) string {
	t.Helper()
	p, err := exec.LookPath("sh")
	if err != nil {
		t.Skip("no sh on PATH")
	}
	return p
}

func TestARealAgentIsRefusedFromATestBinary(t *testing.T) {
	sh := realBinary(t)
	for _, bin := range []string{sh, filepath.Base(sh)} {
		_, err := Resolve(bin)
		if !errors.Is(err, ErrRealAgent) {
			t.Errorf("Resolve(%q) = %v, want ErrRealAgent", bin, err)
		}
		if err != nil && !strings.Contains(err.Error(), sh) {
			t.Errorf("the error does not name the executable: %v", err)
		}
	}
}

func TestAFakeUnderTheTemporaryDirectoryResolves(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "claude")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// By path, and by name through PATH, which is how the runner is given
	// it when nothing names the executable.
	t.Setenv("PATH", dir)
	for _, bin := range []string{fake, "claude"} {
		got, err := Resolve(bin)
		if err != nil {
			t.Fatalf("Resolve(%q): %v", bin, err)
		}
		if got != fake {
			t.Errorf("Resolve(%q) = %q, want %q", bin, got, fake)
		}
	}
}

func TestTheTestBinaryItselfResolves(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// Named directly, as internal/scheduler's fake is, and through a
	// symlink elsewhere: the file is what counts.
	link := filepath.Join(t.TempDir(), "claude")
	if err := os.Symlink(self, link); err != nil {
		t.Fatal(err)
	}
	for _, bin := range []string{os.Args[0], self, link} {
		if _, err := Resolve(bin); err != nil {
			t.Errorf("Resolve(%q): %v", bin, err)
		}
	}
}

func TestAMissingAgentIsReturnedAsItIs(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got, err := Resolve("no-such-agent")
	if err != nil || got != "no-such-agent" {
		t.Errorf("Resolve = %q, %v; want the name back and no error", got, err)
	}
}

// TestARefusalSurvivesReexecution pins the shape of the incident the guard
// exists for: a test binary started again as a program of its own, with
// nothing on PATH but the machine's real executables, must still refuse.
func TestARefusalSurvivesReexecution(t *testing.T) {
	sh := realBinary(t)
	cmd := exec.Command(os.Args[0])
	cmd.Env = append(os.Environ(), "AGENTBIN_CHILD="+sh)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if !strings.HasPrefix(string(out), "refused: "+ErrRealAgent.Error()) {
		t.Errorf("the re-executed test binary said:\n%s", out)
	}
}
