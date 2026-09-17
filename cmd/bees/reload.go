package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/daemon"
	"github.com/kpenfound/busybees/internal/scheduler"
)

// reloadProjectConfig reads a project's bees.toml again, the way `bees run`
// read it when it started: loaded, resolved against the clone,
// filter.assignee = "@me" resolved, and the sandbox every role asks for
// checked. [logging] is not applied: it is one of the keys a reload cannot
// change (scheduler.CheckReload), so the console is what it was.
func reloadProjectConfig(ctx context.Context, path string) (*config.Config, error) {
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	if err := prepareReload(ctx, cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// prepareReload resolves a loaded bees.toml for a running scheduler the way
// its start resolved the one it runs on, so the two compare key by key.
func prepareReload(ctx context.Context, cfg *config.Config) error {
	if err := cfg.Resolve(ctx); err != nil {
		return err
	}
	if err := resolveFilterAssignee(ctx, cfg); err != nil {
		return err
	}
	// As at start: a role whose sandbox cannot be built here must not run
	// unboxed, and a reload asking for one is refused whole.
	return cfg.CheckSandbox()
}

// projectReloader is the live view's reload for a single project (tui.Deps
// Reload): the project's bees.toml read again and handed to its scheduler,
// which takes it from its next pass or refuses it and keeps what it has.
func projectReloader(ctx context.Context, a *app, s *scheduler.Scheduler) func() error {
	return func() error {
		cfg, err := reloadProjectConfig(ctx, a.cfg.Path)
		if err != nil {
			a.log.Error("reload bees.toml", "path", a.cfg.Path, "err", err)
			return err
		}
		if err := s.Reload(cfg); err != nil {
			a.log.Error("reload bees.toml", "path", a.cfg.Path, "err", err)
			return err
		}
		return nil
	}
}

// machineReloader is the reload of a running machine, what SIGHUP and the
// live view's r key both run: the machine config read again, every listed
// project that is running handed its bees.toml read again, and the project
// list handed to the daemon to reconcile. It is all or nothing: a machine
// config that does not load, or a project's bees.toml that does not load or
// changes a key its scheduler cannot (scheduler.CheckReload), leaves every
// project and the project list untouched, and the error says which file and
// why.
type machineReloader struct {
	path string
	// build turns the machine config into the daemon's project list.
	build func(*config.Machine) []daemon.Project
	// apply hands the running projects their bees.toml read again.
	apply func(context.Context, *config.Machine) error
	// changes is the daemon's Reload channel.
	changes chan<- []daemon.Project
	log     *slog.Logger
}

// reload runs one reload and reports how it went.
func (r *machineReloader) reload(ctx context.Context) error {
	m, err := config.LoadMachine(r.path)
	if err != nil {
		r.log.Error("reload machine config", "err", err)
		return err
	}
	if err := r.apply(ctx, m); err != nil {
		r.log.Error("reload machine config", "config", r.path, "err", err)
		return err
	}
	select {
	case r.changes <- r.build(m):
		r.log.Info("reloaded machine config", "config", r.path)
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// serve runs a reload for every SIGHUP until ctx is cancelled; what each
// came to is in the log.
func (r *machineReloader) serve(ctx context.Context, hup <-chan os.Signal) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-hup:
			_ = r.reload(ctx)
		}
	}
}

// reloadProjects is machineReloader.apply for the projects of a
// machineRuntime: every project of m whose scheduler is running is handed
// its bees.toml read again. A project that is not running — listed for the
// first time, or still starting — is left to start on the file as it is
// now. Every project's file is checked before any project's is applied.
func (r *machineRuntime) reloadProjects(ctx context.Context, m *config.Machine) error {
	type reload struct {
		loop *projectLoop
		cfg  *config.Config
	}
	var (
		errs    []error
		pending []reload
	)
	for _, cfg := range m.Configs {
		loop := r.running(cfg.Path)
		if loop == nil {
			continue
		}
		if err := prepareReload(ctx, cfg); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", cfg.Path, err))
			continue
		}
		if err := loop.CheckReload(cfg); err != nil {
			errs = append(errs, err)
			continue
		}
		pending = append(pending, reload{loop, cfg})
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	for _, p := range pending {
		if err := p.loop.Reload(p.cfg); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
