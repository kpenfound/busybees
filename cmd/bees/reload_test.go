package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/daemon"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/tui"
	"github.com/kpenfound/busybees/internal/versions"
)

// The live view's r key on a single project reads bees.toml again and hands
// it to the scheduler: a file that does not load, or one that changes a key
// the running factory cannot, is refused with the reason and nothing
// changes; one that changes a live key is accepted.
func TestProjectReloaderReadsTheFileAgainAndReportsRefusals(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	path := writeProject(t, "acme/a", "")
	m := loadMachine(t, path)
	var console bytes.Buffer
	g := &globalFlags{logger: logging.New(logging.Options{Console: &console})}
	t.Cleanup(func() { _ = g.logger.Close() })
	loop, err := startProject(context.Background(), g, m.Configs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loop.Close() }()
	reload := projectReloader(context.Background(), loop.app, loop.Scheduler)
	base := "version = 2\n[project]\nrepo = \"acme/a\"\ndefault_branch = \"main\"\n"
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	write(base + "[scheduler]\nno_such_key = 1\n")
	if err := reload(); err == nil || !strings.Contains(err.Error(), "no_such_key") {
		t.Fatalf("an invalid file: %v, want the load error", err)
	}
	write(base + "[filter]\nlabel = \"other\"\n")
	if err := reload(); err == nil || !strings.Contains(err.Error(), "filter.label") {
		t.Fatalf("a fixed key changed: %v, want a refusal naming filter.label", err)
	}
	// A file that loads and cannot be resolved is refused naming it too:
	// filter.assignee = "@me" with no gh user to resolve it to.
	realMe := meLookup
	t.Cleanup(func() { meLookup = realMe })
	meLookup = func(context.Context) (string, error) { return "", errors.New("gh is signed out") }
	write(base + "[filter]\nassignee = \"@me\"\n")
	if err := reload(); err == nil || !strings.Contains(err.Error(), "gh is signed out") || !strings.Contains(err.Error(), path) {
		t.Fatalf("an unresolvable file: %v, want the reason and the file", err)
	}
	if strings.Contains(console.String(), "reload accepted") {
		t.Fatal("a refused reload reached the scheduler")
	}
	if !strings.Contains(console.String(), "no_such_key") || !strings.Contains(console.String(), "filter.label") {
		t.Errorf("the refusals were not logged: %q", console.String())
	}

	write(base + "[scheduler]\npoll_interval = \"9m\"\n")
	if err := reload(); err != nil {
		t.Fatalf("a live key changed: %v", err)
	}
	if !strings.Contains(console.String(), "reload accepted") {
		t.Errorf("the accepted reload did not reach the scheduler: %q", console.String())
	}
}

// A machine reload checks every running project's file before it applies
// any: with one project's bees.toml changing a live key and another's a
// fixed one, the reload is refused and the first project's scheduler is
// handed nothing. A listed project that is not running is skipped, whatever
// its file says, and a file that cannot be resolved refuses the reload the
// way a fixed key does.
func TestMachineReloadIsAllOrNothingAcrossProjects(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	a, b, c := writeProject(t, "acme/a", ""), writeProject(t, "acme/b", ""), writeProject(t, "acme/c", "")
	m := loadMachine(t, a, b, c)
	var console bytes.Buffer
	g := &globalFlags{logger: logging.New(logging.Options{Console: &console})}
	t.Cleanup(func() { _ = g.logger.Close() })
	rt := newMachineRuntime(g, m)
	// a and b run; c is listed and never started.
	for _, p := range rt.projects(m)[:2] {
		l, err := p.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = l.(*projectLoop).Close() }()
	}
	changes := make(chan []daemon.Project, 1)
	r := &machineReloader{path: m.Path, build: rt.projects, apply: rt.reloadProjects, changes: changes, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	toml := func(repo, extra string) string {
		return "version = 2\n[project]\nrepo = \"" + repo + "\"\ndefault_branch = \"main\"\n" + extra
	}
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	accepted := func() int { return strings.Count(console.String(), "reload accepted") }
	ctx := context.Background()

	write(a, toml("acme/a", "[scheduler]\npoll_interval = \"9m\"\n"))
	write(b, toml("acme/b", "branch_prefix = \"other/\"\n"))
	err := r.reload(ctx)
	if err == nil || !strings.Contains(err.Error(), "project.branch_prefix") || !strings.Contains(err.Error(), b) {
		t.Fatalf("one project's fixed key: %v, want a refusal naming the key and %s", err, b)
	}
	if strings.Contains(err.Error(), a) {
		t.Errorf("the refusal blames the project whose file was fine: %v", err)
	}
	if n := accepted(); n != 0 {
		t.Fatalf("a refused machine reload handed %d schedulers their file: the valid project's was applied", n)
	}
	select {
	case <-changes:
		t.Fatal("a refused machine reload changed the project list")
	default:
	}

	// c is not running: its file changing a fixed key refuses nothing, and
	// only the two running schedulers are handed theirs.
	write(b, toml("acme/b", "[scheduler]\npoll_interval = \"9m\"\n"))
	write(c, toml("acme/c", "state_dir = \"elsewhere\"\n"))
	if err := r.reload(ctx); err != nil {
		t.Fatalf("a project that is not running refused the reload: %v", err)
	}
	if n := accepted(); n != 2 {
		t.Fatalf("%d schedulers were handed their file, want the 2 that are running", n)
	}
	if ps := <-changes; len(ps) != 3 {
		t.Fatalf("projects: %+v", ps)
	}

	// A file that cannot be resolved refuses the whole reload as well.
	realMe := meLookup
	t.Cleanup(func() { meLookup = realMe })
	meLookup = func(context.Context) (string, error) { return "", errors.New("gh is signed out") }
	write(a, toml("acme/a", "[filter]\nassignee = \"@me\"\n"))
	write(b, toml("acme/b", "[scheduler]\npoll_interval = \"3m\"\n"))
	err = r.reload(ctx)
	if err == nil || !strings.Contains(err.Error(), "gh is signed out") || !strings.Contains(err.Error(), a) {
		t.Fatalf("an unresolvable file: %v, want the reason and %s", err, a)
	}
	if n := accepted(); n != 2 {
		t.Fatalf("a reload refused over one file still reached a scheduler (%d accepted)", n)
	}
}

