package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/daemon"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/scheduler"
)

// machineDaemon builds the daemon that runs the scheduler of every project m
// lists, each built the way a single-project run builds its own: its own
// bees.toml, state directory, mailbox, notes and poll loop. The projects
// share the console; each keeps its own <state_dir>/bees.log, and its
// records carry a project attribute naming its repository. With the machine
// config's max_developers set they share one developer pool of that size
// too (scheduler.SharedPool), on top of each project's own.
func machineDaemon(g *globalFlags, m *config.Machine) *daemon.Daemon {
	d := &daemon.Daemon{Logger: slog.Default()}
	var shared *scheduler.SharedPool
	if m.MaxDevelopers > 0 {
		shared = scheduler.NewSharedPool(m.MaxDevelopers)
	}
	for _, cfg := range m.Configs {
		d.Projects = append(d.Projects, daemon.Project{
			Name:  cfg.Path,
			Start: func(ctx context.Context) (daemon.Loop, error) { return startProject(ctx, g, cfg, shared) },
		})
	}
	return d
}

// projectLoop is one project's scheduler, closing the project's log file when
// its loop returns.
type projectLoop struct {
	*scheduler.Scheduler
	app  *app
	file io.Closer
}

func (p *projectLoop) Run(ctx context.Context) error {
	defer func() { _ = p.Close() }()
	return p.Scheduler.Run(ctx)
}

// Close releases the project's log file: after Run, or instead of it when
// the daemon discards a loop it never ran (see daemon.Loop).
func (p *projectLoop) Close() error { return p.file.Close() }

// The daemon closes a loop it discards unrun only through io.Closer.
var _ io.Closer = (*projectLoop)(nil)

// startProject builds one project's scheduler. The project's [logging] table
// is not applied: one console serves every project, so no project's table
// decides its format or level.
func startProject(ctx context.Context, g *globalFlags, cfg *config.Config, pool *scheduler.SharedPool) (*projectLoop, error) {
	if err := cfg.Resolve(ctx); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.StateDir(), 0o755); err != nil {
		return nil, err
	}
	shared := g.logger
	if shared == nil {
		shared = logging.New(logging.Options{})
	}
	log, file, err := shared.Tee(filepath.Join(cfg.StateDir(), "bees.log"))
	if err != nil {
		return nil, fmt.Errorf("open the log file: %w", err)
	}
	log = log.With("project", cfg.Project.Repo)
	loop, err := buildProject(ctx, g, cfg, log, pool)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	loop.file = file
	return loop, nil
}

func buildProject(ctx context.Context, g *globalFlags, cfg *config.Config, log *slog.Logger, pool *scheduler.SharedPool) (*projectLoop, error) {
	a, err := newAppFor(ctx, g, cfg, log)
	if err != nil {
		return nil, err
	}
	a.shared = pool
	// The project's bees.log is the tee's file: attaching it to the shared
	// logger would write every project's records into it.
	a.logger = nil
	// As in `bees run`: a role whose sandbox cannot be built here must not
	// run unboxed.
	if err := a.cfg.CheckSandbox(); err != nil {
		return nil, err
	}
	s, err := a.scheduler()
	if err != nil {
		return nil, err
	}
	return &projectLoop{Scheduler: s, app: a}, nil
}
