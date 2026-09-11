// Package review holds `bees review`, the pull request review tool: a
// command that reviews any GitHub pull request, not only the ones a factory
// manages.
//
// This file is its configuration surface, which is two files and neither of
// them is bees.toml:
//
//	~/.config/bees/config.toml   the person's own settings (this file):
//	                             provider and model, where reviewer notes and
//	                             review artifacts live, the default output
//	                             mode, GitHub authentication
//	context.toml                 the project's settings (project.go): which
//	                             angles run, extra style sources, category
//	                             overrides, which context sources are gathered
//
// Both are optional: a missing file loads as defaults. A file that is
// present is held to the same standard as bees.toml, an unknown key and an
// invalid value are load errors naming the key.
//
// The review itself starts in ref.go, which turns what a person typed into
// the pull request under review, and context.go, which gathers the sources
// context.toml enables into the bundle a review reads. distill.go runs the
// first session over that bundle and brief.go is what it produces, the
// starting context of every session after it; checkout.go clones the pull
// request's head, in a container, for the sessions to read; angles.go fans
// those sessions out, one per angle the project enables, in that checkout,
// and keeps enough of each to reopen it; agent.go runs every one of them,
// read-only. findings.go is what an
// angle answers with and judge.go merges every angle's into the one list a
// review triages; notes.go is the reviewer notes, the file outside every
// review that holds what this reviewer has dismissed before, and noise.go
// keeps what they dismissed out of the list before triage reads it.
// triage.go is the triage queue over that list, the four actions it takes on
// a finding and what each writes back; console.go drives it from a
// terminal one line at a time (the screen that drives it on single keys is
// internal/reviewtui), and factory.go from the answers of an agent session
// instead, factory mode. run.go strings all of that together for one
// pull request, from the gather to the judged list, and output.go is the
// end of the review: what triage selected posted as one review, printed as
// a report, or discarded. diffview.go reads the pull request's diff, with
// the same walk output.go anchors comments by, into the files, hunks and
// lines a screen shows beside a finding, the finding's lines marked.
// artifact.go is the directory the whole review is kept in.
package review

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/BurntSushi/toml"

	"github.com/kpenfound/busybees/internal/config"
)

// ConfigFile is the name of the global configuration file, and ProjectFile
// the name of the per-project one.
const (
	ConfigFile  = "config.toml"
	ProjectFile = "context.toml"
)

// Output modes: what happens to the findings triage selected, a person's
// or an agent's. One of them is the end of every review.
const (
	// OutputAsk asks which of the other five to take: the person at the
	// console, or in factory mode the agent that triaged (factory.go).
	OutputAsk = "ask"
	// OutputApprove submits the selected findings as review comments and
	// approves the pull request.
	OutputApprove = "approve"
	// OutputComment submits a comment-only review.
	OutputComment = "comment"
	// OutputReject submits a request-changes review.
	OutputReject = "reject"
	// OutputReport prints the selected findings as a markdown report on
	// stdout and submits nothing.
	OutputReport = "report"
	// OutputDiscard exits without submitting anything.
	OutputDiscard = "discard"
)

// OutputModes lists the accepted output values, in the order they are
// printed.
var OutputModes = []string{OutputAsk, OutputApprove, OutputComment, OutputReject, OutputReport, OutputDiscard}

// Defaults used for every key the global file leaves out.
const (
	// DefaultProvider and DefaultModel are the agent the review sessions
	// run as. They are the values a factory role gets from bees.toml with
	// nothing configured, so one person's reviews and their factory read
	// the same way.
	DefaultProvider = config.AgentClaude
	DefaultModel    = config.DefaultModel
	// DefaultNotesPath and DefaultStoragePath are relative, so they land
	// next to the configuration file they were left out of.
	DefaultNotesPath   = "reviewer-notes.md"
	DefaultStoragePath = "reviews"
	DefaultOutput      = OutputAsk
)

// Config is the global configuration, `~/.config/bees/config.toml`.
type Config struct {
	// Path is the file this configuration was read from, absolute, and set
	// even when that file does not exist (Loaded says which): a relative
	// notes_path or storage_path resolves against its directory.
	Path string `toml:"-"`
	// Loaded reports whether Path existed. It is false for a configuration
	// that is every default.
	Loaded bool `toml:"-"`

	// Provider is the agent the review sessions run as, one of
	// config.Agents, and Model the model they use.
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	// NotesPath is where the reviewer notes a dismissal appends to live,
	// StoragePath the directory review artifact directories are created in.
	// Both take `~`, an absolute path, or a path relative to Path's
	// directory; read them through ResolvedNotesPath and
	// ResolvedStoragePath, never directly.
	NotesPath   string `toml:"notes_path"`
	StoragePath string `toml:"storage_path"`
	// Output is the end state a review takes when the command line asks for
	// none, one of OutputModes.
	Output string `toml:"output"`

	GitHub GitHub `toml:"github"`
}

// GitHub is where `bees review` gets its GitHub credentials from. An empty
// table means the machine's own `gh` authentication, which is what most
// people want; a token is for a machine that has none, or an account other
// than the one `gh auth status` reports.
type GitHub struct {
	// Token authenticates the gh calls a review makes. A "$VAR" or
	// "${VAR}" reference is expanded from the environment, so the secret
	// need not be written into the file. Read it through ResolvedToken,
	// never directly.
	Token string `toml:"token"`
}

