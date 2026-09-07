package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/procs"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/workspace"
)

func newKillCmd(g *globalFlags) *cobra.Command {
	var dryRun, scheduler bool
	var grace time.Duration
	cmd := &cobra.Command{
		Use:   "kill",
		Short: "Stop leftover sessions and clean up worktrees after a crash",
		Long: `kill finds the agent sessions started by bees (from the pid files in the
state directory and from the process table, limited to sessions of this state
directory), terminates them together with their process groups (MCP servers,
shells), removes stale pid files, removes the temporary worktrees bees created
and resets the worker list in status.json. A session in the container sandbox
is found through its container, which the engine is asked for by label, and
stopped by removing it; the MCP server bees started on the host for it is
stopped too, from the pid file it left in the session directory.

Sessions of another project's factory are never touched, however many
factories share a machine.

It refuses to run while a bees scheduler is alive, because killing sessions
under a running scheduler corrupts its state; pass --scheduler to stop the
scheduler as well.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			store := state.New(cfg.StateDir())
			st, _ := store.LoadStatus()

			// Scheduler still running?
			if st.PID > 0 && st.PID != os.Getpid() && procs.Alive(st.PID) {
				if !scheduler {
					return fmt.Errorf("a bees scheduler is running (pid %d); stop it with Ctrl-C or pass --scheduler", st.PID)
				}
				fmt.Printf("stopping scheduler pid %d\n", st.PID)
				if !dryRun {
					pgid := st.PID
					if err := procs.Kill(procs.Proc{PID: st.PID, PGID: pgid}, grace); err != nil {
						return fmt.Errorf("stop scheduler: %w", err)
					}
				}
			}

			found, err := procs.Find(ctx, store.SessionsDir())
			if err != nil {
				return err
			}
			if len(found) == 0 {
				fmt.Println("no leftover sessions")
			}
			for _, p := range found {
				desc := p.Command
				if desc == "" {
					desc = filepath.Base(p.SessionDir)
				}
				fmt.Printf("killing %s (%s): %s\n", killTarget(p), p.Source, truncateStr(desc, 100))
				if dryRun {
					continue
				}
				if err := procs.Kill(p, grace); err != nil {
					fmt.Fprintf(os.Stderr, "warning: %s: %v\n", killTarget(p), err)
				}
				// A session stopped here wrote no result, and the next
				// session for its issue would otherwise have to guess
				// whether the machine crashed. Only sessions found through a
				// pid file or through their container name their directory;
				// a process found in the process table alone is killed just
				// the same, unmarked.
				if err := session.MarkInterrupted(p.SessionDir, "stopped by bees kill"); err != nil {
					fmt.Fprintf(os.Stderr, "warning: mark session interrupted: %v\n", err)
				}
			}

			removed, err := cleanWorktrees(ctx, cfg, dryRun)
			if err != nil {
				fmt.Fprintln(os.Stderr, "warning:", err)
			}
			for _, w := range removed {
				fmt.Println("removed worktree", w)
			}

			if !dryRun {
				st.Workers = nil
				for r := range st.Singletons {
					st.Singletons[r] = "idle"
				}
				if st.PID > 0 && !procs.Alive(st.PID) {
					st.LastError = "killed by bees kill"
				}
				if err := store.SaveStatus(st); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "show what would be killed and removed")
	cmd.Flags().BoolVar(&scheduler, "scheduler", false, "also stop a running bees scheduler")
	cmd.Flags().DurationVar(&grace, "grace", procs.DefaultGrace, "time to wait after SIGTERM before SIGKILL")
	return cmd
}

// killTarget names what stopping a session means for it: its process, the
// container it runs in and the MCP server left on the host for it, the last
// two for a container-backed session. Any of the three can be all there is
// to stop, when a crash left one of them behind on its own.
func killTarget(p procs.Proc) string {
	var parts []string
	if p.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", p.PID))
	}
	if p.Container != "" {
		parts = append(parts, "container "+shortID(p.Container))
	}
	if p.Server > 0 {
		parts = append(parts, fmt.Sprintf("MCP server pid %d", p.Server))
	}
	switch len(parts) {
	case 0:
		return fmt.Sprintf("pid %d", p.PID) // nothing to name: the pid as read
	case 1:
		return parts[0]
	default:
		return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
	}
}

// shortID abbreviates a container id the way the engine does.
func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// cleanWorktrees removes every worktree of the main clone that lives under
// the workspace root, then prunes stale worktree metadata and empty
// workspace directories.
func cleanWorktrees(ctx context.Context, cfg *config.Config, dryRun bool) ([]string, error) {
	root := cfg.Scheduler.WorkspaceRoot
	if root == "" {
		root = filepath.Join(os.TempDir(), "bees")
	}
	root, _ = filepath.Abs(root)
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	out, err := workspace.Git(ctx, cfg.Dir(), "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, line := range strings.Split(out, "\n") {
		path, ok := strings.CutPrefix(line, "worktree ")
		if !ok {
			continue
		}
		resolved := path
		if r, err := filepath.EvalSymlinks(path); err == nil {
			resolved = r
		}
		if !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
			continue
		}
		removed = append(removed, path)
		if dryRun {
			continue
		}
		if _, err := workspace.Git(ctx, cfg.Dir(), "worktree", "remove", "--force", path); err != nil {
			fmt.Fprintf(os.Stderr, "warning: %v\n", err)
		}
		_ = os.RemoveAll(filepath.Dir(path)) // the temp dir holding the worktree
	}
	if !dryRun {
		_, _ = workspace.Git(ctx, cfg.Dir(), "worktree", "prune")
		if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				p := filepath.Join(root, e.Name())
				if isLeftoverWorkspace(p) {
					_ = os.RemoveAll(p) // leftover empty workspace dir
				}
			}
		}
	}
	return removed, nil
}

// isLeftoverWorkspace reports whether the directory p, a child of the
// workspace root, still holds a worktree. The worktree's leaf name is the
// workspace's own (see workspace.Manager.prepare), not a fixed "repo", so the
// test cannot assume a name: p is leftover when none of its entries is a
// directory containing a .git entry.
func isLeftoverWorkspace(p string) bool {
	entries, err := os.ReadDir(p)
	if err != nil {
		return false // not a directory we can read: leave it alone
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(p, e.Name(), ".git")); err == nil {
			return false
		}
	}
	return true
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
