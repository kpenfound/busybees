package main

import (
	"context"
	"io"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/tui"
)

// runWithTUI runs the factory with the terminal UI drawn over it. The view
// owns the terminal for as long as the factory runs, so the console logging
// is silenced while it is up and given back before anything else prints —
// including the drain, which a person who pressed Ctrl-C twice waits out
// with the console back. <state_dir>/bees.log keeps every record throughout,
// so nothing the view covers up is lost.
func runWithTUI(ctx context.Context, a *app, s *scheduler.Scheduler, g *globalFlags, console io.Writer) error {
	restore := quietConsole(a.logger, g.console, a.cfg.Logging, console)
	defer restore()
	// --verbose streams claude's own events to stderr, which would scribble
	// over the view exactly as the console log would. The log file and the
	// per-session transcripts still have everything.
	a.runner.Stream = nil
	return tui.Run(ctx, tui.Deps{
		Status: a.store.LoadStatus,
		Mail:   a.mail.Counts,
		Now:    time.Now,
		Repo:   a.cfg.Project.Repo,
	}, s)
}

// quietConsole sends console logging to io.Discard and returns the function
// that gives it back. Only the console destination changes: the log file
// handler is untouched and still gets every record at debug level.
func quietConsole(l *logging.Logger, f consoleFlags, cfg config.Logging, console io.Writer) func() {
	if l == nil {
		return func() {}
	}
	o := mergeLogging(f, cfg)
	quiet := o
	quiet.Console = io.Discard
	l.SetConsole(quiet)
	return func() {
		o.Console = console
		l.SetConsole(o)
	}
}
