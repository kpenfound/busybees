package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/review"
	"github.com/kpenfound/busybees/internal/reviewtui"
	"github.com/kpenfound/busybees/internal/text"
)

// ---- review ----------------------------------------------------------------

// newReviewCmd holds `bees review`, the pull request review tool, which a
// person runs on their own. It reads no bees.toml and no factory state: its
// configuration is ~/.config/bees/config.toml (internal/review).
func newReviewCmd() *cobra.Command {
	var configPath string
	var end endFlags
	var who triageFlags
	cmd := &cobra.Command{
		Use:   "review <pr>",
		Short: "The pull request review tool",
		Long: `bees review reviews a GitHub pull request from several angles at once and
hands you what they found.

The pull request is a github.com URL, owner/name#123, or a number when the
current directory is a checkout of the repository. The review gathers the
pull request's context, briefs it, reviews it from the angles its size
calls for that the repository's context.toml enables, merges what the
angles found, and shows you each finding in turn to select, dismiss, defer
or ask about (see triage). What you selected then ends the review one of five ways: posted as
review comments with an approval, as a comment-only review or with changes
requested, printed as a markdown report, or discarded. --post and --report
choose; with neither, the output key of ~/.config/bees/config.toml does, and
its default asks you at the end.

Each step prints a line as it goes, and at a terminal one row per angle
shows a spinner while its session runs and a mark when it ends; --no-tui,
or a stdout that is not a terminal, prints a line per angle as it starts and
ends instead.

With --agent an agent session triages instead of you, with the same four
actions: what it dismisses goes into your reviewer notes, and --instructions
tells it what you want from the review. It chooses how the review ends where
you would have been asked.

Its settings are ~/.config/bees/config.toml, and the repository's own
context.toml says which angles run there. Every finding you dismiss is
recorded in your reviewer notes, and consolidate turns the dismissals that
repeat into rules: what a review drops, or ranks down, before it reaches you
again.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 {
				return cmd.Help()
			}
			ctx := cmd.Context()
			if err := who.check(); err != nil {
				return err
			}
			cfg, err := review.LoadConfig(configPath)
			if err != nil {
				return err
			}
			mode, err := end.mode(cfg)
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
			runner, err := review.NewRunner(ctx, ref, cfg, cwd)
			if err != nil {
				return err
			}
			artifact, err := runReviewShowingProgress(ctx, runner, ref, who.noTUI)
			if err != nil {
				return err
			}
			queue, err := runner.Queue(artifact)
			if err != nil {
				return err
			}
			return who.triage(ctx, cmd, cfg, ref, artifact.Brief, queue, runner.Pipeline.Dir, mode)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the global configuration (default: ~/.config/bees/config.toml)")
	end.add(cmd)
	who.add(cmd)
	cmd.AddCommand(newReviewTriageCmd())
	cmd.AddCommand(newReviewConsolidateCmd())
	return cmd
}

// endFlags are the flags that choose how a review ends: --post with one of
// the three modes that submit a review, or --report.
type endFlags struct {
	post   string
	report bool
}

// postModes are the values --post takes.
var postModes = []string{review.OutputApprove, review.OutputComment, review.OutputReject}

func (e *endFlags) add(cmd *cobra.Command) {
	cmd.Flags().StringVar(&e.post, "post", "", "submit the selected findings as one review: "+strings.Join(postModes, ", "))
	cmd.Flags().BoolVar(&e.report, "report", false, "print the selected findings as a markdown report and post nothing")
}

// mode is the output mode the flags chose, and cfg's when they chose none.
// Both flags at once, and a --post value that is not a posting mode, are
// errors.
func (e *endFlags) mode(cfg *review.Config) (string, error) {
	switch {
	case e.post != "" && e.report:
		return "", errors.New("--post and --report choose different ends: give one of them")
	case e.report:
		return review.OutputReport, nil
	case e.post == "":
		return cfg.Output, nil
	case !review.Posts(e.post):
		return "", fmt.Errorf("--post %q: want one of %s", e.post, strings.Join(postModes, ", "))
	}
	return e.post, nil
}

// endReview ends a review the way mode says, once triage is over: it asks
// at the console when the mode is to ask, prints the report, posts the
// review, or discards. console is nil when an agent triaged, which never
// leaves the mode to ask. A review that could not be posted as asked at the
// console (nothing selected for a comment-only review) is asked again;
// asked for by a flag or the configuration, it is the command's error.
func endReview(ctx context.Context, cfg *review.Config, ref review.Ref, brief *review.Brief, queue *review.Queue, console *review.Console, mode string) error {
	ask := mode == review.OutputAsk
	for {
		if ask {
			mode = console.Choose(queue)
		}
		switch mode {
		case review.OutputDiscard:
			fmt.Printf("%s: nothing posted\n", ref)
			return nil
		case review.OutputReport:
			fmt.Print(review.Report(brief, queue.Selected()))
			return nil
		}
		posted, err := review.Post(ctx, review.NewClient(ref, cfg), ref, mode, queue.Selected())
		var refusal *review.Refusal
		if ask && errors.As(err, &refusal) {
			fmt.Printf("%v\n", err)
			continue
		}
		if err != nil {
			return err
		}
		fmt.Printf("%s: %s with %s\n", ref, review.Verb(mode), text.Count(len(posted.Comments), "comment"))
		return nil
	}
}

// newReviewTriageCmd builds `bees review triage`.
func newReviewTriageCmd() *cobra.Command {
	var configPath string
	var end endFlags
	var who triageFlags
	cmd := &cobra.Command{
		Use:   "triage <pr>",
		Short: "Pick up the triage of a pull request's latest review",
		Long: `Open the latest review of a pull request and triage what is still undecided.

The pull request is a github.com URL, owner/name#123, or a number when the
current directory is a checkout of the repository. Each undecided finding is
shown in turn, and a key decides it: s selects it, e opens its comment text
in $VISUAL or $EDITOR and selects it, d dismisses it with a reason that is
appended to your reviewer notes, f defers it, a asks the angle that found it
a question, n leaves it for now and q stops. Every decision is written into
the review's artifact directory as it is taken, so stopping loses nothing.
The review then ends the way bees review's does: what is selected is
posted, printed as a report, or discarded, as --post, --report, the output
key of ~/.config/bees/config.toml or the prompt at the end says.

At a terminal, the same keys drive a full-screen view beside the diff,
pressed without return; --no-tui, or a stdout that is not a terminal, is a
console where a key needs return.

With --agent an agent session triages what is undecided instead of you, as
bees review --agent does, told what --instructions says.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			if err := who.check(); err != nil {
				return err
			}
			cfg, err := review.LoadConfig(configPath)
			if err != nil {
				return err
			}
			mode, err := end.mode(cfg)
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
			return who.triage(ctx, cmd, cfg, ref, artifact.Brief, queue, checkout, mode)
		},
	}
	cmd.Flags().StringVar(&configPath, "config", "", "path to the global configuration (default: ~/.config/bees/config.toml)")
	end.add(cmd)
	who.add(cmd)
	return cmd
}

