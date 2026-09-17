package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/logging"
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
