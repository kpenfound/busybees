package main

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/versions"
)

// writeProject writes a bees.toml for repo into a fresh git clone, with
// extra appended inside [project], and returns its path.
func writeProject(t *testing.T, repo, extra string) string {
	t.Helper()
	_, clone := testutil.SetupRepos(t)
	path := filepath.Join(clone, "bees.toml")
	body := "version = 2\n[project]\nrepo = \"" + repo + "\"\ndefault_branch = \"main\"\n" + extra
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func loadMachine(t *testing.T, projects ...string) *config.Machine {
	t.Helper()
	path := filepath.Join(t.TempDir(), "machine.toml")
	quoted := make([]string, len(projects))
	for i, p := range projects {
		quoted[i] = `"` + filepath.ToSlash(p) + `"`
	}
	if err := os.WriteFile(path, []byte("projects = ["+strings.Join(quoted, ", ")+"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := config.LoadMachine(path)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// The daemon builds one scheduler per project listed, each from its own
// bees.toml with its own state directory, and each project's records reach
// the shared console and its own bees.log, never the other project's.
func TestMachineDaemonBuildsASchedulerPerProject(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	foo := writeProject(t, "acme/foo", "")
	bar := writeProject(t, "acme/bar", "")
	m := loadMachine(t, foo, bar)

	var console bytes.Buffer
	g := &globalFlags{logger: logging.New(logging.Options{Format: logging.FormatJSON, Level: slog.LevelInfo, Console: &console})}
	t.Cleanup(func() { _ = g.logger.Close() })
	d := machineDaemon(g, m)
	if len(d.Projects) != 2 || d.Projects[0].Name != foo || d.Projects[1].Name != bar {
		t.Fatalf("projects: %+v", d.Projects)
	}

	var loops []*projectLoop
	for _, p := range d.Projects {
		l, err := p.Start(context.Background())
		if err != nil {
			t.Fatalf("%s: %v", p.Name, err)
		}
		loop := l.(*projectLoop)
		t.Cleanup(func() { _ = loop.file.Close() })
		loops = append(loops, loop)
	}
	fooApp, barApp := loops[0].app, loops[1].app
	if fooApp.cfg.Project.Repo != "acme/foo" || barApp.cfg.Project.Repo != "acme/bar" {
		t.Fatalf("repos: %s, %s", fooApp.cfg.Project.Repo, barApp.cfg.Project.Repo)
	}
	if fooApp.store.Dir == barApp.store.Dir {
		t.Fatalf("both projects share the state directory %s", fooApp.store.Dir)
	}
	if want := filepath.Join(filepath.Dir(foo), ".bees"); fooApp.store.Dir != want {
		t.Errorf("foo's state directory: %s, want %s", fooApp.store.Dir, want)
	}

	fooApp.log.Info("polled foo")
	barApp.log.Info("polled bar")
	read := func(a *app) string {
		b, err := os.ReadFile(filepath.Join(a.store.Dir, "bees.log"))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	fooLog, barLog := read(fooApp), read(barApp)
	if !strings.Contains(fooLog, "polled foo") || !strings.Contains(fooLog, `"project":"acme/foo"`) {
		t.Errorf("foo's bees.log: %q", fooLog)
	}
	if strings.Contains(fooLog, "polled bar") || strings.Contains(barLog, "polled foo") {
		t.Errorf("a project's record reached the other's bees.log: foo %q, bar %q", fooLog, barLog)
	}
	if !strings.Contains(console.String(), "polled foo") || !strings.Contains(console.String(), "polled bar") {
		t.Errorf("console: %q", console.String())
	}
}

// A project that cannot be built fails its own start, naming what is wrong,
// and the other project still builds.
func TestMachineDaemonProjectFailsToStartAlone(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	notAClone := t.TempDir()
	broken := writeProject(t, "acme/broken", "dir = \""+filepath.ToSlash(notAClone)+"\"\n")
	good := writeProject(t, "acme/good", "")
	m := loadMachine(t, broken, good)

	g := &globalFlags{logger: logging.New(logging.Options{Console: &bytes.Buffer{}})}
	t.Cleanup(func() { _ = g.logger.Close() })
	d := machineDaemon(g, m)
	if _, err := d.Projects[0].Start(context.Background()); err == nil || !strings.Contains(err.Error(), "project.dir") {
		t.Fatalf("broken project: %v, want an error naming project.dir", err)
	}
	l, err := d.Projects[1].Start(context.Background())
	if err != nil {
		t.Fatalf("good project: %v", err)
	}
	t.Cleanup(func() { _ = l.(*projectLoop).file.Close() })
}
