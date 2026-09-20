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
				d := doctor.New(ctx, p.app.cfg.Path, claudeBin(), codexBin(), opencodeBin(), piBin())
				if err := loggedPreflight(ctx, d.Checks(), p.app.log); err != nil {
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
	rt := newMachineRuntime(g, m)
	build := func(next *config.Machine) []daemon.Project { return opts.projects(rt.projects(next)) }
	d := &daemon.Daemon{Logger: slog.Default(), Projects: build(m)}
	ctx, cancel := context.WithCancel(cmd.Context())
	defer cancel()
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)
	var view *machineViews
	if logTUIMode(d.Logger, noTUI, os.Stdout) {
		view = newMachineView(ctx, d)
		defer view.close()
		d.Projects = view.wrap(m, d.Projects)
	}
	if !opts.once {
		changes := make(chan []daemon.Project)
		d.Reload = changes
		reloader := &machineReloader{path: m.Path, apply: rt.reloadProjects, changes: changes, log: d.Logger,
			build: func(next *config.Machine) []daemon.Project {
				projects := build(next)
				if view != nil {
					projects = view.wrap(next, projects)
				}
				return projects
			}}
		go reloader.serve(ctx, hup)
		if view != nil {
			view.reload = func() error { return reloader.reload(ctx) }
		}
	}
	// SIGTERM is registered by main; register SIGHUP before exposing the pid.
	cleanup, err := registerDaemonChild(filepath.Join(filepath.Dir(m.Path), MachinePIDFile))
	if err != nil {
		return err
	}
	defer cleanup()
	defer hardStopOnSecondInterrupt(d.HardStop)()
	if view != nil {
		return runPreparedMachineView(ctx, g, cmd.ErrOrStderr(), d, view)
	}
	return d.Run(ctx)
}
