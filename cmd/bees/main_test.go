package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
)

// runRoot executes the CLI with args, stopping before any command body runs:
// every case here fails (or is inspected) in PersistentPreRunE.
func runRoot(t *testing.T, args ...string) error {
	t.Helper()
	root := newRoot()
	root.SetArgs(args)
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&bytes.Buffer{})
	return root.Execute()
}

func TestQuietRejectsDebug(t *testing.T) {
	for _, args := range [][]string{
		{"version", "--quiet", "-v"},
		{"version", "--quiet", "--log-level", "debug"},
	} {
		err := runRoot(t, args...)
		if err == nil || !strings.Contains(err.Error(), "--quiet cannot be combined") {
			t.Errorf("%v: got %v", args, err)
		}
	}
}

func TestInvalidLogFlags(t *testing.T) {
	err := runRoot(t, "version", "--log-format", "bogus")
	if err == nil || !strings.Contains(err.Error(), "text, json") {
		t.Errorf("--log-format bogus: got %v", err)
	}
	err = runRoot(t, "version", "--log-level", "loud")
	if err == nil || !strings.Contains(err.Error(), "debug, info, warn, error") {
		t.Errorf("--log-level loud: got %v", err)
	}
}

func TestQuietWithWarnIsFine(t *testing.T) {
	if err := runRoot(t, "version", "--quiet", "--log-level", "warn"); err != nil {
		t.Fatal(err)
	}
}

func TestLogFlagsFromEnvironment(t *testing.T) {
	t.Setenv("BEES_LOG_FORMAT", "json")
	t.Setenv("BEES_LOG_LEVEL", "warn")
	var out bytes.Buffer
	root := newRoot()
	root.SetArgs([]string{"version"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	slog.Info("dropped by the env level")
	slog.Warn("kept", "from", "env")
	if strings.Contains(out.String(), "dropped") {
		t.Errorf("BEES_LOG_LEVEL ignored: %q", out.String())
	}
	if !strings.HasPrefix(strings.TrimSpace(out.String()), "{") {
		t.Errorf("BEES_LOG_FORMAT ignored: %q", out.String())
	}
}

// The flag beats the environment variable.
func TestLogFlagBeatsEnvironment(t *testing.T) {
	t.Setenv("BEES_LOG_FORMAT", "json")
	var out bytes.Buffer
	root := newRoot()
	root.SetArgs([]string{"version", "--log-format", "text"})
	root.SetOut(&bytes.Buffer{})
	root.SetErr(&out)
	if err := root.Execute(); err != nil {
		t.Fatal(err)
	}
	slog.Info("hello")
	if !strings.Contains(out.String(), "msg=hello") {
		t.Errorf("flag did not win: %q", out.String())
	}
}

// An invalid environment value is an error too, and names the valid ones.
func TestInvalidEnvironmentValue(t *testing.T) {
	t.Setenv("BEES_LOG_FORMAT", "yaml")
	err := runRoot(t, "version")
	if err == nil || !strings.Contains(err.Error(), "text, json") {
		t.Errorf("got %v", err)
	}
}
