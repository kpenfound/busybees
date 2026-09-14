package main

import (
	"context"
	"fmt"
	"io"
	"path"
	"path/filepath"
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
// The view is over every project the daemon runs, with a selector to cycle
// through them; its stop keys stop the daemon, and every project with it.
func runMachineWithTUI(ctx context.Context, g *globalFlags, m *config.Machine, console io.Writer) error {
	d := machineDaemon(g, m)
	projects, stop := machineView(ctx, d, m)
	defer stop()
	// No project's [logging] table applies to the shared console (see
	// startProject), so the flags alone say what it prints once it is back.
	restore := quietConsole(g.logger, g.console, config.Logging{}, console)
	var once sync.Once
	give := func() { once.Do(restore) }
	defer give()
	return runMachineView(ctx, tui.Deps{Projects: projects, Now: time.Now, Open: openInBrowser}, d, give)
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
	names := projectNames(ctx, m.Configs)
	stop := make(chan struct{})
	projects := make([]tui.Project, len(d.Projects))
	for i := range d.Projects {
		cfg := m.Configs[i]
		pv := &projectView{name: names[i], events: make(chan scheduler.Event, viewEventBuffer), stop: stop}
		start := d.Projects[i].Start
		d.Projects[i].Start = func(ctx context.Context) (daemon.Loop, error) {
			loop, err := start(ctx)
			if err != nil {
				pv.fail(err)
				return nil, err
			}
			pv.attach(loop.(*projectLoop))
			return loop, nil
		}
		store := state.New(cfg.StateDir())
		projects[i] = tui.Project{
			Name:   names[i],
			Repo:   cfg.Project.Repo,
			Events: pv.events,
			Status: pv.status(store),
			Mail:   mail.Open(store.MailDir()).Counts,
			Kill:   pv.kill(ctx),
			Send:   pv.send,
		}
	}
	return projects, sync.OnceFunc(func() { close(stop) })
}

// projectNames is what the live view calls the daemon's projects: the name
// half of each one's repository (acme/foo is foo), or the whole owner/name
// when two projects share one. A project's repository is resolved here,
// from its remote when its bees.toml does not set it, so the view can name
// the project before its scheduler has started; one that cannot be resolved
// is named by the directory its bees.toml is in, and its start reports the
// error.
func projectNames(ctx context.Context, configs []*config.Config) []string {
	full := make([]string, len(configs))
	for i, cfg := range configs {
		if err := cfg.Resolve(ctx); err != nil {
			full[i] = filepath.ToSlash(filepath.Dir(cfg.Path))
			continue
		}
		full[i] = cfg.Project.Repo
	}
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
	name   string
	events chan scheduler.Event
	stop   <-chan struct{}

	mu   sync.Mutex
	loop *projectLoop
	err  error // why the project did not start
}

// attach records the started project and starts forwarding its scheduler's
// events to the view.
func (pv *projectView) attach(loop *projectLoop) {
	pv.mu.Lock()
	pv.loop = loop
	pv.mu.Unlock()
	// --verbose streams every session event to stderr, which would scribble
	// over the view exactly as the console log would (see runWithTUI).
	loop.app.runner.Stream = nil
	go forward(loop.Subscribe(), pv.events, pv.stop)
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
		case ev := <-from:
			select {
			case to <- ev:
			default:
			}
		}
	}
}
