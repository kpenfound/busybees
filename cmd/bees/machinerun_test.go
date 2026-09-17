package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/daemon"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/versions"
)

// A reload of the machine config — SIGHUP, or the live view's r key — is
// all or nothing: a machine config that does not load, or a running
// project's bees.toml that changes a key its scheduler cannot, changes
// nothing and says why; an accepted one hands every running project its
// bees.toml read again and the daemon the new project list, on the
// original shared pool.
func TestMachineReloadRejectsInvalidConfigAndPreservesPool(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	a, b := writeProject(t, "acme/a", ""), writeProject(t, "acme/b", "")
	m := loadMachineWith(t, "max_developers = 3\n", a)
	var console bytes.Buffer
	g := &globalFlags{logger: logging.New(logging.Options{Console: &console})}
	t.Cleanup(func() { _ = g.logger.Close() })
	rt := newMachineRuntime(g, m)
	original, err := rt.projects(m)[0].Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = original.(*projectLoop).Close() }()
	if rt.running(a) != original {
		t.Fatal("the started project is not recorded for reloads")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hup := make(chan os.Signal)
	changes := make(chan []daemon.Project, 1)
	done := make(chan struct{})
	r := &machineReloader{path: m.Path, build: rt.projects, apply: rt.reloadProjects, changes: changes, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	go func() {
		defer close(done)
		r.serve(ctx, hup)
	}()
	write := func(path, body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	unchanged := func(what string) {
		t.Helper()
		select {
		case <-changes:
			t.Fatal(what + " changed the projects")
		case <-time.After(50 * time.Millisecond):
		}
	}
	write(m.Path, "projects = [\"missing.toml\"]\n")
	hup <- syscall.SIGHUP
	unchanged("an invalid machine config")

	// A running project's file that changes a fixed key refuses the whole
	// reload, naming the file and the key, and no membership changes.
	aToml := "version = 2\n[project]\nrepo = \"acme/a\"\ndefault_branch = \"main\"\n"
	write(a, aToml+"state_dir = \"elsewhere\"\n")
	write(m.Path, "projects = [\""+filepath.ToSlash(a)+"\",\""+filepath.ToSlash(b)+"\"]\nmax_developers = 9\n")
	if err := r.reload(ctx); err == nil || !strings.Contains(err.Error(), a) || !strings.Contains(err.Error(), "project.state_dir") {
		t.Fatalf("a fixed key changed in a running project's bees.toml: %v, want the file and the key", err)
	}
	unchanged("a refused project reload")
	if strings.Contains(console.String(), "reload accepted") {
		t.Fatal("a refused reload reached the scheduler")
	}
	// The same file that does not load refuses the reload the same way.
	write(a, aToml+"[scheduler]\nno_such_key = 1\n")
	if err := r.reload(ctx); err == nil || !strings.Contains(err.Error(), "no_such_key") {
		t.Fatalf("an invalid project file: %v", err)
	}
	unchanged("an invalid project file")

	// A live key changed reaches the running scheduler, and the project
	// list reaches the daemon.
	write(a, aToml+"[scheduler]\npoll_interval = \"9m\"\n")
	hup <- syscall.SIGHUP
	var projects []daemon.Project
	select {
	case projects = <-changes:
	case <-time.After(5 * time.Second):
		t.Fatal("valid reload missing")
	}
	if len(projects) != 2 || projects[0].Name != a || projects[1].Name != b {
		t.Fatalf("projects: %+v", projects)
	}
	if !strings.Contains(console.String(), "reload accepted") {
		t.Errorf("the running project's scheduler was not handed its bees.toml: %q", console.String())
	}
	added, err := projects[1].Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = added.(*projectLoop).Close() }()
	pool := added.(*projectLoop).SharedPool()
	if pool != original.(*projectLoop).SharedPool() || pool.Size() != 3 {
		t.Fatal("reload replaced the original shared pool")
	}
	// A closed loop is forgotten: the next reload has nothing to hand it.
	_ = added.(*projectLoop).Close()
	if rt.running(b) != nil {
		t.Fatal("a closed project is still recorded for reloads")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("reload handler leaked")
	}
}

func TestMachineRunOptionsReachEveryScheduler(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	m := loadMachine(t, writeProject(t, "acme/a", ""), writeProject(t, "acme/b", ""))
	opts := runOptions{once: true, roles: "qa", skipDoctor: true}
	for _, p := range opts.projects(machineDaemon(&globalFlags{}, m).Projects) {
		l, err := p.Start(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		loop := l.(*projectLoop)
		defer func() { _ = loop.Close() }()
		if !loop.Once || len(loop.OnlyRoles) != 1 || !loop.OnlyRoles["qa"] {
			t.Fatalf("options not applied: %+v", loop)
		}
	}
	// With preflight enabled an unavailable tool is a per-project start error.
	t.Setenv("PATH", t.TempDir())
	_, err := (runOptions{}).projects(machineDaemon(&globalFlags{}, m).Projects)[0].Start(context.Background())
	if err == nil || !strings.Contains(err.Error(), "git") {
		t.Fatalf("bad toolchain accepted: %v", err)
	}
}

// Machine preflight happens under the live view. Its diagnostics belong in
// the project's log and status error, without writes over the terminal.
func TestMachinePreflightLogsFailureAndReportsDetailsToView(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	m := loadMachine(t, writeProject(t, "acme/broken", ""))
	var console bytes.Buffer
	g := &globalFlags{logger: logging.New(logging.Options{Console: &console})}
	t.Cleanup(func() { _ = g.logger.Close() })
	// Build while git is available; only preflight sees the broken toolchain.
	loop, err := machineDaemon(g, m).Projects[0].Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon.Daemon{Logger: g.logger.Logger, Projects: (runOptions{}).projects([]daemon.Project{{
		Name:  m.Configs[0].Path,
		Start: func(context.Context) (daemon.Loop, error) { return loop, nil },
	}})}
	projects, stop := machineView(context.Background(), d, m)
	defer stop()
	restore := quietConsole(g.logger, g.console, m.Configs[0].Logging, &console)
	defer restore()
	console.Reset()
	t.Setenv("PATH", t.TempDir())
	out := captureStdout(t, func() {
		if err := d.Run(context.Background()); err == nil {
			t.Error("broken toolchain accepted")
		}
	})
	if out != "" || console.Len() != 0 {
		t.Errorf("preflight wrote over the view: stdout=%q console=%q", out, console.String())
	}
	data, err := os.ReadFile(filepath.Join(m.Configs[0].StateDir(), "bees.log"))
	if err != nil {
		t.Fatal(err)
	}
	_, statusErr := projects[0].Status()
	if statusErr == nil {
		t.Fatal("view has no failure")
	}
	if !strings.Contains(string(data), `"project":"acme/broken"`) {
		t.Errorf("failure log has no project attribution: %s", data)
	}
	for _, want := range []string{"git", "not found", "install"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("project log missing %q: %s", want, data)
		}
		if !strings.Contains(statusErr.Error(), want) {
			t.Errorf("view missing %q: %v", want, statusErr)
		}
	}
}
