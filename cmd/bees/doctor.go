package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/doctor"
)

func newDoctorCmd(g *globalFlags) *cobra.Command {
	var asJSON, fix bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that the machine, the configuration and GitHub are ready",
		Long: `doctor runs the preflight checks the factory otherwise only discovers
mid-run: the toolchain (git, gh, claude), the configuration, access to the
GitHub repository and creating a worktree.

Every check reports a pass, a warning (something that will probably bite you)
or a failure (the factory cannot run), plus the command to fix it. doctor
changes nothing unless --fix is given. It exits 1 when a check failed;
warnings do not change the exit code.

--fix applies the repairs doctor knows how to make, prints one line per action
and then re-runs the checks, so the table is what the repository looks like
afterwards. The only repair today brings open issues and pull requests that
carry the base label but fall outside filter.assignee or filter.milestone back
into the filter. It never touches an item that does not carry the base label,
and it never changes bees.toml.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := &doctor.Deps{ClaudeBin: claudeBin()}
			// No bees.toml is not fatal: the toolchain checks still run and
			// the config check reports why there is no configuration.
			if path, err := configPath(g); err != nil {
				d.ConfigErr = err
			} else {
				d = doctor.New(cmd.Context(), path, d.ClaudeBin)
			}
			checks := d.Checks()
			results := doctor.Run(cmd.Context(), checks)
			if fix {
				outcomes := doctor.ApplyFixes(cmd.Context(), checks, results)
				// The fix log goes to stderr under --json so the JSON on
				// stdout stays parseable.
				out := cmd.OutOrStdout()
				if asJSON {
					out = cmd.ErrOrStderr()
				}
				_, _ = fmt.Fprint(out, doctor.FixText(outcomes))
				// The table must describe the repository after the repair,
				// and the exit code must follow it.
				results = doctor.Run(cmd.Context(), checks)
			}
			if asJSON {
				b, err := json.MarshalIndent(results, "", "  ")
				if err != nil {
					return err
				}
				fmt.Println(string(b))
			} else {
				fmt.Print(doctor.Text(results))
			}
			if n := doctor.Failures(results); n > 0 {
				return fmt.Errorf("%d of %d checks failed", n, len(results))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print the results as JSON")
	cmd.Flags().BoolVar(&fix, "fix", false, "apply the repairs doctor knows how to make, then re-run the checks")
	return cmd
}
