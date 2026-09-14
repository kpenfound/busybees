package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/versions"
)

// The view names each project by its repository's name, links its rows to
// that repository, and reads that project's own status.json and mailbox —
// before its scheduler exists, since the daemon builds the schedulers only
// once it runs, with the view already up.
func TestMachineViewReadsEachProjectsOwnState(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	foo := writeProject(t, "acme/foo", "")
	bar := writeProject(t, "acme/bar", "")
	m := loadMachine(t, foo, bar)
	g := &globalFlags{logger: logging.New(logging.Options{Console: &bytes.Buffer{}})}
	t.Cleanup(func() { _ = g.logger.Close() })
	d := machineDaemon(g, m)
	projects, stop := machineView(context.Background(), d, m)
	defer stop()

	if len(projects) != 2 || projects[0].Name != "foo" || projects[1].Name != "bar" {
		t.Fatalf("projects: %+v", projects)
	}
	if projects[0].Repo != "acme/foo" || projects[1].Repo != "acme/bar" {
		t.Errorf("repos: %s, %s", projects[0].Repo, projects[1].Repo)
	}

	// foo's state directory has a status.json and a mailbox entry; bar's
	// has nothing yet.
	store := state.New(filepath.Join(filepath.Dir(foo), ".bees"))
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	st, _ := json.Marshal(state.Status{Queues: map[string]int{"ready": 4}})
	if err := os.WriteFile(filepath.Join(store.Dir, "status.json"), st, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := mail.Open(store.MailDir()).Send(mail.Message{From: "human", To: "developer", Subject: "hi", Body: "there"}); err != nil {
		t.Fatal(err)
	}
	got, err := projects[0].Status()
	if err != nil || got.Queues["ready"] != 4 {
		t.Errorf("foo's status: %+v, %v", got, err)
	}
	counts, err := projects[0].Mail()
	if err != nil || counts["developer"] != 1 {
		t.Errorf("foo's mail: %v, %v", counts, err)
	}
	got, err = projects[1].Status()
	if err != nil || len(got.Queues) != 0 {
		t.Errorf("bar's status is not empty: %+v, %v", got, err)
	}
	counts, err = projects[1].Mail()
	if err != nil || len(counts) != 0 {
		t.Errorf("bar's mail is not empty: %v, %v", counts, err)
	}
}

// Before a project's scheduler exists, stopping a session or messaging a
// role says the project has not started; once the daemon has started it,
// both reach that project's scheduler and mailbox, and its events reach the
// view's channel.
func TestMachineViewReachesTheSchedulerOnceItHasStarted(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	foo := writeProject(t, "acme/foo", "")
	m := loadMachine(t, foo)
	// --verbose, which streams every session event to stderr: not under the
	// view, which owns the terminal.
	g := &globalFlags{logger: logging.New(logging.Options{Console: &bytes.Buffer{}}), verbose: true}
	t.Cleanup(func() { _ = g.logger.Close() })
	d := machineDaemon(g, m)
	projects, stop := machineView(context.Background(), d, m)
	defer stop()

	if err := projects[0].Kill("developer-issue-1-r1"); err == nil || !strings.Contains(err.Error(), "foo has not started yet") {
		t.Errorf("Kill before the start: %v", err)
	}
	if err := projects[0].Send("developer", 1, 0, "s", "b"); err == nil || !strings.Contains(err.Error(), "foo has not started yet") {
		t.Errorf("Send before the start: %v", err)
	}

	loop, err := d.Projects[0].Start(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	pl := loop.(*projectLoop)
	t.Cleanup(func() { _ = pl.Close() })
	if pl.app.runner.Stream != nil {
		t.Error("the project's runner still streams session events to stderr under the view")
	}
	// A session the scheduler does not know is the scheduler's answer, not
	// the stand-in's.
	if err := projects[0].Kill("developer-issue-1-r1"); err == nil || strings.Contains(err.Error(), "not started") {
		t.Errorf("Kill after the start: %v", err)
	}
	if err := projects[0].Send("developer", 1, 0, "from the view", "hello"); err != nil {
		t.Fatal(err)
	}
	msgs, err := pl.app.mail.List(mail.Filter{To: "developer"})
	if err != nil || len(msgs) != 1 || msgs[0].From != scheduler.HumanSender || msgs[0].Body != "hello" {
		t.Errorf("foo's mailbox: %+v, %v", msgs, err)
	}
}

// A project whose start failed says so through its status read, so the
// view's header names it, and stopping or messaging it says why.
func TestMachineViewReportsAProjectThatDidNotStart(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	notAClone := t.TempDir()
	broken := writeProject(t, "acme/broken", "dir = \""+filepath.ToSlash(notAClone)+"\"\n")
	m := loadMachine(t, broken)
	g := &globalFlags{logger: logging.New(logging.Options{Console: &bytes.Buffer{}})}
	t.Cleanup(func() { _ = g.logger.Close() })
	d := machineDaemon(g, m)
	projects, stop := machineView(context.Background(), d, m)
	defer stop()

	if _, err := d.Projects[0].Start(context.Background()); err == nil {
		t.Fatal("the broken project started")
	}
	if _, err := projects[0].Status(); err == nil || !strings.Contains(err.Error(), "broken did not start: ") || !strings.Contains(err.Error(), "project.dir") {
		t.Errorf("Status: %v", err)
	}
	if err := projects[0].Kill("x"); err == nil || !strings.Contains(err.Error(), "broken did not start") {
		t.Errorf("Kill: %v", err)
	}
}

// Two projects whose repositories share a name are told apart by the whole
// owner/name, and a project whose repository cannot be resolved is named by
// the directory its bees.toml is in.
func TestProjectNamesTellProjectsApart(t *testing.T) {
	foo := writeProject(t, "acme/foo", "")
	other := writeProject(t, "other/foo", "")
	bar := writeProject(t, "acme/bar", "")
	// No repository set and no git remote to derive one from.
	_, clone := testutil.SetupRepos(t)
	unresolved := filepath.Join(clone, "bees.toml")
	if err := os.WriteFile(unresolved, []byte("version = 2\n[project]\ndir = \""+filepath.ToSlash(t.TempDir())+"\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := loadMachine(t, foo, other, bar, unresolved)
	got := projectNames(context.Background(), m.Configs)
	want := []string{"acme/foo", "other/foo", "bar", filepath.Base(clone)}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("names %v, want %v", got, want)
	}
}

// forward copies a scheduler's stream into the view's channel, drops what
// the view has no room for rather than waiting on it, and stops when told.
func TestForwardCopiesEventsUntilStopped(t *testing.T) {
	from := make(chan scheduler.Event, 4)
	to := make(chan scheduler.Event, 1)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { forward(from, to, stop); close(done) }()
	from <- scheduler.Event{Kind: scheduler.EventPoll, Session: "first"}
	from <- scheduler.Event{Kind: scheduler.EventPoll, Session: "second"}
	select {
	case ev := <-to:
		if ev.Session != "first" {
			t.Errorf("forwarded %q first", ev.Session)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing was forwarded")
	}
	close(stop)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forward did not stop")
	}
}
