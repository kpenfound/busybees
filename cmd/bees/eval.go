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
		Use: "eval",
		// No role argument yet: per-role evals (bees eval <role>) will read
		// evals/<role>/, which LoadCases already leaves alone.
		Args:  cobra.NoArgs,
		Short: "Run the whole factory against the eval cases under evals/ and grade the result",
		Long: `eval runs the whole factory against each case under ./evals/: it builds the
case's fixture repository as a local origin, seeds an in-memory GitHub with the
case's issues and mail, and runs the scheduler until every seeded issue is
closed or held for a person, or the case's timeout or budget runs out. The
factory's approved pull requests are merged as a person would merge them.

A case passes when its test command passes on the default branch after the
run (having failed before it), and every seeded issue closed with a pull
request of its own. The table goes to stdout and the JSON report to
<state_dir>/evals/<timestamp>/report.json, beside each case's fixture, state
and test output. It exits non-zero when any case fails.

Sessions are real agent sessions and cost money. --profile runs every role on
one profile from bees.toml or ~/.config/bees/config.toml; without it the eval
takes the profiles bees.toml selects, or config.toml's provider and model when
there is no bees.toml. Nothing else of bees.toml is used. See docs/evals.md.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := refuseInsideSession("eval"); err != nil {
				return err
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
			cases, err := eval.LoadCases(evalsDir, caseName)
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
			r := &eval.Runner{Bees: self, ClaudeBin: claudeBin(), CodexBin: codexBin(), OpenCodeBin: opencodeBin(),
				Skills: skillMgr, Console: cmd.ErrOrStderr()}
			_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "running %s with profile %s in %s\n", text.Count(len(cases), "case"), sel, dir)
			rep, err := r.Run(cmd.Context(), cases, sel, dir)
			if rep != nil {
				_, _ = fmt.Fprint(cmd.OutOrStdout(), rep.Table())
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "\nreport: %s\n", rep.Path())
			}
			if err != nil {
				return err
			}
			if !rep.Pass() {
				failed := 0
				for _, c := range rep.Cases {
					if !c.Pass {
						failed++
					}
				}
				if len(rep.Cases) < len(cases) {
					return fmt.Errorf("the eval was stopped after %s", text.Count(len(rep.Cases), "case"))
				}
				return fmt.Errorf("%s of %d failed", text.Count(failed, "case"), len(rep.Cases))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&caseName, "case", "", "run only the case in evals/<name>")
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
