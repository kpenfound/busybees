package main

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/state"
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

func TestQueuesTextShowsReadySizes(t *testing.T) {
	st := state.Status{
		Queues:     map[string]int{"ready": 4, "triage": 2},
		ReadySizes: map[string]int{"xs": 1, "s": 2, "m": 1},
	}
	got := queuesText(st)
	want := "  ready          4  (xs 1, s 2, m 1)\n  triage         2\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	// No breakdown recorded (an old status.json): just the count.
	st.ReadySizes = nil
	if got := queuesText(st); strings.Contains(got, "(") {
		t.Fatalf("unexpected breakdown: %q", got)
	}
	// Issues the scheduler has not sized yet are reported as unsized.
	st.ReadySizes = map[string]int{"l": 1, "": 3}
	if got := queuesText(st); !strings.Contains(got, "(l 1, unsized 3)") {
		t.Fatalf("unsized issues: %q", got)
	}
}

func TestClaudeBin(t *testing.T) {
	t.Setenv("BEES_CLAUDE_BIN", "")
	if got := claudeBin(); got != "claude" {
		t.Errorf("default claude binary = %q", got)
	}
	t.Setenv("BEES_CLAUDE_BIN", "/opt/claude")
	if got := claudeBin(); got != "/opt/claude" {
		t.Errorf("BEES_CLAUDE_BIN = %q", got)
	}
}

// A typo in a subcommand must fail, exactly like a typo in a top-level
// command: a group that only printed its help and exited 0 left a session
// with no way to tell its command had not run (#83).
func TestUnknownSubcommandIsAnError(t *testing.T) {
	for _, group := range []string{"config", "prompts", "mail", "issue", "labels", "skills", "mcp"} {
		err := runRoot(t, group, "bogus")
		want := `unknown command "bogus" for "bees ` + group + `"`
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("bees %s bogus: got %v, want an error containing %q", group, err, want)
		}
		// The bare group still prints its help and succeeds.
		if err := runRoot(t, group); err != nil {
			t.Errorf("bees %s: got %v, want nil", group, err)
		}
	}
}

// The realistic shape from #83: a typo'd subcommand carrying the subcommand's
// flags. Cobra parses flags before it validates Args, so the error names the
// flag rather than the command — what matters is that it is an error at all.
func TestUnknownSubcommandWithFlagsIsAnError(t *testing.T) {
	for _, args := range [][]string{
		{"issue", "craete", "--bug", "--title", "x"},
		{"mail", "snd", "--to", "project_manager"},
	} {
		if err := runRoot(t, args...); err == nil {
			t.Errorf("bees %v: got nil, want an error", args)
		}
	}
}

// An unknown top-level command was already rejected; keep it that way.
func TestUnknownCommandIsAnError(t *testing.T) {
	err := runRoot(t, "boguscmd")
	if err == nil || !strings.Contains(err.Error(), `unknown command "boguscmd" for "bees"`) {
		t.Errorf("got %v", err)
	}
}
