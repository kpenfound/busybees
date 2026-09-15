package main

import (
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
	"github.com/kpenfound/busybees/internal/versions"
)

func TestMachineReloadRejectsInvalidConfigAndPreservesPool(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	a, b := writeProject(t, "acme/a", ""), writeProject(t, "acme/b", "")
	m := loadMachineWith(t, "max_developers = 3\n", a)
	g := &globalFlags{}
	build := machineProjectFactory(g, m)
	original, err := build(m)[0].Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = original.(*projectLoop).Close() }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hup := make(chan os.Signal)
	changes := make(chan []daemon.Project)
	done := make(chan struct{})
	go func() {
		defer close(done)
		reloadMachine(ctx, hup, changes, m.Path, build, slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(m.Path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("projects = [\"missing.toml\"]\n")
	hup <- syscall.SIGHUP
	select {
	case <-changes:
		t.Fatal("invalid reload changed projects")
	case <-time.After(50 * time.Millisecond):
	}
	write("projects = [\"" + filepath.ToSlash(a) + "\",\"" + filepath.ToSlash(b) + "\"]\nmax_developers = 9\n")
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
	added, err := projects[1].Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = added.(*projectLoop).Close() }()
	pool := added.(*projectLoop).SharedPool()
	if pool != original.(*projectLoop).SharedPool() || pool.Size() != 3 {
		t.Fatal("reload replaced the original shared pool")
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