// The single-project view is given the reload behind its r key, and a
// --once run, which has no next pass to take a reload, is given none.
func TestTheProjectViewIsWiredToTheReload(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	path := writeProject(t, "acme/a", "")
	var console bytes.Buffer
	g := &globalFlags{logger: logging.New(logging.Options{Console: &console})}
	t.Cleanup(func() { _ = g.logger.Close() })
	loop, err := startProject(context.Background(), g, loadMachine(t, path).Configs[0], nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = loop.Close() }()
	real := runView
	t.Cleanup(func() { runView = real })
	var got tui.Deps
	runView = func(_ context.Context, d tui.Deps, _ tui.Factory, _ func()) error { got = d; return nil }

	if err := runWithTUI(context.Background(), loop.app, loop.Scheduler, g, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got.Reload == nil {
		t.Fatal("the view was given no reload")
	}
	if err := got.Reload(); err != nil || !strings.Contains(console.String(), "reload accepted") {
		t.Fatalf("the view's reload did not reach the scheduler: %v, %q", err, console.String())
	}
	loop.Once = true
	if err := runWithTUI(context.Background(), loop.app, loop.Scheduler, g, io.Discard); err != nil {
		t.Fatal(err)
	}
	if got.Reload != nil {
		t.Error("a --once run's view was given a reload")
	}
}

// A machine run's view is given the machine reload SIGHUP runs, which
// reaches the daemon, and a --once machine run is given none. The project
// is one that cannot start, so no scheduler polls anything.
func TestTheMachineViewIsWiredToTheReload(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	broken := writeProject(t, "acme/broken", "dir = \""+filepath.ToSlash(t.TempDir())+"\"\n")
	m := loadMachine(t, broken)
	g := &globalFlags{logger: logging.New(logging.Options{Console: io.Discard})}
	t.Cleanup(func() { _ = g.logger.Close() })
	realTerminal, realView := isTerminal, runMachineView
	t.Cleanup(func() { isTerminal, runMachineView = realTerminal, realView })
	isTerminal = func(*os.File) bool { return true }

	for _, once := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		var got tui.Deps
		var reloadErr error
		runMachineView = func(ctx context.Context, d tui.Deps, machine tui.Machine, down func()) error {
			got = d
			if d.Reload == nil {
				return nil
			}
			done := make(chan error, 1)
			go func() { done <- machine.Run(ctx) }()
			reloadErr = d.Reload()
			cancel()
			select {
			case err := <-done:
				return err
			case <-time.After(10 * time.Second):
				return errors.New("the daemon did not stop")
			}
		}
		cmd := &cobra.Command{}
		cmd.SetContext(ctx)
		cmd.SetErr(io.Discard)
		err := runMachine(cmd, g, m, runOptions{once: once, skipDoctor: true}, false)
		cancel()
		if once {
			if err != nil || got.Reload != nil {
				t.Errorf("--once: err %v, reload given: %v", err, got.Reload != nil)
			}
			continue
		}
		var pe *daemon.ProjectError
		if !errors.As(err, &pe) {
			t.Errorf("runMachine: %v, want the broken project's error", err)
		}
		if got.Reload == nil {
			t.Fatal("the machine view was given no reload")
		}
		if reloadErr != nil {
			t.Errorf("the view's reload did not reach the daemon: %v", reloadErr)
		}
	}
}
