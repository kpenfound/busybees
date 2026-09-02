package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
)

// sandboxConfig is a config whose state dir is somewhere nothing else writes,
// standing in for the "-c /tmp/sandbox.toml" of issue #71.
func sandboxConfig(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	cfg, err := config.Parse(`
[project]
repo = "owner/name"
state_dir = "sandbox-state"
`, dir+"/bees.toml")
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

// An explicit --config must win over an ambient BEES_STATE_DIR: inside a
// session both are set, and mail sent with a sandbox config used to land in the
// live factory's mailbox.
func TestMailStateDirPrefersExplicitConfig(t *testing.T) {
	cfg := sandboxConfig(t)
	loaded := 0
	load := func() (*config.Config, error) { loaded++; return cfg, nil }

	got, err := mailStateDir(cfg.Path, "/live/state", load)
	if err != nil {
		t.Fatal(err)
	}
	if want := cfg.StateDir(); got != want {
		t.Errorf("state dir = %q, want the config's %q", got, want)
	}
	if loaded != 1 {
		t.Errorf("load called %d times, want 1", loaded)
	}
}

// Without an explicit --config, a session reaches its own mailbox through the
// environment and no bees.toml is searched for or read.
func TestMailStateDirUsesEnvWithoutExplicitConfig(t *testing.T) {
	load := func() (*config.Config, error) {
		t.Error("config loaded although $BEES_STATE_DIR is set")
		return nil, errors.New("must not be called")
	}

	got, err := mailStateDir("", "/live/state", load)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/live/state" {
		t.Errorf("state dir = %q, want /live/state", got)
	}
}

// Outside a session with no flag, the config found by configPath decides.
func TestMailStateDirFallsBackToTheConfig(t *testing.T) {
	cfg := sandboxConfig(t)
	got, err := mailStateDir("", "", func() (*config.Config, error) { return cfg, nil })
	if err != nil {
		t.Fatal(err)
	}
	if want := cfg.StateDir(); got != want {
		t.Errorf("state dir = %q, want %q", got, want)
	}
}

func TestMailStateDirReportsLoadFailures(t *testing.T) {
	boom := errors.New("no bees.toml found")
	if _, err := mailStateDir("", "", func() (*config.Config, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

// doneLong's status table is derived from session.ValidOutcomes, so it
// cannot drift from what "bees done" actually validates (#355: it used to
// be a hardcoded literal that omitted "failed" for both managers).
func TestDoneLongMatchesValidOutcomes(t *testing.T) {
	long := doneLong()
	lines := strings.Split(long, "\n")
	for _, role := range config.Roles {
		prefix := "  " + role
		var statuses []string
		found := false
		for _, line := range lines {
			if !strings.HasPrefix(line, prefix) {
				continue
			}
			found = true
			rest := strings.TrimSpace(strings.TrimPrefix(line, prefix))
			statuses = strings.Split(rest, ", ")
			break
		}
		if !found {
			t.Fatalf("no line for role %q in:\n%s", role, long)
		}

		want := session.ValidOutcomes(role)
		got := slices.Clone(statuses)
		slices.Sort(got)
		wantSorted := slices.Clone(want)
		slices.Sort(wantSorted)
		if !slices.Equal(got, wantSorted) {
			t.Errorf("role %s: statuses = %v, want %v (session.ValidOutcomes)", role, statuses, want)
		}
	}
}
