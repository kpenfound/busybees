package main

import (
	"bytes"
	"context"
	"encoding/json"
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

// The terminal UI is only drawn when a person asked for it and stdout is a
// terminal. --no-tui turns it off whatever stdout is, and a redirected or
// piped stdout turns it off on its own, so a service never draws one.
func TestTUINeedsATerminalAndNoFlag(t *testing.T) {
	// The real seam first: a regular file is not a terminal.
	f, err := os.Create(filepath.Join(t.TempDir(), "out"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	if isTerminal(f) {
		t.Error("a regular file was reported as a terminal")
	}

	real := isTerminal
	t.Cleanup(func() { isTerminal = real })
	isTerminal = func(*os.File) bool { return true }
	if !tuiMode(false, os.Stdout) {
		t.Error("a terminal and no --no-tui: want the UI on")
	}
	if tuiMode(true, os.Stdout) {
		t.Error("--no-tui did not turn the UI off")
	}
	isTerminal = func(*os.File) bool { return false }
	if tuiMode(false, os.Stdout) {
		t.Error("stdout is not a terminal: want the UI off")
	}
	if tuiMode(true, os.Stdout) {
		t.Error("--no-tui with no terminal: want the UI off")
	}
}

// The one thing this change adds to the run path — deciding whether to draw
// a terminal UI — must not reach the console: it is recorded at debug level,
// so `bees run` and `bees run --no-tui` print what they printed before the
// flag existed (#244).
func TestTheTUIDecisionStaysOutOfTheConsole(t *testing.T) {
	real := isTerminal
	t.Cleanup(func() { isTerminal = real })
	isTerminal = func(*os.File) bool { return true }

	for _, noTUI := range []bool{false, true} {
		var console bytes.Buffer
		lg := logging.New(logging.Options{Format: logging.FormatText, Level: slog.LevelInfo, Console: &console})
		on := logTUIMode(lg.Logger, noTUI, os.Stdout)
		if err := lg.Close(); err != nil {
			t.Fatal(err)
		}
		if on == noTUI {
			t.Errorf("--no-tui=%v: the UI is %v", noTUI, on)
		}
		if console.Len() != 0 {
			t.Errorf("--no-tui=%v printed %q; the decision belongs in the log file only", noTUI, console.String())
		}
	}
}

// `bees run --no-tui`, `bees run` and `bees tick` log the same thing, byte
// for byte: the console logger is built from the global flags alone and the
// flag changes none of it (#244). Only the timestamp differs between two
// invocations, so it is dropped before the comparison. The commands are
// stopped at loadConfig, before anything reaches GitHub, so what this
// compares is the console logging the flag could have changed.
func TestNoTUIAndTickKeepTodaysLogOutput(t *testing.T) {
	// A config path that does not exist: the command fails in loadConfig,
	// after the logging the comparison is about has been set up and before
	// anything touches GitHub.
	missing := filepath.Join(t.TempDir(), "bees.toml")
	record := func(t *testing.T, args ...string) (string, consoleFlags) {
		t.Helper()
		g, root := newRootWithFlags()
		var console bytes.Buffer
		root.SetArgs(append(args, "--config", missing, "--log-format", "json"))
		root.SetOut(&bytes.Buffer{})
		root.SetErr(&console)
		if err := root.Execute(); err == nil {
			t.Fatal("the missing config should have failed the command")
		}
		slog.Info("polled github", "issues", 3)
		return withoutTime(t, console.String()), g.console
	}

	want, wantFlags := record(t, "run")
	if want == "" {
		t.Fatal("nothing was logged")
	}
	for _, args := range [][]string{{"run", "--no-tui"}, {"run", "--no-tui", "--once"}, {"tick"}} {
		got, flags := record(t, args...)
		if got != want {
			t.Errorf("`bees %s` logs\n\t%q\nwant\n\t%q", strings.Join(args, " "), got, want)
		}
		if flags != wantFlags {
			t.Errorf("`bees %s` resolved console options %+v, want %+v", strings.Join(args, " "), flags, wantFlags)
		}
	}

	// `bees tick` never draws a UI, so it does not offer the flag.
	if err := runRoot(t, "tick", "--no-tui"); err == nil || !strings.Contains(err.Error(), "unknown flag: --no-tui") {
		t.Errorf("tick --no-tui: got %v", err)
	}
}

// withoutTime drops the "time" field of every JSON log record, which is the
// only thing that differs between two runs of the same command.
func withoutTime(t *testing.T, out string) string {
	t.Helper()
	var b strings.Builder
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		rec := map[string]any{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line is not JSON: %q", line)
		}
		delete(rec, "time")
		enc, err := json.Marshal(rec)
		if err != nil {
			t.Fatal(err)
		}
		b.Write(enc)
		b.WriteByte('\n')
	}
	return b.String()
}

// The commands that run sessions also write their log to
// <state_dir>/bees.log, so nothing a terminal UI covers up is lost. The file
// gets every record at debug level, whatever the console flags say, so it
// holds everything that reached stderr and more (#244).
func TestSchedulerCommandsWriteTheStateDirLogFile(t *testing.T) {
	t.Setenv(versions.EnvSkip, "1")
	_, clone := testutil.SetupRepos(t)
	cfgPath := filepath.Join(clone, "bees.toml")
	body := "version = 1\n[project]\nrepo = \"acme/widgets\"\ndefault_branch = \"main\"\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var console bytes.Buffer
	g := &globalFlags{config: cfgPath}
	g.logger = logging.New(logging.Options{Format: logging.FormatJSON, Level: slog.LevelInfo, Console: &console})
	t.Cleanup(func() { _ = g.logger.Close() })
	slog.SetDefault(g.logger.Logger)

	a, err := newApp(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.scheduler(); err != nil {
		t.Fatal(err)
	}
	a.log.Info("polled github", "issues", 3)
	a.log.Debug("dispatching", "issue", 7)

	path := filepath.Join(a.cfg.StateDir(), "bees.log")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no log file at %s: %v", path, err)
	}
	file := string(b)
	if !strings.Contains(console.String(), "polled github") {
		t.Fatalf("console: %q", console.String())
	}
	if !strings.Contains(file, "polled github") || !strings.Contains(file, `"issues":3`) {
		t.Errorf("%s lost the console record: %q", path, file)
	}
	if !strings.Contains(file, "dispatching") {
		t.Errorf("%s did not get the debug record the console dropped: %q", path, file)
	}
}

// While the view owns the terminal the console log has to be silent, or its
// records scribble over the panels. Only the console destination changes:
// the state directory's log file still gets every record, so nothing the
// view covers up is lost — and when the view is gone the console gets its
// logging back, which is what a person who pressed Ctrl-C twice watches the
// drain in.
func TestTheViewSilencesTheConsoleAndGivesItBack(t *testing.T) {
	var console bytes.Buffer
	lg := logging.New(logging.Options{Format: logging.FormatText, Level: slog.LevelInfo, Console: &console})
	t.Cleanup(func() { _ = lg.Close() })
	file := filepath.Join(t.TempDir(), "bees.log")
	if err := lg.AttachFile(file); err != nil {
		t.Fatal(err)
	}

	restore := quietConsole(lg, consoleFlags{format: logging.FormatText, level: slog.LevelInfo}, config.Logging{}, &console)
	lg.Info("dispatching", "issue", 7)
	if console.Len() != 0 {
		t.Errorf("the console printed %q while the view was up", console.String())
	}
	restore()
	lg.Info("polled github", "issues", 3)
	if !strings.Contains(console.String(), "polled github") {
		t.Errorf("the console did not get its logging back: %q", console.String())
	}
	if strings.Contains(console.String(), "dispatching") {
		t.Errorf("the silenced record reached the console after all: %q", console.String())
	}

	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "dispatching") {
		t.Errorf("%s lost the record the console was not shown: %q", file, string(b))
	}
}

// `bees run` draws the view only when it decided to: the same seam the flag
// and the terminal check go through picks between the view and the console
// (#244), and with no terminal `bees run` still runs the scheduler and logs.
func TestRunDrawsTheViewOnlyWhenTheModeSaysSo(t *testing.T) {
	real := isTerminal
	t.Cleanup(func() { isTerminal = real })
	for _, tc := range []struct {
		name     string
		terminal bool
		noTUI    bool
		want     bool
	}{
		{"a terminal", true, false, true},
		{"--no-tui", true, true, false},
		{"a pipe", false, false, false},
	} {
		isTerminal = func(*os.File) bool { return tc.terminal }
		var console bytes.Buffer
		lg := logging.New(logging.Options{Format: logging.FormatText, Level: slog.LevelDebug, Console: &console})
		if got := logTUIMode(lg.Logger, tc.noTUI, os.Stdout); got != tc.want {
			t.Errorf("%s: the view is %v, want %v", tc.name, got, tc.want)
		}
		if err := lg.Close(); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(console.String(), "terminal UI") {
			t.Errorf("%s: the decision was not recorded: %q", tc.name, console.String())
		}
	}
}