// runReviewTUI draws the terminal triage screen: a variable so a test can
// swap it for one that needs no terminal, the way isTerminal is.
var runReviewTUI = reviewtui.Run

// triageFlags choose who triages: you, at the terminal UI or the console, or
// with --agent an agent session (review.AgentTriage) told what
// --instructions says. --no-tui also keeps the review's progress to plain
// lines (runReviewShowingProgress).
type triageFlags struct {
	agent        bool
	instructions string
	noTUI        bool
}

func (w *triageFlags) add(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&w.agent, "agent", false, "let an agent session triage the findings instead of you (factory mode)")
	cmd.Flags().StringVar(&w.instructions, "instructions", "", "what the agent triaging with --agent is told you want, in your words")
	cmd.Flags().BoolVar(&w.noTUI, "no-tui", false, "print the review's progress as plain lines and triage at the console instead of the terminal UI")
}

// check refuses --instructions without --agent, which nobody would read.
func (w *triageFlags) check() error {
	if w.instructions != "" && !w.agent {
		return errors.New("--instructions are for the agent that triages: give --agent too")
	}
	return nil
}

// triage triages queue: at a terminal, in the internal/reviewtui screen,
// unless --no-tui was given or stdout is not a terminal, when it falls back
// to the console; or with --agent by an agent session run in dir. It then
// ends the review the way mode says. The agent chooses the end when mode is
// to ask.
func (w *triageFlags) triage(ctx context.Context, cmd *cobra.Command, cfg *review.Config, ref review.Ref, brief *review.Brief, queue *review.Queue, dir, mode string) error {
	if !w.agent {
		console := &review.Console{In: cmd.InOrStdin(), Out: os.Stdout, Editor: editComment}
		if tuiMode(w.noTUI, os.Stdout) {
			diff, err := review.NewClient(ref, cfg).PRDiff(ctx, ref.Number)
			if err != nil {
				return fmt.Errorf("read the diff of %s: %w", ref, err)
			}
			if err := runReviewTUI(ctx, diff, queue); err != nil {
				return err
			}
		} else if err := console.Run(ctx, queue); err != nil {
			return err
		}
		return endReview(ctx, cfg, ref, brief, queue, console, mode)
	}
	agent := review.NewAgentTriage(cfg, dir)
	agent.Instructions, agent.Mode, agent.Log = w.instructions, mode, os.Stdout
	mode, err := agent.Run(ctx, queue)
	if err != nil {
		return err
	}
	return endReview(ctx, cfg, ref, brief, queue, nil, mode)
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
