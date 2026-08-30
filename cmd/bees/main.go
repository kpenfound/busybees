// Command bees is the busybees software factory: a lightweight orchestrator
// that runs a staff of Claude Code sessions (product manager, project
// manager, developers, reviewers, QA) against one GitHub repository.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
)

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
	config  string
	verbose bool
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
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			level := slog.LevelInfo
			if g.verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))
		},
	}
	root.PersistentFlags().StringVarP(&g.config, "config", "c", "", "path to bees.toml (default: search upwards from cwd, or $BEES_CONFIG)")
	root.PersistentFlags().BoolVarP(&g.verbose, "verbose", "v", false, "debug logging")

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
		newConfigCmd(g),
		newSkillsCmd(g),
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
			fmt.Println("bees", version)
		},
	}
}
