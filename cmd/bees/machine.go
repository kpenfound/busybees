package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/spf13/cobra"
)

func newMachineCmd(g *globalFlags) *cobra.Command {
	cmd := groupCmd("machine", "Inspect and control a multi-project daemon")
	cmd.Long = "Inspect and control a daemon using the active machine config.\nSelect it with --config, $BEES_CONFIG, or the upward bees.toml search."
	cmd.AddCommand(newMachineStatusCmd(g), newMachineCostCmd(g),
		newMachineControlCmd(g, false, syscall.Kill), newMachineControlCmd(g, true, syscall.Kill))
	return cmd
}

// Controls need only the machine config's identity. In particular, a removed
// project config must not prevent stopping the daemon or requesting a reload.
func activeMachine(g *globalFlags) (*runConfig, error) {
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
	if kind != config.KindMachine {
		return nil, fmt.Errorf("%s: bees machine requires an active machine config listing projects; select it with --config", path)
	}
	return &runConfig{path: path, machine: &config.Machine{Path: path}}, nil
}

func machineConfig(g *globalFlags) (*config.Machine, error) {
	active, err := activeMachine(g)
	if err != nil {
		return nil, err
	}
	return config.LoadMachine(active.path)
}

func newMachineStatusCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use: "status", Short: "List each project's recorded status",
		Long: "Read each project's status.json using the active machine config. Missing or unreadable status is unavailable; no schedulers are started.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := machineConfig(g)
			if err != nil {
				return err
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 4, 2, ' ', 0)
			if _, err := fmt.Fprintln(w, "project\tstatus\tupdated\tworkers\tlast error"); err != nil {
				return err
			}
			for _, cfg := range m.Configs {
				st, err := state.New(cfg.StateDir()).LoadStatus()
				// LoadStatus treats a missing file as an empty snapshot.
				if err != nil || st.UpdatedAt.IsZero() {
					if _, err := fmt.Fprintf(w, "%s\tunavailable\t-\t-\t-\n", cfg.Path); err != nil {
						return err
					}
					continue
				}
				if _, err := fmt.Fprintf(w, "%s\trecorded\t%s\t%d\t%s\n", cfg.Path, st.UpdatedAt.Format(time.RFC3339), len(st.Workers), strings.Join(strings.Fields(st.LastError), " ")); err != nil {
					return err
				}
			}
			return w.Flush()
		},
	}
}

func newMachineCostCmd(g *globalFlags) *cobra.Command {
	var since time.Duration
	cmd := &cobra.Command{
		Use: "cost", Short: "Sum finished session costs across projects",
		Long: "Read each project's session ledger using the active machine config and print a row per project plus a total. No schedulers are started.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			m, err := machineConfig(g)
			if err != nil {
				return err
			}
			cutoff := time.Now().Add(-since)
			var rows []costGroup
			total := costGroup{Group: "total"}
			for _, cfg := range m.Configs {
				entries, err := state.New(cfg.StateDir()).ReadLedger(cutoff)
				if err != nil {
					return fmt.Errorf("%s: read costs: %w", cfg.Path, err)
				}
				_, row := groupCost(entries, byRole)
				row.Group = cfg.Path
				rows = append(rows, row)
				total.Sessions += row.Sessions
				total.Turns += row.Turns
				total.CostUSD += row.CostUSD
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), costText("project", rows, total))
			return err
		},
	}
	cmd.Flags().DurationVar(&since, "since", 24*time.Hour, "how far back to look (Go duration)")
	return cmd
}

type machineSignal func(int, syscall.Signal) error

func newMachineControlCmd(g *globalFlags, reload bool, signal machineSignal) *cobra.Command {
	use, short := "stop", "Stop the daemon and wait for its projects to drain"
	if reload {
		use, short = "reload", "Ask the daemon to reload its project list"
	}
	return &cobra.Command{
		Use: use, Short: short,
		Long: short + ". Uses bees-machine.pid beside the active machine config. Requires a detached machine run (bees run -d).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			active, err := activeMachine(g)
			if err != nil {
				return err
			}
			pidPath := active.pidPath()
			pid, err := machinePID(pidPath)
			if err != nil {
				return err
			}
			if pid != 0 {
				sig := syscall.SIGTERM
				if reload {
					sig = syscall.SIGHUP
				}
				err = signal(pid, sig)
				if errors.Is(err, syscall.ESRCH) {
					pid = 0
				} else if err != nil {
					return fmt.Errorf("%s daemon %d: %w", use, pid, err)
				}
			}
			if pid == 0 {
				if reload {
					return fmt.Errorf("no running machine daemon for %s; start it with bees run --config %s -d", active.path, active.path)
				}
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "no running machine daemon")
				return err
			}
			if reload {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "reload requested for machine daemon %d\n", pid)
				return err
			}
			if err := waitMachine(cmd.Context(), pidPath, pid, signal); err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), "machine daemon stopped")
			return err
		},
	}
}

var errMachineStarting = errors.New("machine daemon is starting or has not published its PID; retry the command")

// The inherited flock spans startup and drain. The child's record lock binds
// the published PID to its kernel-reported owner, excluding stale PID files
// while a replacement daemon is starting.
func machinePID(path string) (int, error) {
	lock, err := os.OpenFile(path+".lock", os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = lock.Close() }()
	err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if err == nil {
		_ = syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
		return 0, nil
	}
	if !errors.Is(err, syscall.EWOULDBLOCK) {
		return 0, err
	}
	pidFile, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, errMachineStarting
	}
	if err != nil {
		return 0, err
	}
	defer func() { _ = pidFile.Close() }()
	data, err := io.ReadAll(pidFile)
	if err != nil {
		return 0, err
	}
	owner := syscall.Flock_t{Type: syscall.F_WRLCK, Whence: 0}
	if err := syscall.FcntlFlock(pidFile.Fd(), syscall.F_GETLK, &owner); err != nil {
		return 0, err
	}
	if owner.Type == syscall.F_UNLCK {
		return 0, errMachineStarting
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return 0, fmt.Errorf("%s: invalid daemon pid %q", path, strings.TrimSpace(string(data)))
	}
	if int(owner.Pid) != pid {
		return 0, errMachineStarting
	}
	return pid, nil
}

func waitMachine(ctx context.Context, path string, pid int, signal machineSignal) error {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		current, err := machinePID(path)
		// Startup without the old owner's published PID means its drain has
		// completed, even if a replacement already holds the inherited flock.
		if errors.Is(err, errMachineStarting) {
			return nil
		}
		if err != nil {
			return err
		}
		// The daemon removes its pid and releases its lock only after every
		// project stops. A new daemon at this path is not the one we stopped.
		if current != pid {
			return nil
		}
		if err := signal(pid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		} else if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