// ResolvedToken is the token with $VAR references expanded, and "" when no
// token is configured. Loading has already rejected a reference that expands
// to nothing, so an empty result means "use the machine's own gh auth".
func (g GitHub) ResolvedToken() string {
	if g.Token == "" {
		return ""
	}
	return strings.TrimSpace(os.ExpandEnv(g.Token))
}

// RedactedToken is what a token may be printed as: a $VAR reference as
// written, which carries no secret, and anything else as a placeholder.
func (g GitHub) RedactedToken() string {
	switch {
	case g.Token == "":
		return ""
	case config.TokenVar(g.Token) != "":
		return g.Token
	}
	return "(set)"
}

// DefaultConfigDir is the directory `bees review` keeps a person's own
// configuration, notes and review artifacts in: $XDG_CONFIG_HOME/bees, or
// ~/.config/bees when that variable is unset.
func DefaultConfigDir() string {
	if dir := os.Getenv("XDG_CONFIG_HOME"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "bees")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".config", "bees")
	}
	return filepath.Join(home, ".config", "bees")
}

// DefaultConfigPath is the global configuration file LoadConfig reads when
// it is given no path.
func DefaultConfigPath() string { return filepath.Join(DefaultConfigDir(), ConfigFile) }

// LoadConfig reads the global configuration at path, or at
// DefaultConfigPath when path is empty. A file that is not there is not an
// error: it loads as the defaults, with Loaded false.
func LoadConfig(path string) (*Config, error) {
	if path == "" {
		path = DefaultConfigPath()
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		cfg := &Config{Path: abs}
		cfg.applyDefaults()
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	return ParseConfig(string(data), abs)
}

// ParseConfig validates the text of a global configuration file as if it had
// been read from path, which is used for the Config's location and in error
// messages but is not itself read.
func ParseConfig(text, path string) (*Config, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	cfg := Config{Path: abs, Loaded: true}
	md, err := toml.Decode(text, &cfg)
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", abs, err)
	}
	if err := undecoded(md, abs); err != nil {
		return nil, err
	}
	cfg.applyDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (c *Config) applyDefaults() {
	if c.Provider == "" {
		c.Provider = DefaultProvider
	}
	if c.Model == "" {
		c.Model = DefaultModel
	}
	if c.NotesPath == "" {
		c.NotesPath = DefaultNotesPath
	}
	if c.StoragePath == "" {
		c.StoragePath = DefaultStoragePath
	}
	if c.Output == "" {
		c.Output = DefaultOutput
	}
}

// Validate checks the global configuration, reporting every problem it finds
// rather than the first.
func (c *Config) Validate() error {
	var errs []string
	if !slices.Contains(config.Agents, c.Provider) {
		errs = append(errs, fmt.Sprintf("provider %q must be one of %s", c.Provider, strings.Join(config.Agents, ", ")))
	}
	if !slices.Contains(OutputModes, c.Output) {
		errs = append(errs, fmt.Sprintf("output %q must be one of %s", c.Output, strings.Join(OutputModes, ", ")))
	}
	if c.GitHub.Token != "" && c.GitHub.ResolvedToken() == "" {
		where := fmt.Sprintf("github.token %q expands to nothing", c.GitHub.Token)
		if v := config.TokenVar(c.GitHub.Token); v != "" {
			where = fmt.Sprintf("github.token reads $%s, which is not set", v)
		}
		errs = append(errs, where+": set it in the environment, or remove github.token to use your own gh authentication")
	}
	return invalid(c.Path, errs)
}

// Dir is the directory the configuration file lives in, which relative paths
// in it resolve against.
func (c *Config) Dir() string { return filepath.Dir(c.Path) }

// ResolvedNotesPath is the absolute path of the reviewer notes file.
func (c *Config) ResolvedNotesPath() string { return c.resolvePath(c.NotesPath) }

// ResolvedStoragePath is the absolute path of the directory review artifact
// directories are created in.
func (c *Config) ResolvedStoragePath() string { return c.resolvePath(c.StoragePath) }

func (c *Config) resolvePath(p string) string {
	p = expandHome(p)
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(c.Dir(), p)
}

// expandHome replaces a leading ~ with the user's home directory. A path
// that does not start with one, and a machine with no home directory, are
// left as they are: resolvePath then treats the path as relative, which
// keeps a surprising path inside the configuration directory instead of
// somewhere unrelated.
func expandHome(p string) string {
	if p != "~" && !strings.HasPrefix(p, "~"+string(filepath.Separator)) {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}
	return filepath.Join(home, strings.TrimPrefix(p[1:], string(filepath.Separator)))
}

// undecoded turns the keys a file has that the schema does not into one
// error naming every one of them.
func undecoded(md toml.MetaData, path string) error {
	keys := md.Undecoded()
	if len(keys) == 0 {
		return nil
	}
	names := make([]string, 0, len(keys))
	for _, k := range keys {
		names = append(names, k.String())
	}
	return fmt.Errorf("%s: unknown keys: %s", path, strings.Join(names, ", "))
}

// invalid joins a file's validation errors into the one error a person
// reads, and is nil when there are none.
func invalid(path string, errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return fmt.Errorf("invalid %s:\n  - %s", path, strings.Join(errs, "\n  - "))
}
