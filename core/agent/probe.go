package agent

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
)

// A prober runs a short command of the agent's before the turn, where the
// turn's own command will run and with what it will see: on the host with
// the turn's environment, under the turn's confinement, or in the turn's
// container or sandbox. extra are the backend's variables, laid over the
// turn's environment as they are for the turn. It returns what the command
// printed on stdout. A backend runs one to inspect what the agent will be
// before the model starts (opencode's effective configuration), so that
// configuration only the turn's own placement can see is inspected too.
type prober func(ctx context.Context, bin string, args []string, extra []envVar) ([]byte, error)

// hostProber runs a probe as a process of this host, confined the way the
// turn is when it is.
func (r *Runner) hostProber(req Request, turn *Turn) prober {
	return func(ctx context.Context, bin string, args []string, extra []envVar) ([]byte, error) {
		resolved, err := agentbin.Resolve(bin)
		if err != nil {
			return nil, err
		}
		cmd := exec.CommandContext(ctx, resolved, args...)
		cmd.Dir = req.workDir()
		cmd.Env = environmentWith(turn.Env, extra)
		start := cmd.Start
		if turn.Confinement != nil {
			confinement, err := turn.Confinement.withExecutable(resolved)
			if err == nil && r.held != nil {
				err = r.held.admitExecutable(resolved)
			}
			if err != nil {
				return nil, err
			}
			confiner := r.Confiner
			if confiner == nil {
				confiner = platformConfiner()
			}
			start = func() error { return confiner.Start(cmd, confinement) }
		}
		return runProbe(cmd, start, nil)
	}
}

// runProbe starts cmd with start (cmd.Start when nil) in a process group of
// its own and returns its stdout. cleanup, when set, runs before the group
// is killed for a cancelled context: what the process started outside it,
// a container, is removed.
func runProbe(cmd *exec.Cmd, start func() error, cleanup func()) ([]byte, error) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cleanup != nil {
			cleanup()
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 10 * time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if start == nil {
		start = cmd.Start
	}
	if err := start(); err != nil {
		return nil, err
	}
	if err := cmd.Wait(); err != nil {
		return nil, fmt.Errorf("%w%s", err, stderrTail(stderr.String()))
	}
	return stdout.Bytes(), nil
}
