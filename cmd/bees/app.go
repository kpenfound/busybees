package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/skills"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/versions"
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
	logger *logging.Logger
}

// claudeBin is the claude executable sessions are run with.
func claudeBin() string {
	if bin := os.Getenv("BEES_CLAUDE_BIN"); bin != "" {
		return bin
	}
	return "claude"
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
	// The console logger was built from the flags before any command ran, so
	// [logging] can only be applied now. Every command that reads bees.toml
	// comes through here.
	g.applyLogging(cfg.Logging)
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

	if cfg.NeedsRewrite() {
		from := cfg.MigratedFrom
		backup, err := cfg.Rewrite()
		if err != nil {
			return nil, fmt.Errorf("migrate %s: %w", cfg.Path, err)
		}
		log.Info("migrated bees.toml", "from", from, "to", config.CurrentVersion, "backup", backup)
	}

	if _, err := workspace.Git(ctx, cfg.Dir(), "rev-parse", "--git-dir"); err != nil {
		return nil, fmt.Errorf("%s must live inside a git clone of %s: %w", cfg.Path, cfg.Project.Repo, err)
	}

	if err := resolveFilterAssignee(ctx, cfg); err != nil {
		return nil, err
	}

	store := state.New(cfg.StateDir())
	if err := store.Init(); err != nil {
		return nil, err
	}
	self, err := os.Executable()
	if err != nil {
		return nil, err
	}
	bin := claudeBin()
	if err := versions.CheckAll(ctx, bin); err != nil {
		return nil, err
	}
	skillMgr := skills.NewManager(cacheDir())
	skillMgr.RefreshAlways, skillMgr.RefreshAfter = cfg.SkillsRefresh()
	skillMgr.Logger = log

	ws := workspace.NewManager(cfg.Dir(), cfg.Scheduler.WorkspaceRoot)
	ws.Keep = cfg.Scheduler.KeepWorkspaces
	ws.Remote = cfg.Project.Remote

	runner := &session.Runner{
		ClaudeBin:   bin,
		BeesBin:     self,
		SessionsDir: store.SessionsDir(),
		StateDir:    store.Dir,
		ConfigPath:  cfg.Path,
		Repo:        cfg.Project.Repo,
		Label:       cfg.Filter.Label,
		GitHub:      cfg.GitHub,
		Skills:      skillMgr,
		AddDirs:     []string{store.Dir},
		Logger:      log,
	}
	if g.verbose {
		runner.Stream = os.Stderr
	}
	return &app{
		cfg:    cfg,
		store:  store,
		gh:     githubClient(cfg),
		mail:   mail.Open(store.MailDir()),
		runner: runner,
		ws:     ws,
		log:    log,
		logger: g.logger,
	}, nil
}

func (a *app) scheduler() (*scheduler.Scheduler, error) {
	// Only the commands that run sessions write the log file: `bees issue`
	// and `bees mail` run inside sessions, concurrently with the scheduler,
	// and must not race its rotation.
	attachLogFile(a.logger, a.log, filepath.Join(a.store.Dir, "bees.log"))
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

// attachLogFile adds the state directory's log file to l. A state directory
// that is read-only or full must never stop the factory over a diagnostic
// file, so a failure to open it is a warning and the run continues with
// console logging only. Warn so it survives --quiet; scheduler() runs once per
// invocation, so it is printed once.
func attachLogFile(l *logging.Logger, log *slog.Logger, path string) {
	if l == nil {
		return
	}
	if err := l.AttachFile(path); err != nil {
		log.Warn("cannot open the log file, logging to the console only", "path", path, "err", err)
	}
}
