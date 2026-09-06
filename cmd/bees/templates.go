package main

import (
	"fmt"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/text"
	"github.com/spf13/cobra"
)

// ---- templates -------------------------------------------------------------

// newTemplatesCmd lists the config templates, prints the bees.toml each one
// writes, and reports how this project's config differs from one. `list` and
// `show` read no bees.toml and no git remote: like `bees init --print`, they
// only render. `diff` reads the config the way `bees config show` does.
func newTemplatesCmd(g *globalFlags) *cobra.Command {
	cmd := groupCmd("templates", "List the config templates, print the bees.toml each one writes, compare this project to one")
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
			file, err := config.RenderTOML(config.RenderOptions{Template: &t})
			if err != nil {
				return err
			}
			fmt.Print(file)
			return nil
		},
	}
	diff := &cobra.Command{
		Use:   "diff [name]",
		Short: "Report the settings this project's config and a template disagree on",
		Long: `Report the settings this project's config and a template disagree on.

With no name, the closest template is reported, so a project set up before the
templates existed can still be placed. The comparison is on the resolved
values, not on the file text: a key left commented out compares equal to a
template that sets it to the same default. Only the settings a template
decides are compared; repo, filter, models, budgets and intervals are not.
Differences are reported, not gated: the exit status is 0 either way.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The name is resolved before the config is read, so an unknown
			// one is reported wherever the command is run from.
			var tpl config.Template
			var err error
			closest := len(args) == 0
			if !closest {
				if tpl, err = config.TemplateByName(args[0]); err != nil {
					return err
				}
			}
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			var diffs []config.Difference
			if closest {
				tpl, diffs = config.Closest(cfg)
			} else {
				diffs = tpl.Compare(cfg)
			}
			fmt.Print(renderTemplateDiff(cfg.Path, tpl, diffs, closest))
			return nil
		},
	}
	cmd.AddCommand(list, show, diff)
	return cmd
}

// renderTemplateDiff is what `bees templates diff` prints: the path of the
// config that was read first, so it is obvious which file the report is
// about, then one line per setting the two disagree on, with the columns
// padded to the widest row. closest says the template was chosen rather than
// named, which the heading records.
func renderTemplateDiff(path string, tpl config.Template, diffs []config.Difference, closest bool) string {
	if len(diffs) == 0 {
		return fmt.Sprintf("%s matches %s.\n", path, tpl.Name)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s vs %s", path, tpl.Name)
	if closest {
		fmt.Fprintf(&b, " (closest of %s)", text.Count(len(config.Templates()), "template"))
	}
	b.WriteString("\n\n")
	keyw, valw := 0, 0
	for _, d := range diffs {
		keyw, valw = max(keyw, len(d.Key)), max(valw, len(d.Config))
	}
	for _, d := range diffs {
		fmt.Fprintf(&b, "  %-*s%-*s(%s: %s)\n", keyw+3, d.Key, valw+2, d.Config, tpl.Name, d.Template)
	}
	return b.String()
}
