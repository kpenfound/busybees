package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/logging"
)

// A log file that cannot be opened is a warning, not a failure: the run
// continues with console logging only.
func TestAttachLogFileWarnsAndCarriesOn(t *testing.T) {
	// A path under a regular file fails with ENOTDIR, for root too.
	file := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(file, "bees.log")

	var buf bytes.Buffer
	lg := logging.New(logging.Options{Format: logging.FormatText, Console: &buf})
	t.Cleanup(func() { _ = lg.Close() })

	attachLogFile(lg, lg.Logger, path)
	lg.Info("polled github")

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want a warning and the later record, got %q", buf.String())
	}
	if !strings.Contains(lines[0], "level=WARN") || !strings.Contains(lines[0], "cannot open the log file") {
		t.Errorf("not a warning: %q", lines[0])
	}
	if !strings.Contains(lines[0], path) || !strings.Contains(lines[0], "err=") {
		t.Errorf("warning names neither the path nor the reason: %q", lines[0])
	}
	if !strings.Contains(lines[1], "polled github") {
		t.Errorf("console stopped working: %q", lines[1])
	}
}

// loadConfig applies bees.toml's [logging] table to the console logger the
// flags built, for every command that reads the file.
func TestLoadConfigAppliesTheLoggingTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bees.toml")
	body := "version = 1\n[project]\nrepo = \"a/b\"\ndefault_branch = \"main\"\n" +
		"[logging]\nformat = \"json\"\nlevel = \"warn\"\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	g := &globalFlags{config: path}
	g.logger = logging.New(logging.Options{Format: logging.FormatText, Level: slog.LevelInfo, Console: &buf})
	t.Cleanup(func() { _ = g.logger.Close() })
	g.console = consoleFlags{format: logging.FormatText, level: slog.LevelInfo}

	// Resolve needs a git clone, so loadConfig fails here — but [logging] has
	// been applied by then, which is the point.
	_, _ = loadConfig(g)

	g.logger.Info("dropped by the file level")
	g.logger.Warn("kept")
	out := strings.TrimSpace(buf.String())
	if strings.Contains(out, "dropped") {
		t.Errorf("logging.level ignored: %q", out)
	}
	if !strings.HasPrefix(out, "{") {
		t.Errorf("logging.format ignored: %q", out)
	}
}
