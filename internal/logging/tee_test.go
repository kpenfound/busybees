package logging

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Two tees of one logger share its console and each keeps a file of its own:
// a record logged for one project never lands in the other's file.
func TestTeeKeepsAFilePerLogger(t *testing.T) {
	var console bytes.Buffer
	lg := New(Options{Format: FormatText, Level: slog.LevelInfo, Console: &console})
	t.Cleanup(func() { _ = lg.Close() })
	dir := t.TempDir()
	fooPath, barPath := filepath.Join(dir, "foo.log"), filepath.Join(dir, "bar.log")
	foo, fooFile, err := lg.Tee(fooPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fooFile.Close() })
	bar, barFile, err := lg.Tee(barPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = barFile.Close() })

	foo.With("project", "foo").Info("polled foo")
	foo.Debug("foo detail")
	bar.Info("polled bar")

	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	fooLog, barLog := read(fooPath), read(barPath)
	if !strings.Contains(fooLog, "polled foo") || !strings.Contains(fooLog, `"project":"foo"`) || !strings.Contains(fooLog, "foo detail") {
		t.Errorf("foo's file: %q", fooLog)
	}
	if strings.Contains(fooLog, "polled bar") || strings.Contains(barLog, "foo") {
		t.Errorf("a record reached the other project's file: foo %q, bar %q", fooLog, barLog)
	}
	if !strings.Contains(console.String(), "polled foo") || !strings.Contains(console.String(), "polled bar") {
		t.Errorf("console: %q", console.String())
	}
	if strings.Contains(console.String(), "foo detail") {
		t.Errorf("the console printed a debug record below its level: %q", console.String())
	}
}

func TestTeeFailsWhenTheFileCannotBeOpened(t *testing.T) {
	lg := New(Options{Console: &bytes.Buffer{}})
	file := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, err := lg.Tee(filepath.Join(file, "bees.log")); err == nil {
		t.Fatal("a log path under a file opened")
	}
}
