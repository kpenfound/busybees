package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestConsoleText(t *testing.T) {
	var buf bytes.Buffer
	lg := New(Options{Format: FormatText, Console: &buf})
	lg.Info("polled github", "issues", 3)
	lg.Info("✓ reviewer PR #31 approved", SummaryKey, true, "pr", 31)

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got %q", buf.String())
	}
	if !strings.Contains(lines[0], "msg=\"polled github\"") || !strings.Contains(lines[0], "issues=3") {
		t.Errorf("normal record: %q", lines[0])
	}
	// A summary is printed as its message alone, with no slog framing.
	if lines[1] != "✓ reviewer PR #31 approved" {
		t.Errorf("summary record: %q", lines[1])
	}
}

func TestConsoleJSON(t *testing.T) {
	var buf bytes.Buffer
	lg := New(Options{Format: FormatJSON, Console: &buf})
	lg.Info("polled github", "issues", 3)
	lg.Info("✓ reviewer PR #31 approved", SummaryKey, true, "pr", 31, "outcome", "approved")

	recs := decode(t, buf.String())
	if len(recs) != 2 {
		t.Fatalf("want 2 records, got %q", buf.String())
	}
	if recs[0]["msg"] != "polled github" || recs[0][SummaryKey] != nil {
		t.Errorf("normal record: %v", recs[0])
	}
	if recs[1]["msg"] != "✓ reviewer PR #31 approved" || recs[1][SummaryKey] != true {
		t.Errorf("summary record: %v", recs[1])
	}
	if recs[1]["outcome"] != "approved" || recs[1]["pr"] != float64(31) {
		t.Errorf("summary attrs: %v", recs[1])
	}
}

func TestQuiet(t *testing.T) {
	var buf bytes.Buffer
	lg := New(Options{Format: FormatText, Quiet: true, Console: &buf})
	lg.Info("polled github")
	lg.Info("✓ QA engineer idle", SummaryKey, true)
	lg.Warn("workspace cleanup failed")

	out := buf.String()
	if strings.Contains(out, "polled github") {
		t.Errorf("quiet kept an info record: %q", out)
	}
	if !strings.Contains(out, "✓ QA engineer idle") {
		t.Errorf("quiet dropped the summary: %q", out)
	}
	if !strings.Contains(out, "workspace cleanup failed") {
		t.Errorf("quiet dropped a warning: %q", out)
	}
}

func TestFileGetsDebugRegardlessOfConsole(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	lg := New(Options{Format: FormatText, Level: slog.LevelWarn, Quiet: true, Console: &buf})
	path := filepath.Join(dir, "bees.log")
	if err := lg.AttachFile(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	lg.Debug("dispatching", "issue", 7)
	lg.Info("polled github")

	if buf.Len() != 0 {
		t.Errorf("console should be silent: %q", buf.String())
	}
	recs := decode(t, read(t, path))
	if len(recs) != 2 || recs[0]["msg"] != "dispatching" || recs[0]["level"] != "DEBUG" {
		t.Fatalf("file records: %v", recs)
	}
	if recs[0]["issue"] != float64(7) {
		t.Errorf("file lost attrs: %v", recs[0])
	}
}

func TestAttachFileIdempotent(t *testing.T) {
	dir := t.TempDir()
	lg := New(Options{Console: &bytes.Buffer{}})
	path := filepath.Join(dir, "bees.log")
	for range 3 {
		if err := lg.AttachFile(path); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() { _ = lg.Close() })
	lg.Info("once")
	if recs := decode(t, read(t, path)); len(recs) != 1 {
		t.Fatalf("record written %d times", len(recs))
	}
}

// With() attributes must reach both destinations, including a file attached
// after the derived logger was made — the scheduler derives loggers early.
func TestWithAttrsReachBothHandlers(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	lg := New(Options{Format: FormatJSON, Console: &buf})
	worker := lg.With("worker", "dev-1").With("issue", 12)

	path := filepath.Join(dir, "bees.log")
	if err := lg.AttachFile(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	worker.Info("developer session")

	for name, out := range map[string]string{"console": buf.String(), "file": read(t, path)} {
		recs := decode(t, out)
		if len(recs) != 1 {
			t.Fatalf("%s: want 1 record, got %q", name, out)
		}
		if recs[0]["worker"] != "dev-1" || recs[0]["issue"] != float64(12) {
			t.Errorf("%s lost With attrs: %v", name, recs[0])
		}
	}
}

// SetConsole replaces the console handler for loggers already derived with
// With(), and leaves the file handler alone.
func TestSetConsoleReachesDerivedLoggers(t *testing.T) {
	var buf bytes.Buffer
	dir := t.TempDir()
	lg := New(Options{Format: FormatText, Console: &buf})
	worker := lg.With("k", "v")

	path := filepath.Join(dir, "bees.log")
	if err := lg.AttachFile(path); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })

	lg.SetConsole(Options{Format: FormatJSON})
	worker.Info("after")

	recs := decode(t, buf.String())
	if len(recs) != 1 {
		t.Fatalf("console is not JSON: %q", buf.String())
	}
	if recs[0]["k"] != "v" || recs[0]["msg"] != "after" {
		t.Errorf("console record: %v", recs[0])
	}
	if recs := decode(t, read(t, path)); len(recs) != 1 || recs[0]["k"] != "v" {
		t.Errorf("file lost the record: %q", read(t, path))
	}
}

