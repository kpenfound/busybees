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
	"io"
	"log/slog"
	"runtime/debug"
	"slices"
	"sync"
)

// Loop is one project's poll loop: what *scheduler.Scheduler is.
//
// A Loop that holds something from its start until it runs (a log file) also
// implements io.Closer: the daemon closes a loop it discards unrun, and a loop
// that runs releases it itself.
type Loop interface {
	// Run polls until ctx is cancelled and the work in flight has finished.
	Run(ctx context.Context) error
	// HardStop stops the loop's running sessions now.
	HardStop()
	// SetPaused pauses (true) or resumes (false) the loop's dispatch.
	SetPaused(paused bool)
}

// Project is one project the daemon runs.
type Project struct {
	// Name identifies the project in log lines and errors: the path of its
	// bees.toml.
	Name string
	// Start builds the project's loop. It runs on the project's own
	// goroutine, so a slow or failing start holds up no other project.
	Start func(ctx context.Context) (Loop, error)
	// Observe receives this loop's lifecycle on the daemon goroutine. It must
	// not wait for a consumer. A removed incarnation finishes before a new
	// incarnation of the same Name starts.
	Observe func(ProjectState)
}

// ProjectState describes one loop's lifecycle, independently of its path.
type ProjectState int

const (
	ProjectActive ProjectState = iota
	ProjectDraining
	ProjectFinished
)

func (p Project) notify(s ProjectState) {
	if p.Observe != nil {
		p.Observe(s)
	}
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
	// Reload supplies replacement project lists. While Reload is open, Run
	// stays alive until cancellation, even when every project has stopped.
	// Once Reload is closed, Run returns after the active projects finish.
	Reload <-chan []Project
	// Reconciled receives the desired order after each reconciliation or loop
	// completion, once all corresponding lifecycle notifications have run.
	// It must not wait for a consumer.
	Reconciled func([]string)
	// Logger gets one record per project that fails. nil is slog.Default().
	Logger *slog.Logger

	mu      sync.Mutex
	loops   []Loop
	stopped bool
	paused  bool
}

// Run starts every project on its own goroutine. Without Reload it returns
// once all projects have returned; while Reload is open it waits for
// cancellation, reconciling each new list by Name. Closing Reload lets Run
// return after the active projects finish. Cancelling ctx asks every loop for
// its cool-down, as it does for a single-project run. A project that fails
// does not cancel the others.
// The error joins a *ProjectError for every project that failed, and is nil
// when none did.
func (d *Daemon) Run(ctx context.Context) error {
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	type running struct {
		project Project
		cancel  context.CancelFunc
		active  bool
		removed bool
	}
	type result struct {
		name string
		err  error
	}
	entries := map[string]*running{}
	desired := map[string]Project{}
	finished := make(chan result)
	active := 0
	var errs []error
	start := func(p Project) {
		projectCtx, cancel := context.WithCancel(ctx)
		entries[p.Name] = &running{project: p, cancel: cancel, active: true}
		active++
		p.notify(ProjectActive)
		go func() { finished <- result{p.Name, d.runProject(projectCtx, p)} }()
	}
	var order []string
	changed := func() {
		if d.Reconciled != nil {
			d.Reconciled(slices.Clone(order))
		}
	}
	reconcile := func(projects []Project) {
		order = nil
		for _, p := range projects {
			order = append(order, p.Name)
		}
		desired = map[string]Project{}
		for _, p := range projects {
			desired[p.Name] = p
		}
		for name, entry := range entries {
			if _, keep := desired[name]; !keep {
				if !entry.removed {
					entry.project.notify(ProjectDraining)
				}
				entry.removed = true
				entry.cancel()
				if !entry.active {
					entry.project.notify(ProjectFinished)
					delete(entries, name)
				}
			}
		}
		for _, p := range projects {
			if _, exists := entries[p.Name]; !exists {
				start(p)
			}
		}
		changed()
	}
	reconcile(d.Projects)
	reload := d.Reload
	stopping := ctx.Done()
	for active > 0 || reload != nil {
		select {
		case <-stopping:
			stopping = nil
			reload = nil
			desired = nil
		case projects, ok := <-reload:
			if !ok {
				reload = nil
				continue
			}
			if ctx.Err() == nil {
				reconcile(projects)
			}
		case r := <-finished:
			active--
			entry := entries[r.name]
			entry.cancel()
			entry.active = false
			if r.err != nil {
				log.Error("project stopped", "project", r.name, "err", r.err)
				errs = append(errs, &ProjectError{Project: r.name, Err: r.err})
			}
			if entry.removed {
				entry.project.notify(ProjectFinished)
				delete(entries, r.name)
				// A project re-added during its cool-down starts only after the old
				// scheduler has released its state and sessions.
				if p, ok := desired[r.name]; ok && ctx.Err() == nil {
					start(p)
				}
			}
			changed()
		}
	}

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
	if ctx.Err() != nil || !d.add(loop) {
		// Never run, so nothing else releases what Start acquired.
		if c, ok := loop.(io.Closer); ok {
			return c.Close()
		}
		return nil
	}
	defer d.remove(loop)
	return loop.Run(ctx)
}

// add records a started loop for HardStop and SetPaused, and reports false
// when HardStop already ran: a loop that had not started by then is not
// started at all. A loop added while the daemon is paused starts paused.
func (d *Daemon) add(loop Loop) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped {
		return false
	}
	if d.paused {
		loop.SetPaused(true)
	}
	d.loops = append(d.loops, loop)
	return true
}

// remove forgets a finished loop so later hard stops only reach live loops.
func (d *Daemon) remove(loop Loop) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.loops = slices.DeleteFunc(d.loops, func(l Loop) bool { return l == loop })
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

// SetPaused pauses (true) or resumes (false) the dispatch of every project
// that has started, and of every project a reload starts while it is paused.
func (d *Daemon) SetPaused(paused bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.paused = paused
	for _, l := range d.loops {
		l.SetPaused(paused)
	}
}
