package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

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
	cmd.AddCommand(newReviewTriageCmd())
	cmd.AddCommand(newReviewConsolidateCmd())
	return cmd
}

// newReviewTriageCmd builds `bees review triage`.
func newReviewTriageCmd() *cobra.Command {
	var configPath string
	cmd := &cobra.Command{
		Use:   "triage <pr>",
		Short: "Pick up the triage of a pull request's latest review",
		Long: `Open the latest review of a pull request and triage what is still undecided.

The pull request is a github.com URL, owner/name#123, or a number when the
current directory is a checkout of the repository. Each undecided finding is
shown in turn, and a key followed by return decides it: s selects it, e opens
its comment text in $VISUAL or $EDITOR and selects it, d dismisses it with a
reason that is appended to your reviewer notes, f defers it, a asks the angle
that found it a question, n leaves it for now and q stops. Every decision is
written into the review's artifact directory as it is taken, so stopping
loses nothing.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := review.LoadConfig(configPath)
			if err != nil {
				return err
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			ref, err := review.ResolveRef(ctx, args[0], cwd)
			if err != nil {
				return err
			}
			dir, err := review.LatestArtifactDir(cfg.ResolvedStoragePath(), ref)
			if err != nil {
				return err
			}
			artifact, err := review.ReadArtifact(dir)
			if err != nil {
				return err
			}
			notes, err := review.ReadNotes(cfg.ResolvedNotesPath())
			if err != nil {
				return err
			}
			// The project's context.toml is read from a checkout of the
			// repository under review, and from nowhere else: another
			// repository's pins are not this one's.
			checkout := review.CheckoutOf(ctx, ref.Repo, cwd)
			project := &review.Project{}
			if checkout != "" {
				if project, err = review.LoadProject(review.FindProject(checkout)); err != nil {
					return err
				}
			}
			queue, err := review.NewQueue(artifact, project, notes, review.NewAngles(cfg, checkout))
			if err != nil {
				return err
			}
			fmt.Printf("%s: the review started %s\n", ref, filepath.Base(dir))
			console := &review.Console{In: cmd.InOrStdin(), Out: os.Stdout, Editor: editComment}
			return console.Run(ctx, queue)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the global configuration (default: ~/.config/bees/config.toml)")
	return cmd
}

// editComment opens a finding's comment text in $VISUAL, $EDITOR or vi and
// returns what it was edited to.
func editComment(text string) (string, error) {
	f, err := os.CreateTemp("", "bees-review-*.md")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer func() { _ = os.Remove(path) }()
	if !strings.HasSuffix(text, "\n") {
		text += "\n"
	}
	if _, err := f.WriteString(text); err != nil {
		return "", errors.Join(err, f.Close())
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	argv := append(editorArgv(os.Getenv("VISUAL"), os.Getenv("EDITOR")), path)
	edit := exec.Command(argv[0], argv[1:]...)
	edit.Stdin, edit.Stdout, edit.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := edit.Run(); err != nil {
		return "", fmt.Errorf("%s: %w", argv[0], err)
	}
	edited, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(edited), nil
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
