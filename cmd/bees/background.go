package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

const (
	// MachinePIDFile is next to the active machine config; ProjectPIDFile is
	// under the project's StateDir. Both contain a decimal pid and a newline.
	MachinePIDFile = "bees-machine.pid"
	ProjectPIDFile = "bees.pid"
	daemonChildEnv = "BEES_DAEMON_CHILD"
)

type runConfig struct {
	path    string
	machine *config.Machine
	project *config.Config
}

func resolveRunConfig(g *globalFlags) (*runConfig, error) {
	path, err := configPath(g)
	if err != nil {
		return nil, err
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	kind, err := config.DetectKind(path)
	if err != nil {
		return nil, err
	}
	active := &runConfig{path: path}
	if kind == config.KindMachine {
		active.machine, err = config.LoadMachine(path)
	} else {
		active.project, err = config.Load(path)
	}
	return active, err
}

func (a *runConfig) pidPath() string {
	if a.machine != nil {
		return filepath.Join(filepath.Dir(a.path), MachinePIDFile)
	}
	return filepath.Join(a.project.StateDir(), ProjectPIDFile)
}

// childArgs preserves parsed flag values, including a value that happens to
// look like -d. Rebuilding from Cobra also handles combined short flags.
func childArgs(cmd *cobra.Command, path string) []string {
	args := []string{"run", "--config=" + path}
	cmd.Flags().Visit(func(f *pflag.Flag) {
		if f.Name != "daemon" && f.Name != "config" {
			args = append(args, "--"+f.Name+"="+f.Value.String())
		}
	})
	return args
}

func detachRun(cmd *cobra.Command, active *runConfig) error {
	pidPath := active.pidPath()
	if err := os.MkdirAll(filepath.Dir(pidPath), 0o755); err != nil {
		return err
	}
	// A separate, persistent lock inode prevents concurrent starts, including
	// the window between exec and the child writing its pid. The child inherits
	// the lock on fd 3 and holds it until its graceful shutdown is complete.
	lock, err := os.OpenFile(pidPath+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = lock.Close() }()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("factory already running for %s: %w", active.path, err)
	}
	logPath := pidPath[:len(pidPath)-len(".pid")] + "-daemon.log"
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer func() { _ = log.Close() }()
	binary, err := os.Executable()
	if err != nil {
		return err
	}
	child := exec.Command(binary, childArgs(cmd, active.path)...)
	child.Env = append(os.Environ(), daemonChildEnv+"=1")
	child.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	child.Stdout, child.Stderr = log, log
	child.ExtraFiles = []*os.File{lock}
	if err := child.Start(); err != nil {
		return err
	}
	pid := child.Process.Pid
	if err := child.Process.Release(); err != nil {
		return err
	}
	_, err = fmt.Fprintf(cmd.OutOrStdout(), "%d\n", pid)
	return err
}

// registerDaemonChild is called after signal handlers are installed. The
// lock survives the parent exiting, and the pid disappears only after drain.
func registerDaemonChild(path string) (func(), error) {
	if os.Getenv(daemonChildEnv) != "1" {
		return func() {}, nil
	}
	lock := os.NewFile(3, "daemon-lock")
	if lock == nil {
		return nil, fmt.Errorf("daemon child has no inherited lock")
	}
	if _, err := lock.Stat(); err != nil {
		_ = lock.Close()
		return nil, err
	}
	// gh and agent subprocesses must not keep the daemon lock alive.
	syscall.CloseOnExec(3)
	pidFile, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_RDWR, 0o600)
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	cleanup := func() { _ = os.Remove(path); _ = pidFile.Close(); _ = lock.Close() }
	if _, err := fmt.Fprintf(pidFile, "%d\n", os.Getpid()); err != nil {
		cleanup()
		return nil, err
	}
	// A process-owned record lock on the PID file exposes its owner's PID
	// through F_GETLK. Publish it only after writing our PID and installing
	// signal handlers, so controls can distinguish startup from a live daemon.
	// Keep it separate from flock: the two lock types interact on macOS.
	owner := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0}
	if err := syscall.FcntlFlock(pidFile.Fd(), syscall.F_SETLK, &owner); err != nil {
		cleanup()
		return nil, err
	}
	return cleanup, nil
}
