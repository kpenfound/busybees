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

	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/versions"
	"github.com/spf13/cobra"
)

// version is the release version stamped in by
// `-ldflags "-X main.version=..."`. Without it, versions.Bees falls back to
// the module version or VCS revision Go records in the binary.
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
}

func newRoot() *cobra.Command {
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
		newKillCmd(g),
		newMailCmd(g),
		newIssueCmd(g),
		newDoneCmd(),
		newMCPCmd(g),
		newConfigCmd(g),
		newPromptsCmd(g),
		newLabelsCmd(g),
		newVersionCmd(),
	)
	return root
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
	g.logger = logging.New(logging.Options{Format: format, Level: level, Quiet: g.quiet, Console: cmd.ErrOrStderr()})
	slog.SetDefault(g.logger.Logger)
	return nil
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
