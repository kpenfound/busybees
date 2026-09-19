// Package review holds `bees review`, the pull request review tool: a
// command that reviews any GitHub pull request, not only the ones a factory
// manages.
//
// This file is its configuration surface, which is two files and neither of
// them is bees.toml:
//
//	~/.config/bees/config.toml   the person's own settings (this file):
//	                             provider and model, the angles of each size
//	                             and the model or profile of each step, where reviewer
//	                             notes and review artifacts live, the default
//	                             output mode, GitHub authentication
//	context.toml                 the project's settings (project.go): which
//	                             angles run, extra style sources, category
//	                             overrides, which context sources are gathered
//
// Both are optional: a missing file loads as defaults. A file that is
// present is held to the same standard as bees.toml, an unknown key and an
// invalid value are load errors naming the key.
//
// ref.go resolves the pull request, and context.go gathers the enabled sources
// into a bundle. checkout.go clones the head for the diff and angle sessions.
// run.go passes the acquired context and diff to core/review: distillation,
// size-based angle execution, deterministic judging and noise filtering.
// agent.go supplies read-only CLI sessions; angles.go supplies configured
// models, working directories and provider-specific resumption. notes.go owns
// reviewer notes storage and consolidation, and judge.go supplies the existing
// busybees text comparator to core's merge and filter stages.
//
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
	"maps"
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

