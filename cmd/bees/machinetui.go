package main

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/daemon"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/tui"
)

// runMachineWithTUI runs the daemon of a machine config with the live view
// drawn over it, the way runWithTUI does a single project's scheduler: the
// console is silenced while the view is up and given back the moment it
// comes down, and each project's <state_dir>/bees.log keeps every record.
// The view starts with every listed project and a selector to cycle through
// them and follows accepted reloads through each old loop's drain. Its stop
// keys stop the daemon, and every project with it.
func runMachineWithTUI(ctx context.Context, g *globalFlags, m *config.Machine, console io.Writer) error {
	return runPreparedMachineWithTUI(ctx, g, m, console, machineDaemon(g, m))
}

func runPreparedMachineWithTUI(ctx context.Context, g *globalFlags, m *config.Machine, console io.Writer, d *daemon.Daemon) error {
	view := newMachineView(ctx, d)
	d.Projects = view.wrap(m, d.Projects)
	return runPreparedMachineView(ctx, g, console, d, view)
}

func runPreparedMachineView(ctx context.Context, g *globalFlags, console io.Writer, d *daemon.Daemon, view *machineViews) error {
	defer view.close()
	// No project's [logging] table applies to the shared console (see
	// startProject), so the flags alone say what it prints once it is back.
	restore := quietConsole(g.logger, g.console, config.Logging{}, console)
	var once sync.Once
	give := func() { once.Do(restore) }
	defer give()
	return runMachineView(ctx, tui.Deps{Projects: view.initial(), ProjectUpdates: view.updates, Now: time.Now, Open: openInBrowser}, d, give)
}

// runMachineView draws the view over the daemon: a variable so a test can
// stand in for the screen, which needs a terminal to open, and check what
// runMachineWithTUI wired up around it.
var runMachineView = tui.RunMachine

// machineView is what the live view is given for the daemon's projects: one
// tui.Project each, wired before any project's scheduler exists. The daemon
// builds a project's scheduler on the project's own goroutine once it runs
// (daemon.Project.Start), and the view is up by then, so each entry stands
// in for a scheduler that is not there yet: an event channel the
// scheduler's stream is forwarded into once it has started; status.json and
// the mailbox read from the project's state directory, which need no
// scheduler (an unwritten status.json reads as empty, and a start that
// failed is what the status read reports); and Kill and Send that reach the
// scheduler once it exists and refuse until then. The function returned
// ends the forwarding, for when the view is down.
func machineView(ctx context.Context, d *daemon.Daemon, m *config.Machine) ([]tui.Project, func()) {
	view := newMachineView(ctx, d)
	d.Projects = view.wrap(m, d.Projects)
	return view.initial(), view.close
}

// machineViews publishes coalesced full snapshots. Only the daemon's lifecycle
// callbacks change membership; preparing an unused reload never replaces a source.
type machineViews struct {
	ctx     context.Context
	mu      sync.Mutex
	next    uint64
	order   []string
	live    map[string]*projectView
	startup []tui.Project
	updates chan []tui.Project
	stopped bool
}

func newMachineView(ctx context.Context, d *daemon.Daemon) *machineViews {
	v := &machineViews{ctx: ctx, live: map[string]*projectView{}, updates: make(chan []tui.Project, 1)}
	d.Reconciled = func(names []string) {
		v.mu.Lock()
		defer v.mu.Unlock()
		v.order = names
		v.publish()
	}
	return v
}

func (v *machineViews) initial() []tui.Project { return v.startup }

func (v *machineViews) wrap(m *config.Machine, projects []daemon.Project) []daemon.Project {
	full := projectFullNames(v.ctx, m.Configs)
	names := projectNames(full)
	v.mu.Lock()
	defer v.mu.Unlock()
	for i := range projects {
		cfg := m.Configs[i]
		v.next++
		pv := &projectView{name: names[i], fullName: full[i], events: make(chan scheduler.Event, viewEventBuffer), stop: make(chan struct{})}
		store := state.New(cfg.StateDir())
		pv.source = tui.Project{
			Path: projects[i].Name, Generation: v.next, Name: names[i], Repo: cfg.Project.Repo,
			Events: pv.events, Done: pv.stop, Status: pv.status(store), Mail: mail.Open(store.MailDir()).Counts,
			Kill: pv.kill(v.ctx), Send: pv.send,
		}
		start := projects[i].Start
		projects[i].Start = func(ctx context.Context) (daemon.Loop, error) {
			loop, err := start(ctx)
			if err != nil {
				pv.fail(err)
				return nil, err
			}
			pv.attach(loop.(*projectLoop))
			return loop, nil
		}
		projects[i].Observe = func(state daemon.ProjectState) {
			v.mu.Lock()
			defer v.mu.Unlock()
			if v.stopped {
				pv.retire()
				return
			}
			switch state {
			case daemon.ProjectActive:
				v.live[pv.source.Path] = pv
			case daemon.ProjectDraining:
				pv.source.Draining = true
				pv.mu.Lock()
				pv.draining = true
				pv.mu.Unlock()
			case daemon.ProjectFinished:
				delete(v.live, pv.source.Path)
				pv.retire()
			}
		}
		// Startup sources are readable before Daemon.Run starts any loop.
		if len(v.order) == 0 {
			v.startup = append(v.startup, pv.source)
			v.live[pv.source.Path] = pv
		}
	}
	if len(v.order) == 0 {
		for _, p := range v.startup {
			v.order = append(v.order, p.Path)
		}
	}
	return projects
}

