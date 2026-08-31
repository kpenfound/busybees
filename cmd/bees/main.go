// Command bees is the busybees software factory: a lightweight orchestrator
// that runs a staff of Claude Code sessions (product manager, project
// manager, developers, reviewers, QA) against one GitHub repository.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/versions"
	"github.com/spf13/cobra"
)

// version is the release version stamped in by
// `-ldflags "-X main.version=..."`. Without it, versions.Bees falls back to
// the module version or VCS revision Go records in the binary.
//
// .github/workflows/release.yml stamps it with the tag it was triggered by, so
// renaming this variable means renaming it there too; release_test.go fails
// when the two drift.
var version = "dev"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := newRoot().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

type globalFlags struct {
	config    string
	verbose   bool
	quiet     bool
	logFormat string
	logLevel  string
	// logger is built by PersistentPreRunE; commands that run sessions
	// attach the state directory's log file to it.
	logger *logging.Logger
	// console records what the flags and the environment resolved to, so
	// loadConfig can apply bees.toml's [logging] table to what they left
	// alone (see mergeLogging).
	console consoleFlags
}

// consoleFlags is the console logging asked for on the command line or in the
// environment, with whether each dimension was an explicit choice rather than
// a flag default.
type consoleFlags struct {
	format         string
	formatExplicit bool
	level          slog.Level
	levelExplicit  bool
	quiet          bool
}

func newRoot() *cobra.Command {
	_, root := newRootWithFlags()
	return root
}

// newRootWithFlags builds the root command together with the global flags its
// PersistentPreRunE resolves into, so tests can inspect them.
func newRootWithFlags() (*globalFlags, *cobra.Command) {
	g := &globalFlags{}
	root := &cobra.Command{
		Use:   "bees",
		Short: "busybees: a software factory of Claude Code sessions driven by GitHub",
		Long: `busybees orchestrates a staff of Claude Code sessions — product manager,
project manager, developers, reviewers and QA — that build a project together
through GitHub issues and pull requests. Configure it with bees.toml.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			return g.setupLogging(cmd)
		},
		PersistentPostRun: func(cmd *cobra.Command, args []string) {
			if g.logger != nil {
				_ = g.logger.Close()
			}
		},
	}
	root.PersistentFlags().StringVarP(&g.config, "config", "c", "", "path to bees.toml (default: search upwards from cwd, or $BEES_CONFIG)")
	root.PersistentFlags().BoolVarP(&g.verbose, "verbose", "v", false, "debug logging (same as --log-level debug), plus claude event streaming")
	root.PersistentFlags().BoolVarP(&g.quiet, "quiet", "q", false, "console shows only session summaries, warnings and errors")
	root.PersistentFlags().StringVar(&g.logFormat, "log-format", logging.FormatText, "console log format: text or json ($BEES_LOG_FORMAT)")
	root.PersistentFlags().StringVar(&g.logLevel, "log-level", "info", "console log level: debug, info, warn or error ($BEES_LOG_LEVEL)")

	root.AddCommand(
		newInitCmd(g),
		newRunCmd(g),
		newTickCmd(g),
		newExecCmd(g),
		newStatusCmd(g),
		newCostCmd(g),
		newKillCmd(g),
		newMailCmd(g),
		newNotesCmd(g),
		newIssueCmd(g),
		newDoneCmd(),
		newMCPCmd(g),
		newDoctorCmd(g),
		newConfigCmd(g),
		newSkillsCmd(g),
		newPromptsCmd(g),
		newLabelsCmd(g),
		newVersionCmd(),
	)
	return g, root
}

func newVersionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the bees version",
		Run: func(cmd *cobra.Command, args []string) {
			bi, _ := debug.ReadBuildInfo()
			fmt.Println("bees", versions.Bees(version, bi))
		},
	}
}

// setupLogging turns the global flags into slog.Default(). A flag beats the
// environment variable; -v is --log-level debug and cannot be quiet.
func (g *globalFlags) setupLogging(cmd *cobra.Command) error {
	format, err := logging.ParseFormat(flagOrEnv(cmd, "log-format", g.logFormat, "BEES_LOG_FORMAT"))
	if err != nil {
		return err
	}
	levelName := flagOrEnv(cmd, "log-level", g.logLevel, "BEES_LOG_LEVEL")
	level, err := logging.ParseLevel(levelName)
	if err != nil {
		return err
	}
	if g.verbose {
		level = slog.LevelDebug
	}
	if g.quiet && level <= slog.LevelDebug {
		return errors.New("--quiet cannot be combined with --verbose or --log-level debug")
	}
	g.console = consoleFlags{
		format:         format,
		formatExplicit: explicitFlag(cmd, "log-format", "BEES_LOG_FORMAT"),
		level:          level,
		// -v is a level chosen for this invocation, like --log-level debug.
		levelExplicit: explicitFlag(cmd, "log-level", "BEES_LOG_LEVEL") || g.verbose,
		quiet:         g.quiet,
	}
	g.logger = logging.New(logging.Options{Format: format, Level: level, Quiet: g.quiet, Console: cmd.ErrOrStderr()})
	slog.SetDefault(g.logger.Logger)
	return nil
}

// applyLogging rebuilds the console handler with bees.toml's [logging] table
// applied. loadConfig calls it once the file is read: the console logger has
// to exist before any command runs, so the file can only be honoured
// afterwards. Commands that never read bees.toml keep the flag settings.
func (g *globalFlags) applyLogging(l config.Logging) {
	if g.logger == nil {
		return
	}
	g.logger.SetConsole(mergeLogging(g.console, l))
}

// mergeLogging resolves the console options: a flag beats the environment
// variable, which beats bees.toml, which beats the built-in default. Values
// config.Validate has already rejected are ignored, so an unreadable table
// can never make logging worse than the flags asked for.
func mergeLogging(f consoleFlags, l config.Logging) logging.Options {
	o := logging.Options{Format: f.format, Level: f.level, Quiet: f.quiet}
	if !f.formatExplicit {
		if v, err := logging.ParseFormat(l.Format); err == nil && l.Format != "" {
			o.Format = v
		}
	}
	if !f.levelExplicit {
		if v, err := logging.ParseLevel(l.Level); err == nil && l.Level != "" {
			o.Level = v
		}
	}
	// --quiet together with level = "debug" in bees.toml is not the
	// contradiction --quiet --verbose is: the file states a default, the flag
	// an intent for this invocation, so quiet wins.
	if o.Quiet && o.Level < slog.LevelInfo {
		o.Level = slog.LevelInfo
	}
	return o
}

// explicitFlag reports whether a logging dimension was chosen on the command
// line or in the environment, rather than left at the flag's default.
func explicitFlag(cmd *cobra.Command, name, env string) bool {
	return cmd.Flags().Changed(name) || os.Getenv(env) != ""
}

// flagOrEnv returns the flag value when it was set on the command line,
// otherwise the environment variable, otherwise the flag's default.
func flagOrEnv(cmd *cobra.Command, name, value, env string) string {
	if cmd.Flags().Changed(name) {
		return value
	}
	if v := os.Getenv(env); v != "" {
		return v
	}
	return value
}

// groupCmd returns a command that only exists to hold subcommands: a bare
// invocation prints help, an unknown subcommand is an error.
//
// The RunE is what makes the second half work: cobra only runs the "unknown
// command" check for the root command, and only validates Args on a runnable
// command — a group with no Run/RunE returns help (and exit 0) before Args is
// ever evaluated. Callers set Long and Hidden on the result.
func groupCmd(use, short string) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return cmd.Help() },
	}
}
