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

// The commands that run sessions also write their log to
// <state_dir>/bees.log, so nothing a terminal UI covers up is lost. The file
// gets every record at debug level, whatever the console flags say, so it
// holds everything that reached stderr and more.
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
// and the terminal check go through picks between the view and the console,
// and with no terminal `bees run` still runs the scheduler and logs.
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

// The browser key hands the URL to the platform's opener as an argument of
// its own. It is built from the configured repository, and a URL that
// reached a shell would be a hazard rather than a link.
func TestTheBrowserOpenerNeverBuildsAShellCommand(t *testing.T) {
	const url = "https://github.com/acme/widgets/issues/31"
	for _, tc := range []struct{ goos, want string }{
		{"darwin", "open"},
		{"linux", "xdg-open"},
		{"windows", "rundll32"},
	} {
		t.Run(tc.goos, func(t *testing.T) {
			cmd := browserCommand(tc.goos, url)
			if filepath.Base(cmd.Path) != tc.want && cmd.Args[0] != tc.want {
				t.Errorf("%s opens with %q, want %q", tc.goos, cmd.Args[0], tc.want)
			}
			if got := cmd.Args[len(cmd.Args)-1]; got != url {
				t.Errorf("%s was given %q as the URL, want %q", tc.goos, got, url)
			}
			for _, a := range cmd.Args {
				if strings.ContainsAny(a, "|;&") && a != url {
					t.Errorf("%s builds a shell command: %q", tc.goos, cmd.Args)
				}
			}
		})
	}
}