// SetConsole applies the new level, and writes to the writer the logger was
// created with when it is given none.
func TestSetConsoleKeepsTheWriterAndAppliesTheLevel(t *testing.T) {
	var buf bytes.Buffer
	lg := New(Options{Format: FormatText, Level: slog.LevelInfo, Console: &buf})
	lg.SetConsole(Options{Format: FormatText, Level: slog.LevelWarn})

	lg.Info("dropped")
	lg.Warn("kept")
	out := buf.String()
	if strings.Contains(out, "dropped") || !strings.Contains(out, "kept") {
		t.Fatalf("console: %q", out)
	}
}

func TestWithGroup(t *testing.T) {
	var buf bytes.Buffer
	lg := New(Options{Format: FormatJSON, Console: &buf})
	lg.WithGroup("session").With("role", "qa").Info("started")
	recs := decode(t, buf.String())
	group, ok := recs[0]["session"].(map[string]any)
	if !ok || group["role"] != "qa" {
		t.Fatalf("group not forwarded: %v", recs[0])
	}
}

func TestRotation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bees.log")
	lg := New(Options{Console: &bytes.Buffer{}})
	if err := lg.attachFile(path, 200); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lg.Close() })
	for i := range 40 {
		lg.Info("a record long enough to fill the file quickly", "i", i)
	}
	for _, name := range []string{"bees.log", "bees.log.1", "bees.log.2"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err != nil {
			t.Errorf("missing %s: %v", name, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "bees.log.3")); !os.IsNotExist(err) {
		t.Errorf("bees.log.3 should never exist: %v", err)
	}
	// The newest generation holds the last records written.
	if !strings.Contains(read(t, path), `"i":39`) {
		t.Errorf("current file does not hold the newest record")
	}
}

// A single record larger than MaxBytes must still be written, once.
func TestRotationOversizedRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bees.log")
	w, err := newRotatingWriter(path, 10)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = w.Close() }()
	if _, err := w.Write([]byte(strings.Repeat("x", 50) + "\n")); err != nil {
		t.Fatal(err)
	}
	if got := len(read(t, path)); got != 51 {
		t.Fatalf("oversized record not written whole: %d bytes", got)
	}
}

func TestParseFormat(t *testing.T) {
	for _, in := range []string{"", "text", "json"} {
		if _, err := ParseFormat(in); err != nil {
			t.Errorf("ParseFormat(%q): %v", in, err)
		}
	}
	_, err := ParseFormat("bogus")
	if err == nil || !strings.Contains(err.Error(), "text, json") {
		t.Errorf("error should list the valid values: %v", err)
	}
}

func TestParseLevel(t *testing.T) {
	want := map[string]slog.Level{
		"":      slog.LevelInfo,
		"debug": slog.LevelDebug,
		"INFO":  slog.LevelInfo,
		"warn":  slog.LevelWarn,
		"error": slog.LevelError,
	}
	for in, exp := range want {
		got, err := ParseLevel(in)
		if err != nil || got != exp {
			t.Errorf("ParseLevel(%q) = %v, %v", in, got, err)
		}
	}
	if _, err := ParseLevel("loud"); err == nil || !strings.Contains(err.Error(), "debug, info, warn, error") {
		t.Errorf("ParseLevel(loud): %v", err)
	}
}

func decode(t *testing.T, out string) []map[string]any {
	t.Helper()
	var recs []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("bad JSON line %q: %v", line, err)
		}
		recs = append(recs, m)
	}
	return recs
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
