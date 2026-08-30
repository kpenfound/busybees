package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// notesStore returns the state directory holding the notes files, resolved the
// same way as the mailbox: an explicit -c, then $BEES_STATE_DIR (so a session
// can append to its own notes), then the config found by configPath.
func notesStore(g *globalFlags) (*state.Store, error) {
	dir, err := mailStateDir(g.config, os.Getenv(session.EnvStateDir), func() (*config.Config, error) {
		return loadConfig(g)
	})
	if err != nil {
		return nil, err
	}
	return state.New(dir), nil
}

// editorArgv picks the command that opens a notes file: $VISUAL, then $EDITOR,
// then "vi". Either variable may carry arguments ("code -w"), which are kept.
func editorArgv(visual, editor string) []string {
	for _, v := range []string{visual, editor} {
		if fields := strings.Fields(v); len(fields) > 0 {
			return fields
		}
	}
	return []string{"vi"}
}

// notesSizeText renders a notes file size for `bees status`; an empty file
// (which is what a role that has never run has) reads as "-".
func notesSizeText(n int64) string {
	switch {
	case n <= 0:
		return "-"
	case n < 1024:
		return fmt.Sprintf("%d B", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(n)/(1024*1024))
	}
}

func newNotesCmd(g *globalFlags) *cobra.Command {
	cmd := groupCmd("notes", "Read, steer and reset a role's notes file")
	cmd.Long = `<state_dir>/notes/<role>.md is a role's only memory between
sessions: its contents go into every task prompt and the role updates it before
it finishes. Editing it is the most direct way to steer a role.`

	show := &cobra.Command{
		Use:   "show <role>",
		Short: "Print a role's notes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := config.CanonicalRole(args[0])
			if err != nil {
				return err
			}
			store, err := notesStore(g)
			if err != nil {
				return err
			}
			text, err := store.ReadNotes(role)
			if err != nil {
				return err
			}
			fmt.Print(text)
			return nil
		},
	}

	edit := &cobra.Command{
		Use:   "edit <role>",
		Short: "Open a role's notes in $VISUAL, $EDITOR or vi",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if os.Getenv(session.EnvSessionDir) != "" {
				return errors.New("bees notes edit needs a terminal and cannot run inside a session; edit the notes file directly, or use bees notes add")
			}
			role, err := config.CanonicalRole(args[0])
			if err != nil {
				return err
			}
			store, err := notesStore(g)
			if err != nil {
				return err
			}
			if err := store.EnsureNotes(role); err != nil {
				return err
			}
			argv := append(editorArgv(os.Getenv("VISUAL"), os.Getenv("EDITOR")), store.NotesPath(role))
			ed := exec.CommandContext(cmd.Context(), argv[0], argv[1:]...)
			ed.Stdin, ed.Stdout, ed.Stderr = os.Stdin, os.Stdout, os.Stderr
			return ed.Run()
		},
	}

	reset := &cobra.Command{
		Use:   "reset <role>",
		Short: "Archive a role's notes and start a fresh file",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := config.CanonicalRole(args[0])
			if err != nil {
				return err
			}
			store, err := notesStore(g)
			if err != nil {
				return err
			}
			archived, err := store.ArchiveNotes(role, time.Now())
			if err != nil {
				return err
			}
			if archived == "" {
				fmt.Printf("%s had no notes; created %s\n", role, store.NotesPath(role))
				return nil
			}
			fmt.Printf("archived %s\n", archived)
			return nil
		},
	}

	var body, bodyFile string
	add := &cobra.Command{
		Use:     "add <role> [text]",
		Short:   "Append a bullet to a role's notes",
		Args:    cobra.RangeArgs(1, 2),
		Example: `  bees notes add developer "Always run dagger check before committing"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := config.CanonicalRole(args[0])
			if err != nil {
				return err
			}
			if len(args) == 2 {
				body = args[1]
			}
			text, err := readBody(body, bodyFile)
			if err != nil {
				return err
			}
			if text = strings.TrimRight(text, "\n"); text == "" {
				return errors.New("nothing to add: pass the text as an argument or with --body-file")
			}
			store, err := notesStore(g)
			if err != nil {
				return err
			}
			if err := store.AppendNotes(role, text); err != nil {
				return err
			}
			fmt.Printf("appended to %s\n", store.NotesPath(role))
			return nil
		},
	}
	add.Flags().StringVar(&body, "body", "", "text to append")
	add.Flags().StringVar(&bodyFile, "body-file", "", "read the text from a file (\"-\" for stdin)")

	cmd.AddCommand(show, edit, reset, add)
	return cmd
}
