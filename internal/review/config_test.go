package review

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestConfigDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(filepath.Join(dir, ConfigFile))
	if err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}
	if cfg.Loaded {
		t.Fatal("Loaded is true for a file that is not there")
	}
	if cfg.Provider != "claude" || cfg.Model != "opus" || cfg.Output != OutputAsk {
		t.Fatalf("defaults: %+v", cfg)
	}
	if got, want := cfg.ResolvedNotesPath(), filepath.Join(dir, "reviewer-notes.md"); got != want {
		t.Fatalf("notes path %q, want %q", got, want)
	}
	if got, want := cfg.ResolvedStoragePath(), filepath.Join(dir, "reviews"); got != want {
		t.Fatalf("storage path %q, want %q", got, want)
	}
	if !cfg.TUI.ColorEnabled() || cfg.TUI.DiffContext != DefaultDiffContext {
		t.Fatalf("tui defaults: %+v", cfg.TUI)
	}
	if cfg.GitHub.ResolvedToken() != "" || cfg.GitHub.RedactedToken() != "" {
		t.Fatalf("no token configured, got %q", cfg.GitHub.RedactedToken())
	}
}

// An empty file is the same as no file, and every key left out of a file
// that does set some keeps its default.
func TestConfigPartialFileKeepsDefaults(t *testing.T) {
	dir := t.TempDir()
	cfg, err := LoadConfig(writeFile(t, dir, ConfigFile, "output = \"report\"\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Loaded {
		t.Fatal("Loaded is false for a file that is there")
	}
	if cfg.Output != OutputReport {
		t.Fatalf("output %q", cfg.Output)
	}
	if cfg.Provider != DefaultProvider || cfg.Model != DefaultModel || cfg.NotesPath != DefaultNotesPath || cfg.StoragePath != DefaultStoragePath {
		t.Fatalf("the rest is not defaulted: %+v", cfg)
	}
}

func TestConfigOverrides(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REVIEW_TOKEN", "ghp_secret")
	cfg, err := LoadConfig(writeFile(t, dir, ConfigFile, `
provider = "codex"
model = "gpt-5"
notes_path = "notes/reviewer-notes.md"
storage_path = "/var/lib/bees/reviews"
output = "comment"

[github]
token = "$REVIEW_TOKEN"

[tui]
color = false
diff_context = 8
`))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Provider != "codex" || cfg.Model != "gpt-5" || cfg.Output != OutputComment {
		t.Fatalf("overrides: %+v", cfg)
	}
	if got, want := cfg.ResolvedNotesPath(), filepath.Join(dir, "notes", "reviewer-notes.md"); got != want {
		t.Fatalf("relative notes path %q, want %q", got, want)
	}
	if got := cfg.ResolvedStoragePath(); got != "/var/lib/bees/reviews" {
		t.Fatalf("absolute storage path %q", got)
	}
	if cfg.TUI.ColorEnabled() || cfg.TUI.DiffContext != 8 {
		t.Fatalf("tui: %+v", cfg.TUI)
	}
	// color = true is not the same as an absent color: both are colour on,
	// and the pointer says which of them the file wrote.
	on, err := ParseConfig("[tui]\ncolor = true\n", filepath.Join(dir, ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if !on.TUI.ColorEnabled() || on.TUI.Color == nil || !*on.TUI.Color {
		t.Fatalf("color = true: %+v", on.TUI)
	}
	if got := cfg.GitHub.ResolvedToken(); got != "ghp_secret" {
		t.Fatalf("token %q, want the expanded variable", got)
	}
	// Whitespace around the reference is the shape a token written on its
	// own line takes; it is not part of the credential.
	padded := GitHub{Token: "  $REVIEW_TOKEN  "}
	if got := padded.ResolvedToken(); got != "ghp_secret" {
		t.Fatalf("padded token %q, want the expanded variable alone", got)
	}
	if got := (GitHub{Token: "ghp_literal"}).RedactedToken(); got != "(set)" {
		t.Fatalf("redacted literal token %q: the value itself is never printed", got)
	}
	if got := cfg.GitHub.RedactedToken(); got != "$REVIEW_TOKEN" {
		t.Fatalf("redacted token %q: a $VAR reference is printed as written", got)
	}
}

// diff_context = 0 is a legal value that means the default, the way every
// numeric bees.toml key with a default does.
func TestConfigDiffContextZeroIsTheDefault(t *testing.T) {
	cfg, err := ParseConfig("[tui]\ndiff_context = 0\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TUI.DiffContext != DefaultDiffContext {
		t.Fatalf("diff_context %d, want the default %d", cfg.TUI.DiffContext, DefaultDiffContext)
	}
}

func TestConfigInvalid(t *testing.T) {
	// A literal token spelled with no $ still has to be there, so the empty
	// case is only reachable through a variable that is not set.
	t.Setenv("REVIEW_UNSET_TOKEN", "")
	for _, tc := range []struct {
		name string
		toml string
		want []string
	}{
		{"unknown key", "provder = \"claude\"\n", []string{"unknown keys", "provder"}},
		{"unknown key in a table", "[tui]\ncolour = true\n", []string{"unknown keys", "tui.colour"}},
		{"wrong type", "[tui]\ndiff_context = \"three\"\n", []string{"diff_context"}},
		{"provider", "provider = \"gemini\"\n", []string{"provider \"gemini\" must be one of claude, codex"}},
		{"output", "output = \"merge\"\n", []string{"output \"merge\" must be one of ask, approve, comment, reject, report, discard"}},
		{"diff_context", "[tui]\ndiff_context = -1\n", []string{"tui.diff_context must be >= 0"}},
		{"token variable", "[github]\ntoken = \"$REVIEW_UNSET_TOKEN\"\n", []string{"github.token reads $REVIEW_UNSET_TOKEN, which is not set"}},
		{"token expands to nothing", "[github]\ntoken = \"$REVIEW_UNSET_TOKEN$REVIEW_UNSET_TOKEN\"\n", []string{"expands to nothing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), ConfigFile)
			_, err := LoadConfig(writeFile(t, filepath.Dir(path), ConfigFile, tc.toml))
			if err == nil {
				t.Fatal("loaded without an error")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not name %q", err, want)
				}
			}
			if !strings.Contains(err.Error(), path) {
				t.Fatalf("error %q does not name the file", err)
			}
		})
	}
}

// Every problem in a file is reported, not only the first: fixing one and
// loading again to find the next is the loop this avoids.
func TestConfigReportsEveryProblem(t *testing.T) {
	_, err := ParseConfig("provider = \"gemini\"\noutput = \"merge\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err == nil {
		t.Fatal("loaded without an error")
	}
	if !strings.Contains(err.Error(), "provider") || !strings.Contains(err.Error(), "output") {
		t.Fatalf("error names one problem only: %q", err)
	}
}

func TestDefaultConfigPath(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/xdg")
	if got, want := DefaultConfigPath(), filepath.Join("/xdg", "bees", ConfigFile); got != want {
		t.Fatalf("XDG_CONFIG_HOME: %q, want %q", got, want)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	// An unset XDG_CONFIG_HOME falls back to ~/.config, and so does a
	// relative one: the spec says a relative value is to be ignored, and
	// resolving it against the working directory would put a person's
	// configuration wherever they happened to run bees from.
	for _, xdg := range []string{"", "relative/config"} {
		t.Setenv("XDG_CONFIG_HOME", xdg)
		if got, want := DefaultConfigPath(), filepath.Join(home, ".config", "bees", ConfigFile); got != want {
			t.Fatalf("XDG_CONFIG_HOME %q: default path %q, want %q", xdg, got, want)
		}
	}
	// LoadConfig with no path reads that file, and the home directory of a
	// test has none, so it is the defaults.
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg, err := LoadConfig("")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Loaded || cfg.Path != DefaultConfigPath() {
		t.Fatalf("LoadConfig(\"\") read %q (loaded %v), want the default path", cfg.Path, cfg.Loaded)
	}
}

func TestConfigPathExpandsHome(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("no home directory on this machine")
	}
	cfg, err := ParseConfig("notes_path = \"~/notes.md\"\nstorage_path = \"~\"\n", filepath.Join(t.TempDir(), ConfigFile))
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cfg.ResolvedNotesPath(), filepath.Join(home, "notes.md"); got != want {
		t.Fatalf("notes path %q, want %q", got, want)
	}
	if got := cfg.ResolvedStoragePath(); got != home {
		t.Fatalf("storage path %q, want %q", got, home)
	}
}
