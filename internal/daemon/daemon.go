// Package daemon runs the schedulers of several projects in one process, each
// on its own goroutine. It knows nothing of how a project's scheduler is
// built: a Project's Start does that, and each project keeps its own state
// directory, mailbox, notes and poll loop exactly as a single-project run
// does. One project failing to start, returning an error or panicking is
// logged and reported for that project while the others run on.
package daemon

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
)

// Loop is one project's poll loop: what *scheduler.Scheduler is.
type Loop interface {
	// Run polls until ctx is cancelled and the work in flight has finished.
	Run(ctx context.Context) error
	// HardStop stops the loop's running sessions now.
	HardStop()
}

// Project is one project the daemon runs.
type Project struct {
	// Name identifies the project in log lines and errors: the path of its
	// bees.toml.
	Name string
	// Start builds the project's loop. It runs on the project's own
	// goroutine, so a slow or failing start holds up no other project.
	Start func(ctx context.Context) (Loop, error)
}

// ProjectError is what one project's start or loop failed with.
type ProjectError struct {
	Project string
	Err     error
}

func (e *ProjectError) Error() string { return e.Project + ": " + e.Err.Error() }

func (e *ProjectError) Unwrap() error { return e.Err }

// Daemon runs Projects concurrently.
type Daemon struct {
	Projects []Project
	// Logger gets one record per project that fails. nil is slog.Default().
	Logger *slog.Logger

	mu      sync.Mutex
	loops   []Loop
	stopped bool
}

// Run starts every project on its own goroutine and returns once all of them
// have returned: cancelling ctx asks each loop for its cool-down, as it does
// for a single-project run. A project that fails does not cancel the others.
// The error joins a *ProjectError for every project that failed, and is nil
// when none did.
func (d *Daemon) Run(ctx context.Context) error {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	errs := make([]error, len(d.Projects))
	var wg sync.WaitGroup
	for i, p := range d.Projects {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.runProject(ctx, p); err != nil {
				log.Error("project stopped", "project", p.Name, "err", err)
				errs[i] = &ProjectError{Project: p.Name, Err: err}
			}
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}

// runProject starts and runs one project, turning a panic in either into an
// error. A panic on a goroutine the loop itself started is out of its reach.
func (d *Daemon) runProject(ctx context.Context, p Project) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	loop, err := p.Start(ctx)
	if err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if !d.add(loop) {
		return nil
	}
	return loop.Run(ctx)
}

// add records a started loop for HardStop, and reports false when HardStop
// already ran: a loop that had not started by then is not started at all.
func (d *Daemon) add(loop Loop) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	d.loops = append(d.loops, loop)
	return true
}

// HardStop stops the running sessions of every project that has started, and
// keeps a project still starting from running.
func (d *Daemon) HardStop() {
	d.mu.Lock()
	d.stopped = true
	loops := append([]Loop(nil), d.loops...)
	d.mu.Unlock()
	for _, l := range loops {
		l.HardStop()
	}
}
