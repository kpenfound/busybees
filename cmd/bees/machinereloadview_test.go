package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/daemon"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/tui"
	"github.com/kpenfound/busybees/internal/versions"
)

func viewSnapshot(t *testing.T, ch <-chan []tui.Project, accept func([]tui.Project) bool) []tui.Project {
	t.Helper()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		select {
		case ps := <-ch:
			if accept(ps) {
				return ps
			}
		case <-timer.C:
			t.Fatal("missing project snapshot")
			return nil
		}
	}
}

func TestMachineViewNamesIncludeDrainingProjects(t *testing.T) {
	for _, repos := range [][]string{{"acme/foo", "other/foo"}, {"other/foo", "acme/foo"}} {
		t.Run(repos[0], func(t *testing.T) {
			a, b := writeProject(t, repos[0], ""), writeProject(t, repos[1], "")
			m := loadMachine(t, a)
			d := machineDaemon(&globalFlags{}, m)
			v := newMachineView(context.Background(), d)
			defer v.close()
			d.Projects = v.wrap(m, d.Projects)
			initial := v.initial()[0]
			old := v.live[a]
			if initial.Name != "foo" {
				t.Fatalf("initial name: %q", initial.Name)
			}

			next := loadMachine(t, b)
			added := v.wrap(next, machineDaemon(&globalFlags{}, next).Projects)
			d.Projects[0].Observe(daemon.ProjectDraining)
			added[0].Observe(daemon.ProjectActive)
			d.Reconciled([]string{b})
			ps := <-v.updates
			if len(ps) != 2 || ps[0].Name != repos[1] || ps[1].Name != repos[0] || !ps[1].Draining {
				t.Fatalf("active and draining names: %+v", ps)
			}
			if v.live[a] != old || ps[1].Path != initial.Path || ps[1].Generation != initial.Generation || ps[1].Events != initial.Events || ps[1].Done != initial.Done {
				t.Fatal("renaming replaced the draining source")
			}
			old.fail(errors.New("old source start failure"))
			if err := ps[1].Kill("x"); err == nil || !strings.Contains(err.Error(), "old source start failure") {
				t.Fatalf("renamed control detached from old source: %v", err)
			}
			if err := ps[0].Kill("x"); err == nil || !strings.Contains(err.Error(), "has not started yet") {
				t.Fatalf("added control attached to old source: %v", err)
			}

			// Once the collision drains, the surviving source gets its short
			// label back without replacing its identity or event subscription.
			d.Projects[0].Observe(daemon.ProjectFinished)
			d.Reconciled([]string{b})
			finished := <-v.updates
			if len(finished) != 1 || finished[0].Name != "foo" {
				t.Fatalf("names after drain: %+v", finished)
			}
			if finished[0].Generation != ps[0].Generation || finished[0].Events != ps[0].Events || finished[0].Done != ps[0].Done {
				t.Fatal("shortening the name replaced the surviving source")
			}
			if ps[0].Name != repos[1] || ps[1].Name != repos[0] {
				t.Fatal("renaming mutated a previously published snapshot")
			}
		})
	}
}

