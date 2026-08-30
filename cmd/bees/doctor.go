package main

import (
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/doctor"
)

func newDoctorCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Check that the machine, the configuration and GitHub are ready",
		Long: `doctor runs the preflight checks the factory otherwise only discovers
mid-run: the toolchain (git, gh, claude), the configuration, access to the
GitHub repository and creating a worktree.

Every check reports a pass, a warning (something that will probably bite you)
or a failure (the factory cannot run), plus the command to fix it. doctor
never fixes anything itself. It exits 1 when a check failed; warnings do not
change the exit code.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			d := &doctor.Deps{ClaudeBin: claudeBin()}
			// No bees.toml is not fatal: the toolchain checks still run and
			// the config check reports why there is no configuration.
			if path, err := configPath(g); err != nil {
				d.ConfigErr = err
			} else {
				d = doctor.New(cmd.Context(), path, d.ClaudeBin)
			}
			results := doctor.Run(cmd.Context(), d.Checks())
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
	return cmd
}
