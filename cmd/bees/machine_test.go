package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
)

// writeMachine writes a project bees.toml and a machine config listing it,
// returning the machine config's path.
func writeMachine(t *testing.T, projects string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	project := "version = 2\n[project]\nrepo = \"a/b\"\ndefault_branch = \"main\"\n"
	if err := os.WriteFile(filepath.Join(dir, "foo", "bees.toml"), []byte(project), 0o644); err != nil {
		t.Fatal(err)
	}
	machine := filepath.Join(dir, "bees.toml")
	if err := os.WriteFile(machine, []byte("projects = "+projects+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return machine
}

// The active config, however it was resolved, can be a machine config:
// `bees config validate` tells it apart and validates it as one.
func TestConfigValidateAMachineConfig(t *testing.T) {
	machine := writeMachine(t, `["foo/bees.toml"]`)
	for name, setup := range map[string]func(t *testing.T) []string{
		"flag":   func(t *testing.T) []string { return []string{"--config", machine} },
		"env":    func(t *testing.T) []string { t.Setenv("BEES_CONFIG", machine); return nil },
		"search": func(t *testing.T) []string { t.Chdir(filepath.Dir(machine)); return nil },
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("BEES_CONFIG", "")
			root := newRoot()
			var out bytes.Buffer
			root.SetArgs(append([]string{"config", "validate"}, setup(t)...))
			root.SetOut(&out)
			root.SetErr(&bytes.Buffer{})
			if err := root.Execute(); err != nil {
				t.Fatal(err)
			}
			if want := "is a valid machine config (1 project)"; !strings.Contains(out.String(), want) {
				t.Errorf("output %q, want %q", out.String(), want)
			}
		})
	}
}

func TestConfigValidateReportsTheBadMachineEntry(t *testing.T) {
	machine := writeMachine(t, `["foo/bees.toml", "missing/bees.toml"]`)
	err := runRoot(t, "config", "validate", "--config", machine)
	if err == nil || !strings.Contains(err.Error(), `projects[1] = "missing/bees.toml"`) {
		t.Fatalf("got %v, want an error naming projects[1]", err)
	}
}

// A command that needs a project refuses a machine config by name rather than
// reporting projects as an unknown key.
func TestLoadConfigRefusesAMachineConfig(t *testing.T) {
	g := &globalFlags{config: writeMachine(t, `["foo/bees.toml"]`)}
	_, err := loadConfig(g)
	if !errors.Is(err, config.ErrMachineConfig) {
		t.Fatalf("loadConfig = %v, want ErrMachineConfig", err)
	}
}
