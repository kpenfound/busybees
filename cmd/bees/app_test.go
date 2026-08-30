package main

import (
	"bytes"
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
