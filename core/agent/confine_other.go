//go:build !linux && !darwin

package agent

import (
	"fmt"
	"os/exec"
	"runtime"
)

// DefaultSystemPaths are what a confined turn reaches beyond its mounts.
// This platform has no confiner, and so no list.
func DefaultSystemPaths() []Mount { return nil }

// platformConfiner refuses: nothing on this platform confines a host session.
func platformConfiner() Confiner {
	return unsupportedConfiner{reason: "nothing confines a host session on " + runtime.GOOS}
}

// unsupportedConfiner is the confiner of a platform that has none.
type unsupportedConfiner struct{ reason string }

func (u unsupportedConfiner) Check(Confinement) error {
	return fmt.Errorf("%w: %s", ErrUnsupported, u.reason)
}

func (u unsupportedConfiner) Start(_ *exec.Cmd, c Confinement) error { return u.Check(c) }
