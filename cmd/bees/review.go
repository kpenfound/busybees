package main

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/review"
	"github.com/kpenfound/busybees/internal/text"
)

// ---- review ----------------------------------------------------------------

// newReviewCmd holds the parts of `bees review`, the pull request review
// tool, that a person runs on their own. It reads no bees.toml and no factory
// state: its configuration is ~/.config/bees/config.toml (internal/review).
func newReviewCmd() *cobra.Command {
	cmd := groupCmd("review", "The pull request review tool")
	cmd.Long = `bees review reviews a GitHub pull request from several angles at once and
hands you what they found.

Its settings are ~/.config/bees/config.toml, and the repository's own
context.toml says which angles run there. Every finding you dismiss is
recorded in your reviewer notes, and consolidate turns the dismissals that
repeat into rules: what a review drops, or ranks down, before it reaches you
again.`
	cmd.AddCommand(newReviewConsolidateCmd())
	return cmd
}

// newReviewConsolidateCmd builds `bees review consolidate`.
func newReviewConsolidateCmd() *cobra.Command {
	var notesPath string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "consolidate",
		Short: "Turn the dismissals that repeat in your reviewer notes into rules",
		Long: `Read the reviewer notes and write the dismissals that repeat into rules.

Dismissals of the same repository, angle and category whose reasons read
alike are one pattern. A pattern dismissed twice becomes a rule that ranks
its findings down, three times or more one that drops them, and both reach
the angle sessions of the next review as what has been dismissed before.

Rules live between the two markers in the notes; nothing else in the file is
touched, and nothing in the block is deleted or rewritten. A rule you
reworded, or whose action you changed, stays as you wrote it and only its
count moves: you read the findings.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			path, err := reviewNotesPath(notesPath)
			if err != nil {
				return err
			}
			notes, err := review.ReadNotes(path)
			if err != nil {
				return err
			}
			if !notes.Loaded {
				fmt.Printf("no reviewer notes yet: %s\n", notes.Path)
				return nil
			}
			added, refreshed := notes.Consolidate()
			printConsolidation(notes, added, refreshed)
			if dryRun || (len(added) == 0 && len(refreshed) == 0) {
				return nil
			}
			return notes.Write()
		},
	}
	cmd.Flags().StringVar(&notesPath, "notes", "", "path to the reviewer notes (default: notes_path in ~/.config/bees/config.toml)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what consolidation would write, and write nothing")
	return cmd
}

// reviewNotesPath is the reviewer notes file to work on: the one named on the
// command line, or the one the global configuration points at.
func reviewNotesPath(flag string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	cfg, err := review.LoadConfig("")
	if err != nil {
		return "", err
	}
	return cfg.ResolvedNotesPath(), nil
}

// printConsolidation reports what a consolidation came to: what the notes
// hold, what it wrote, and the lines of the rules block it did not read as
// rules.
func printConsolidation(notes *review.Notes, added, refreshed []review.Rule) {
	fmt.Printf("%s: %s, %s\n", notes.Path, text.Count(len(notes.Dismissals), "dismissal"), text.Count(len(notes.Rules), "rule"))
	for _, group := range []struct {
		label string
		rules []review.Rule
	}{{"added", added}, {"refreshed", refreshed}} {
		if len(group.rules) == 0 {
			continue
		}
		fmt.Printf("\n%s:\n", group.label)
		for _, r := range group.rules {
			fmt.Println(r.Line())
		}
	}
	if len(added) == 0 && len(refreshed) == 0 {
		fmt.Printf("\nnothing to consolidate: no pattern is new and every rule's count is current\n")
	}
	if len(notes.Skipped) > 0 {
		fmt.Printf("\n%s in the rules block did not read as a rule, and stays as written:\n", text.Count(len(notes.Skipped), "line"))
		for _, line := range notes.Skipped {
			fmt.Println(line)
		}
	}
}