// SupportedProviders lists the providers CLIAgent.command() (agent.go)
// implements. Config.Validate() checks Provider against this list rather
// than against config.Agents, the factory's own shared agent-name enum: the
// two happen to agree today, but config.Agents can grow a value (a new
// factory session backend) before internal/review implements it, and
// validating against it would then accept a provider Run cannot start.
// Widening this list is a deliberate step taken together with adding that
// provider's case to command().
var SupportedProviders = []string{config.AgentClaude, config.AgentCodex}

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
	// SupportedProviders, and Model the model they use.
	Provider string `toml:"provider"`
	Model    string `toml:"model"`
	// Angles replaces, for each size it names, the angles a change of that
	// size is reviewed from (sizeAngles in core/review/angles.go): angles.xs =
	// ["quick_general"]. A size it leaves out keeps the built-in list, and
	// context.toml's per-project switches still turn an angle off on top of
	// either.
	Angles map[string][]string `toml:"angles"`
	// BriefModel is the model the distiller session uses, and AngleModels
	// the model of each angle session it names; a step with none uses Model.
	// The provider is never overridden per step: every session runs as
	// Provider.
	BriefModel  string            `toml:"brief_model"`
	AngleModels map[string]string `toml:"angle_models"`
	// JudgeModel is accepted and validated so the file has the shape of
	// bees.toml's roles.reviewer, and has no effect here: the judge of
	// `bees review` is deterministic code (core/review/judge.go), not a session.
	JudgeModel string `toml:"judge_model"`
	// Profiles are named session settings, bees.toml's [profiles.*] with the
	// same keys and the same checks (config.ValidateProfiles): agent, model,
	// fallback, effort and sandbox. BriefProfile selects the distiller's and
	// AngleProfiles each named angle's, which then runs as that profile's
	// agent, model and effort, with its fallback chain behind it; a step with
	// a profile ignores its flat keys (provider, model, brief_model,
	// angle_models). Sandbox is checked and not used: every review session
	// keeps its read-only floor. JudgeProfile, like JudgeModel, is accepted
	// so a reviewer section of bees.toml copies across, and has no effect.
	Profiles      map[string]config.AgentProfile `toml:"profiles"`
	BriefProfile  string                         `toml:"brief_profile"`
	JudgeProfile  string                         `toml:"judge_profile"`
	AngleProfiles map[string]string              `toml:"angle_profiles"`
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
	if !slices.Contains(SupportedProviders, c.Provider) {
		errs = append(errs, fmt.Sprintf("provider %q must be one of %s", c.Provider, strings.Join(SupportedProviders, ", ")))
	}
	if !slices.Contains(OutputModes, c.Output) {
		errs = append(errs, fmt.Sprintf("output %q must be one of %s", c.Output, strings.Join(OutputModes, ", ")))
	}
	for _, size := range slices.Sorted(maps.Keys(c.Angles)) {
		if !slices.Contains(Sizes, size) {
			errs = append(errs, fmt.Sprintf("angles.%s: %q is not a size, which is one of %s", size, size, strings.Join(Sizes, ", ")))
			continue
		}
		if len(c.Angles[size]) == 0 {
			errs = append(errs, fmt.Sprintf("angles.%s must name at least one angle: remove the key to keep the built-in list", size))
		}
		for _, angle := range c.Angles[size] {
			if !slices.Contains(BuiltinAngles, angle) {
				errs = append(errs, fmt.Sprintf("angles.%s: %q is not an angle, which is one of %s", size, angle, strings.Join(BuiltinAngles, ", ")))
			}
		}
	}
	for _, angle := range slices.Sorted(maps.Keys(c.AngleModels)) {
		switch {
		case !slices.Contains(BuiltinAngles, angle):
			errs = append(errs, fmt.Sprintf("angle_models.%s: %q is not an angle, which is one of %s", angle, angle, strings.Join(BuiltinAngles, ", ")))
		case strings.TrimSpace(c.AngleModels[angle]) == "":
			errs = append(errs, fmt.Sprintf("angle_models.%s must name a model: remove the key to use model", angle))
		}
	}
	errs = append(errs, config.ValidateProfiles(c.Profiles)...)
	if c.BriefProfile != "" {
		errs = append(errs, c.checkProfile("brief_profile", c.BriefProfile, true)...)
	}
	if c.JudgeProfile != "" {
		errs = append(errs, c.checkProfile("judge_profile", c.JudgeProfile, false)...)
	}
	for _, angle := range slices.Sorted(maps.Keys(c.AngleProfiles)) {
		if !slices.Contains(BuiltinAngles, angle) {
			errs = append(errs, fmt.Sprintf("angle_profiles.%s: %q is not an angle, which is one of %s", angle, angle, strings.Join(BuiltinAngles, ", ")))
			continue
		}
		errs = append(errs, c.checkProfile("angle_profiles."+angle, c.AngleProfiles[angle], true)...)
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

// checkProfile checks the profile key selects: that Profiles has it, and
// for a session's profile, that it and every profile of its fallback chain
// run an agent a review session can run as, one of SupportedProviders.
func (c *Config) checkProfile(key, name string, session bool) []string {
	if _, ok := c.Profiles[name]; !ok {
		return []string{fmt.Sprintf("%s: unknown profile %q (declare it under [profiles.%s])", key, name, name)}
	}
	if !session {
		return nil
	}
	var errs []string
	at := name
	for i, p := range config.ProfileChain(c.Profiles, name) {
		if i > 0 {
			at = c.Profiles[at].Fallback
		}
		if slices.Contains(SupportedProviders, p.Agent) {
			continue
		}
		which := fmt.Sprintf("profile %q", at)
		if i > 0 {
			which = fmt.Sprintf("its fallback profile %q", at)
		}
		errs = append(errs, fmt.Sprintf("%s: review sessions run as one of %s, and %s runs %q", key, strings.Join(SupportedProviders, ", "), which, p.Agent))
	}
	return errs
}

// profileAgent is the agent a session on the profile called name runs as:
// the profile's agent, model and effort, and behind it, as its Fallback,
// the agent of each profile its fallback chain runs through. The profile's
// sandbox is not used: a review session is read-only whatever it says.
func (c *Config) profileAgent(name string) *CLIAgent {
	var head, at *CLIAgent
	for _, p := range config.ProfileChain(c.Profiles, name) {
		a := &CLIAgent{Provider: p.Agent, Model: p.Model, Effort: p.Effort}
		if head == nil {
			head = a
		} else {
			at.Fallback = a
		}
		at = a
	}
	return head
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
