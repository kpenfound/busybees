package main

import (
	"fmt"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/spf13/cobra"
)

// ---- templates -------------------------------------------------------------

// newTemplatesCmd lists the config templates and prints the bees.toml each one
// writes. Neither subcommand reads a bees.toml or a git remote: like `bees
// init --print`, they only render.
func newTemplatesCmd() *cobra.Command {
	cmd := groupCmd("templates", "List the config templates and print the bees.toml each one writes")
	list := &cobra.Command{
		Use:   "list",
		Short: "Print every template, name then summary",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, t := range config.Templates() {
				fmt.Printf("%-14s %s\n", t.Name, t.Summary)
			}
			return nil
		},
	}
	show := &cobra.Command{
		Use:   "show <name>",
		Short: "Print the bees.toml a template writes, headed by when to use it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			t, err := config.TemplateByName(args[0])
			if err != nil {
				return err
			}
			text, err := config.RenderTOML(config.RenderOptions{Template: &t})
			if err != nil {
				return err
			}
			fmt.Print(text)
			return nil
		},
	}
	cmd.AddCommand(list, show)
	return cmd
}
