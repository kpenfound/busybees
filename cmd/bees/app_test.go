package main

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"runtime/debug"
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

// The build handed to the scheduler is the one `bees version` prints, plus
// the untruncated revision behind it: the version is for a person to read and
// the revision is what #297 compares against the repository, so the two must
// not collapse into one. Built from a hand-made *debug.BuildInfo — under
// `go test` the real one describes the test binary, not bees.
func TestSchedulerBuild(t *testing.T) {
	build := func(main string, settings ...debug.BuildSetting) *debug.BuildInfo {
		return &debug.BuildInfo{Main: debug.Module{Version: main}, Settings: settings}
	}
	rev := func(v string) debug.BuildSetting { return debug.BuildSetting{Key: "vcs.revision", Value: v} }
	mod := func(v string) debug.BuildSetting { return debug.BuildSetting{Key: "vcs.modified", Value: v} }
	const sha = "b24a0605c2a1e9f0d3c4b5a6978869d3d1e2f3a4"

	for _, c := range []struct {
		name         string
		override     string
		bi           *debug.BuildInfo
		wantVersion  string
		wantRevision string
	}{
		{"release build", "v0.2.0", build("(devel)", rev(sha), mod("false")), "v0.2.0", sha},
		{"local build of a dirty tree", "dev", build("(devel)", rev(sha), mod("true")), "dev (b24a0605c2a1 modified)", sha},
		{"local build of a clean tree", "dev", build("(devel)", rev(sha), mod("false")), "dev (b24a0605c2a1)", sha},
		{"no vcs stamps", "dev", build("(devel)"), "dev", ""},
		{"no build info at all", "dev", nil, "dev", ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			gotVersion, gotRevision := schedulerBuild(c.override, c.bi)
			if gotVersion != c.wantVersion {
				t.Errorf("version: got %q want %q", gotVersion, c.wantVersion)
			}
			if gotRevision != c.wantRevision {
				t.Errorf("revision: got %q want %q", gotRevision, c.wantRevision)
			}
		})
	}
}
