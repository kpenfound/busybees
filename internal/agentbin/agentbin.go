// Package agentbin resolves the executable of a coding agent — claude,
// codex or opencode — before a session runs it, and keeps a test binary
// from ever running a real one.
//
// Every test that runs a session fakes its agent, with a script under the
// test's temporary directory or with the test binary itself (see
// CONTRIBUTING.md). A test that forgets to, or a test binary re-executed as
// a command of its own (`bees.test run`), would otherwise find the real
// claude on PATH and run it: for real, unwatched, and as many times as the
// scheduler it started asks. Resolve refuses that. In a binary built by
// `go test` the executable must be one of those two fakes, and anything
// else is ErrRealAgent, however the binary was invoked: testing.Testing
// reports the build, not the command line.
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

// ErrRealAgent is the error Resolve wraps when a test binary would run an
// agent that is not one of the fakes tests make.
var ErrRealAgent = errors.New("refusing to run a real coding agent from a test binary")

// Resolve returns the executable exec would run for bin: bin itself when it
// names a path, else the first match on PATH. A bin that cannot be found is
// returned as it is, so the caller fails the way exec does when it starts
// it. In a test binary the executable must be a fake, the test binary
// itself or a file under the temporary directory, or the error wraps
// ErrRealAgent.
func Resolve(bin string) (string, error) {
	path, err := exec.LookPath(bin)
	if err != nil {
		return bin, nil
	}
	if !testing.Testing() || isFake(path) {
		return path, nil
	}
	return "", fmt.Errorf("%w: %s is %s; tests fake the agent with a script under t.TempDir() or the test binary itself (see CONTRIBUTING.md)",
		ErrRealAgent, bin, path)
}

// isFake reports whether path is one of the executables a test may run as
// an agent: the running binary, or a file under the temporary directory.
func isFake(path string) bool {
	if self, err := os.Executable(); err == nil && sameFile(self, path) {
		return true
	}
	return under(os.TempDir(), path)
}

func sameFile(a, b string) bool {
	fa, err := os.Stat(a)
	if err != nil {
		return false
	}
	fb, err := os.Stat(b)
	if err != nil {
		return false
	}
	return os.SameFile(fa, fb)
}

// under reports whether path lies inside dir, both with their symlinks
// resolved: the temporary directory is one on macOS.
func under(dir, path string) bool {
	dir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return false
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
