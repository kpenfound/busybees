package main

import (
	"errors"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
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
