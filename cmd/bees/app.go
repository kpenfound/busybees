package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/skills"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/workspace"
)

// app wires the packages together for the scheduler commands.
type app struct {
	cfg    *config.Config
	store  *state.Store
	gh     *github.Client
	mail   *mail.Box
	runner *session.Runner
	ws     *workspace.Manager
	log    *slog.Logger
}

// configPath resolves the bees.toml to use.
func configPath(g *globalFlags) (string, error) {
	if g.config != "" {
		return filepath.Abs(g.config)
	}
	if p := os.Getenv(session.EnvConfig); p != "" {
		return p, nil
	}
	cwd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return config.Find(cwd)
}

// loadConfig loads bees.toml and derives repo/default branch from git.
func loadConfig(g *globalFlags) (*config.Config, error) {
	p, err := configPath(g)
	if err != nil {
		return nil, err
	}
	cfg, err := config.Load(p)
	if err != nil {
		return nil, err
	}
	if err := cfg.Resolve(context.Background()); err != nil {
		return nil, err
	}
	return cfg, nil
}

func newApp(ctx context.Context, g *globalFlags) (*app, error) {
	cfg, err := loadConfig(g)
	if err != nil {
		return nil, err
	}
	log := slog.Default()

	if _, err := workspace.Git(ctx, cfg.Dir(), "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s must live inside a git clone of %s: %w", cfg.Path, cfg.Project.Repo, err)
	}

	if cfg.Filter.Assignee == "@me" {
		login, err := github.CurrentUser(ctx)
		if err != nil {
			return nil, fmt.Errorf("resolve filter.assignee=@me: %w", err)
		}
		cfg.Filter.Assignee = login
	}

	store := state.New(cfg.StateDir())
	if err := store.Init(); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	claudeBin := os.Getenv("BEES_CLAUDE_BIN")
	if claudeBin == "" {
		claudeBin = "claude"
	}
	cache := os.Getenv("BEES_CACHE_DIR")
	if cache == "" {
		cache = skills.DefaultCacheDir()
	}
	ws := workspace.NewManager(cfg.Dir(), cfg.Scheduler.WorkspaceRoot)
	ws.Keep = cfg.Scheduler.KeepWorkspaces
	ws.Remote = cfg.Project.Remote

	runner := &session.Runner{
		ClaudeBin:   claudeBin,
		BeesBin:     self,
		SessionsDir: store.SessionsDir(),
		StateDir:    store.Dir,
		ConfigPath:  cfg.Path,
		Repo:        cfg.Project.Repo,
		Label:       cfg.Filter.Label,
		Skills:      skills.NewManager(cache),
		AddDirs:     []string{store.Dir},
		Logger:      log,
	}
	if g.verbose {
		runner.Stream = os.Stderr
	}
	return &app{
		cfg:    cfg,
		store:  store,
		gh:     github.New(cfg.Project.Repo),
		mail:   mail.Open(store.MailDir()),
		runner: runner,
		ws:     ws,
		log:    log,
	}, nil
}

func (a *app) scheduler() (*scheduler.Scheduler, error) {
	return scheduler.New(scheduler.Deps{
		Config:     a.cfg,
		GitHub:     a.gh,
		Mail:       a.mail,
		Runner:     a.runner,
		Workspaces: a.ws,
		Store:      a.store,
		Logger:     a.log,
	})
}