// publish replaces a buffered snapshot while holding mu. The only other
// channel operation is the UI receiving, so the send always has room.
func (v *machineViews) publish() {
	if v.stopped {
		return
	}
	out := make([]tui.Project, 0, len(v.live))
	seen := map[string]bool{}
	for _, name := range v.order {
		if p := v.live[name]; p != nil {
			out = append(out, p.source)
			seen[name] = true
		}
	}
	// Draining removals follow the desired entries in their original order.
	var draining []tui.Project
	for name, p := range v.live {
		if !seen[name] {
			draining = append(draining, p.source)
		}
	}
	slices.SortFunc(draining, func(a, b tui.Project) int { return cmp.Compare(a.Generation, b.Generation) })
	out = append(out, draining...)
	// Labels describe the complete live membership, including collisions
	// with draining removals. Rename only snapshot values, retaining every
	// source's identity, event subscription and callbacks.
	full := make([]string, len(out))
	for i, p := range out {
		full[i] = v.live[p.Path].fullName
	}
	for i, name := range projectNames(full) {
		out[i].Name = name
	}
	select {
	case <-v.updates:
	default:
	}
	v.updates <- out
}

func (v *machineViews) close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stopped {
		return
	}
	v.stopped = true
	close(v.updates)
	for _, p := range v.live {
		p.retire()
	}
}

// projectFullNames resolves the repositories used to name view sources,
// from its remote when its bees.toml does not set it, so the view can name
// the project before its scheduler has started; one that cannot be resolved
// is named by the directory its bees.toml is in, and its start reports the
// error.
func projectFullNames(ctx context.Context, configs []*config.Config) []string {
	full := make([]string, len(configs))
	for i, cfg := range configs {
		if err := cfg.Resolve(ctx); err != nil {
			full[i] = filepath.ToSlash(filepath.Dir(cfg.Path))
			continue
		}
		full[i] = cfg.Project.Repo
	}
	return full
}

// projectNames shortens repository names (acme/foo is foo), keeping the
// whole owner/name when live projects, including draining ones, share a name.
func projectNames(full []string) []string {
	names := make([]string, len(full))
	shared := map[string]int{}
	for i, f := range full {
		names[i] = path.Base(f)
		shared[names[i]]++
	}
	for i := range names {
		if shared[names[i]] > 1 {
			names[i] = full[i]
		}
	}
	return names
}

// viewEventBuffer is how far behind the view may fall on one project's
// stream before its events are dropped: the scheduler's own subscriber
// buffer, so forwarding loses nothing the view would have kept.
const viewEventBuffer = 64

// projectView is one project's stand-in in the live view until its
// scheduler exists (see machineView), and its way to the scheduler after.
type projectView struct {
	name     string
	fullName string
	events   chan scheduler.Event
	stop     chan struct{}
	source   tui.Project
	once     sync.Once
	draining bool
	retired  bool

	mu   sync.Mutex
	loop *projectLoop
	err  error // why the project did not start
}

// attach records the started project and starts forwarding its scheduler's
// events to the view.
func (pv *projectView) attach(loop *projectLoop) {
	pv.mu.Lock()
	if pv.retired {
		pv.mu.Unlock()
		return
	}
	pv.loop = loop
	pv.mu.Unlock()
	// --verbose streams every session event to stderr, which would scribble
	// over the view exactly as the console log would (see runWithTUI).
	loop.app.runner.Stream = nil
	go forward(loop.Subscribe(), pv.events, pv.stop)
}

func (pv *projectView) retire() {
	pv.mu.Lock()
	pv.retired = true
	pv.mu.Unlock()
	pv.once.Do(func() { close(pv.stop) })
}

func (pv *projectView) fail(err error) {
	pv.mu.Lock()
	pv.err = err
	pv.mu.Unlock()
}

// started is the project's scheduler, or why there is none: not yet, or a
// start that failed.
func (pv *projectView) started() (*projectLoop, error) {
	pv.mu.Lock()
	defer pv.mu.Unlock()
	switch {
	case pv.retired:
		return nil, fmt.Errorf("%s is no longer active", pv.name)
	case pv.loop != nil:
		return pv.loop, nil
	case pv.err != nil:
		return nil, fmt.Errorf("%s did not start: %w", pv.name, pv.err)
	default:
		return nil, fmt.Errorf("%s has not started yet", pv.name)
	}
}

// status reads the project's status.json, or says why the project is not
// running when its start failed.
func (pv *projectView) status(store *state.Store) func() (state.Status, error) {
	return func() (state.Status, error) {
		pv.mu.Lock()
		err := pv.err
		pv.mu.Unlock()
		if err != nil {
			return state.Status{}, fmt.Errorf("%s did not start: %w", pv.name, err)
		}
		return store.LoadStatus()
	}
}

// kill stops one of the project's sessions, as runWithTUI's Kill does: not
// under the view's context, because stopping a session and handing its
// issue over must finish even when the daemon is draining.
func (pv *projectView) kill(ctx context.Context) func(session string) error {
	return func(session string) error {
		loop, err := pv.started()
		if err != nil {
			return err
		}
		return loop.KillSession(context.WithoutCancel(ctx), session)
	}
}

// send queues a message typed in the session view in the project's mailbox
// (see sendFromView).
func (pv *projectView) send(to string, issue, pr int, subject, body string) error {
	pv.mu.Lock()
	disabled := pv.draining || pv.retired
	pv.mu.Unlock()
	if disabled {
		return fmt.Errorf("%s is no longer active; messaging disabled", pv.name)
	}
	loop, err := pv.started()
	if err != nil {
		return err
	}
	return sendFromView(loop.app)(to, issue, pr, subject, body)
}

// forward copies a scheduler's event stream into the view's channel until
// stop is closed, dropping an event the view has no room for, as the
// scheduler's own publish does.
func forward(from <-chan scheduler.Event, to chan<- scheduler.Event, stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		case ev, ok := <-from:
			if !ok {
				return
			}
			select {
			case to <- ev:
			default:
			}
		}
	}
}
