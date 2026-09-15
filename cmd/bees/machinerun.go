package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/daemon"
	"github.com/kpenfound/busybees/internal/doctor"
	"github.com/spf13/cobra"
)

type runOptions struct {
	once       bool
	roles      string
	skipDoctor bool
}

func (o runOptions) projects(projects []daemon.Project) []daemon.Project {
	for i := range projects {
		start := projects[i].Start
		projects[i].Start = func(ctx context.Context) (daemon.Loop, error) {
			l, err := start(ctx)
			if err != nil {
				return nil, err
			}
			p := l.(*projectLoop)
			if !o.skipDoctor {
				d := doctor.New(ctx, p.app.cfg.Path, claudeBin(), codexBin(), opencodeBin())
				if err := preflight(ctx, d.Checks()); err != nil {
					_ = p.Close()
					return nil, err
				}
			}
			p.Once = o.once
			p.OnlyRoles, err = parseRoles(o.roles)
			if err != nil {
				_ = p.Close()
				return nil, err
			}
			return p, nil
		}
	}
	return projects
}

func runMachine(cmd *cobra.Command, g *globalFlags, m *config.Machine, opts runOptions, noTUI bool) error {
	build := machineProjectFactory(g, m)
	d := &daemon.Daemon{Logger: slog.Default(), Projects: opts.projects(build(m))}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	if !opts.once {
		changes := make(chan []daemon.Project)
		d.Reload = changes
		go reloadMachine(ctx, hup, changes, m.Path, func(next *config.Machine) []daemon.Project { return opts.projects(build(next)) }, d.Logger)
	}
	// SIGTERM is registered by main; register SIGHUP before exposing the pid.
	cleanup, err := registerDaemonChild(filepath.Join(filepath.Dir(m.Path), MachinePIDFile))
	if err != nil {
		return err
	}
	defer cleanup()
	defer hardStopOnSecondInterrupt(d.HardStop)()
	if logTUIMode(d.Logger, noTUI, os.Stdout) {
		return runPreparedMachineWithTUI(ctx, g, m, cmd.ErrOrStderr(), d)
	}
	return d.Run(ctx)
}

// A malformed reload leaves the entire running set untouched. Only membership
// is reloaded: unchanged projects keep their config and the original pool.
func reloadMachine(ctx context.Context, hup <-chan os.Signal, changes chan<- []daemon.Project, path string, build func(*config.Machine) []daemon.Project, log *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			m, err := config.LoadMachine(path)
			if err != nil {
				log.Error("reload machine config", "err", err)
				continue
			}
			select {
			case changes <- build(m):
				log.Info("reloaded machine projects", "config", path)
			case <-ctx.Done():
				return
			}
		}
	}
}