// Exercise the accepted config -> daemon -> view path with delayed starts.
// No scheduler or external executable runs: starts drain under explicit control.
func TestMachineViewReloadReconcilesAndReaddsDuringDrain(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	a, b, c := writeProject(t, "acme/a", ""), writeProject(t, "acme/b", ""), writeProject(t, "acme/c", "")
	m := loadMachine(t, a, b)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reload := make(chan []daemon.Project)
	d := &daemon.Daemon{Reload: reload, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	v := newMachineView(ctx, d)
	defer v.close()
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	build := func(m *config.Machine) []daemon.Project {
		var ps []daemon.Project
		for _, cfg := range m.Configs {
			name := cfg.Path
			ps = append(ps, daemon.Project{Name: name, Start: func(ctx context.Context) (daemon.Loop, error) {
				if name == c {
					return nil, errors.New("new project start failed")
				}
				<-ctx.Done()
				if name == b {
					<-release
				}
				return nil, ctx.Err()
			}})
		}
		return v.wrap(m, ps)
	}
	d.Projects = build(m)
	initial := v.initial()
	done := make(chan error, 1)
	go func() { done <- d.Run(ctx) }()
	viewSnapshot(t, v.updates, func(ps []tui.Project) bool { return len(ps) == 2 })
	hup := make(chan os.Signal)
	reloadDone := make(chan struct{})
	r := &machineReloader{path: m.Path, build: build, changes: reload, log: d.Logger,
		apply: func(context.Context, *config.Machine) error { return nil }}
	go func() { defer close(reloadDone); r.serve(ctx, hup) }()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(m.Path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		hup <- syscall.SIGHUP
	}
	write("projects = [\"missing.toml\"]\n")
	select {
	case ps := <-v.updates:
		t.Fatalf("invalid reload updated view: %+v", ps)
	case <-time.After(50 * time.Millisecond):
	}
	write("projects = [\"" + filepath.ToSlash(a) + "\",\"" + filepath.ToSlash(c) + "\"]\n")
	ps := viewSnapshot(t, v.updates, func(ps []tui.Project) bool {
		if len(ps) != 3 || ps[1].Name != "c" {
			return false
		}
		_, err := ps[1].Status()
		return err != nil
	})
	if ps[0].Events != initial[0].Events || ps[0].Generation != initial[0].Generation || ps[2].Events != initial[1].Events || !ps[2].Draining {
		t.Fatalf("sources/drain lost: %+v", ps)
	}
	if _, err := ps[1].Mail(); err != nil {
		t.Fatal(err)
	}
	if err := ps[1].Kill("x"); err == nil || !strings.Contains(err.Error(), "new project start failed") {
		t.Fatalf("added failure: %v", err)
	}
	if err := ps[2].Send("developer", 1, 0, "s", "b"); err == nil || !strings.Contains(err.Error(), "messaging disabled") {
		t.Fatalf("draining send: %v", err)
	}
	write("projects = [\"" + filepath.ToSlash(c) + "\",\"" + filepath.ToSlash(b) + "\",\"" + filepath.ToSlash(a) + "\"]\n")
	ps = viewSnapshot(t, v.updates, func(ps []tui.Project) bool { return len(ps) == 3 && ps[0].Name == "c" })
	if ps[1].Generation != initial[1].Generation || !ps[1].Draining || ps[2].Events != initial[0].Events {
		t.Fatal("readd detached an old source before drain")
	}
	close(release)
	ps = viewSnapshot(t, v.updates, func(ps []tui.Project) bool { return len(ps) == 3 && ps[1].Name == "b" && !ps[1].Draining })
	if ps[1].Generation == initial[1].Generation || ps[1].Events == initial[1].Events {
		t.Fatal("readd reused old source")
	}
	select {
	case <-initial[1].Done:
	default:
		t.Fatal("retired source not stopped")
	}
	if err := ps[1].Kill("x"); err == nil || !strings.Contains(err.Error(), "has not started yet") {
		t.Fatalf("new incarnation stand-in: %v", err)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("daemon blocked on view")
	}
	select {
	case <-reloadDone:
	case <-time.After(5 * time.Second):
		t.Fatal("reload did not stop")
	}
}

func TestMachineViewAddedSchedulerControlsAndSlowConsumer(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	a, b := writeProject(t, "acme/a", ""), writeProject(t, "acme/b", "")
	m := loadMachine(t, a)
	d := machineDaemon(&globalFlags{}, m)
	v := newMachineView(context.Background(), d)
	defer v.close()
	d.Projects = v.wrap(m, d.Projects)
	next := loadMachine(t, a, b)
	ps := v.wrap(next, machineDaemon(&globalFlags{}, next).Projects)
	ps[1].Observe(daemon.ProjectActive)
	d.Reconciled([]string{a, b})
	snapshot := <-v.updates
	added := snapshot[1]
	if _, err := added.Status(); err != nil {
		t.Fatal(err)
	}
	if _, err := added.Mail(); err != nil {
		t.Fatal(err)
	}
	if err := added.Send("developer", 1, 0, "s", "b"); err == nil || !strings.Contains(err.Error(), "has not started yet") {
		t.Fatalf("pre-start Send: %v", err)
	}
	loop, err := ps[1].Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loop.(*projectLoop).Close() }()
	if err := added.Send("developer", 1, 0, "s", "b"); err != nil {
		t.Fatal(err)
	}
	counts, err := added.Mail()
	if err != nil || counts["developer"] != 1 {
		t.Fatalf("added mailbox: %v, %v", counts, err)
	}
	ps[1].Observe(daemon.ProjectDraining)
	if err := added.Kill("unknown"); err == nil || strings.Contains(err.Error(), "not started") || strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("draining kill did not reach scheduler: %v", err)
	}
	// Run a single failed poll through a fake gh. Its event must travel from
	// this added scheduler through the stand-in's existing event channel.
	pl := loop.(*projectLoop)
	pl.Once = true
	pl.app.gh.Exec = func(context.Context, ...string) ([]byte, error) { return nil, errors.New("fake poll failed") }
	if err := pl.Scheduler.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case ev := <-added.Events:
		if ev.Kind != scheduler.EventPoll || !strings.Contains(ev.Err, "fake poll failed") {
			t.Fatalf("added scheduler event: %+v", ev)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("added scheduler events are detached")
	}
	ps[1].Observe(daemon.ProjectFinished)
	if err := added.Kill("unknown"); err == nil || !strings.Contains(err.Error(), "no longer active") {
		t.Fatalf("retired kill: %v", err)
	}
	// Fill and overwrite the channel repeatedly, then repeat after UI exit.
	done := make(chan struct{})
	go func() {
		for range 1000 {
			d.Reconciled([]string{a})
		}
		v.close()
		for range 1000 {
			d.Reconciled([]string{a})
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("lifecycle waited for the UI")
	}
}
