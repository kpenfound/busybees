package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/eval"
	"github.com/kpenfound/busybees/internal/review"
	"github.com/kpenfound/busybees/internal/skills"
	"github.com/kpenfound/busybees/internal/text"
)

// evalsDir is the directory `bees eval` reads its cases from, relative to
// the current directory.
const evalsDir = "evals"

func newEvalCmd(g *globalFlags) *cobra.Command {
	var caseName, profile string
	cmd := &cobra.Command{
		Use:   "eval [role]",
		Args:  cobra.MaximumNArgs(1),
		Short: "Run the factory, or one role, against the eval cases under evals/ and grade the result",
		Long: `eval runs the whole factory against each case under ./evals/: it builds the
case's fixture repository as a local origin, seeds an in-memory GitHub with the
case's issues, pull requests and mail, and runs the scheduler until every
seeded issue is closed or held for a person, or the case's timeout or budget
runs out. The factory's approved pull requests are merged as a person would
merge them.

A case passes when its test command passes on the default branch after the
run (having failed before it), and every seeded issue closed with a pull
request of its own. The table goes to stdout and the JSON report to
<state_dir>/evals/<timestamp>/report.json, beside each case's fixture, state
and test output. It exits non-zero when any case fails.

With a role, eval runs that role in isolation against the cases under
./evals/<role>/, the way bees exec runs one session: the scheduler is scoped
to that role, so the seeded GitHub state and mailbox stand in for the rest of
the factory, which stays configured so that routing does not move. Such a
case is graded by the checks it declares — the outcome the session reported,
labels moved, mail sent, issues created or closed, a pull request opened —
and by the rubrics a grader session scores, whose score is the table's SCORE
column.

Sessions are real agent sessions and cost money. --profile runs every role on
one profile from bees.toml or ~/.config/bees/config.toml; without it the eval
takes the profiles bees.toml selects, or config.toml's provider and model when
there is no bees.toml. Nothing else of bees.toml is used. See docs/evals.md.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseInsideSession("eval"); err != nil {
				return err
			}
			role := ""
			if len(args) == 1 {
				var err error
				if role, err = config.CanonicalRole(args[0]); err != nil {
					return err
				}
			}
			local, err := evalLocalConfig(g)
			if err != nil {
				return err
			}
			global, err := review.LoadConfig("")
			if err != nil {
				return err
			}
			sel, err := eval.SelectProfile(profile, local, global)
			if err != nil {
				return err
			}
			cases, err := evalCases(role, caseName)
			if err != nil {
				return err
			}
			stateDir := ".bees"
			if local != nil {
				stateDir = local.StateDir()
			}
			dir, err := filepath.Abs(filepath.Join(stateDir, "evals", time.Now().Format("20060102-150405")))
			if err != nil {
				return err
			}
			self, err := os.Executable()
			if err != nil {
				return err
			}
			skillMgr := skills.NewManager(cacheDir())
			r := &eval.Runner{Bees: self, ClaudeBin: claudeBin(), CodexBin: codexBin(), OpenCodeBin: opencodeBin(), PiBin: piBin(),
				Skills: skillMgr, Grader: evalGrader(global), Console: cmd.ErrOrStderr()}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "running %s%s with profile %s in %s\n",
				text.Count(len(cases), "case"), evalOf(role), sel, dir)
			rep, err := r.Run(cmd.Context(), cases, sel, dir)
			if rep != nil {
				_, _ = fmt.Fprint(cmd.OutOrStdout(), rep.Table())
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nreport: %s\n", rep.Path())
			}
			if err != nil {
				return err
			}
			return evalExit(rep, len(cases))
		},
	}
	cmd.Flags().StringVar(&caseName, "case", "", "run only this case, from evals/ or evals/<role>/")
	cmd.Flags().StringVar(&profile, "profile", "", "run every role on this profile, from bees.toml or ~/.config/bees/config.toml")

	// The gh every eval session runs: the script eval writes in front of
	// the session's PATH calls this, never a person.
	gh := &cobra.Command{
		Use:                eval.ShimCommand[1] + " <url> [gh arguments]",
		Short:              "Forward one gh call to a running eval's GitHub (run by the eval's gh, not by hand)",
		Hidden:             true,
		DisableFlagParsing: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return errors.New("usage: bees eval gh <url> [gh arguments]")
			}
			dir, err := os.Getwd()
			if err != nil {
				return err
			}
			if code := eval.Shim(cmd.Context(), args[0], dir, args[1:], cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()); code != 0 {
				os.Exit(code)
			}
			return nil
		},
	}
	cmd.AddCommand(gh)
	return cmd
}

// evalCases are the cases a run takes: the whole-factory ones under evals/,
// or one role's under evals/<role>/.
func evalCases(role, name string) ([]eval.Case, error) {
	if role == "" {
		return eval.LoadCases(evalsDir, name)
	}
	return eval.LoadRoleCases(evalsDir, role, name)
}

// evalOf names the role a per-role run is of, for the line that says what
// is about to run.
func evalOf(role string) string {
	if role == "" {
		return ""
	}
	return " for the " + role
}

// evalGrader is the agent every graded check of a run is judged by: the
// person's own read-only session agent, from ~/.config/bees/config.toml,
// and never the profile the eval runs its roles on. Two runs of a case are
// compared by their scores, so what does the scoring has to stay the same
// while --profile changes what is being scored.
func evalGrader(global *review.Config) review.Agent {
	a := review.NewAgent(global)
	a.ClaudeBin, a.CodexBin = claudeBin(), codexBin()
	return a
}

// evalExit is the error bees eval exits with for a run of total cases: one
// when the run stopped before every case ran, whatever the cases that ran
// came to, and one when a case failed.
func evalExit(rep *eval.Report, total int) error {
	if len(rep.Cases) < total {
		return fmt.Errorf("the eval was stopped after %s of %d", text.Count(len(rep.Cases), "case"), total)
	}
	failed := 0
	for _, c := range rep.Cases {
		if !c.Pass {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("%s of %d failed", text.Count(failed, "case"), len(rep.Cases))
	}
	return nil
}

// evalLocalConfig is the bees.toml a normal run in this directory would
// use, or nil when there is none.
func evalLocalConfig(g *globalFlags) (*config.Config, error) {
	path, err := configPath(g)
	if err != nil {
		if g.config == "" {
			return nil, nil
		}
		return nil, err
	}
	cfg, err := config.Load(path)
	if errors.Is(err, config.ErrMachineConfig) {
		return nil, fmt.Errorf("bees eval reads its profiles from a project's bees.toml: %w", err)
	}
	return cfg, err
}
