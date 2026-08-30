package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/prompts"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/versions"
	"github.com/kpenfound/busybees/internal/workspace"
)

// ---- init ------------------------------------------------------------------

// initOptions are the inputs of `bees init`, flags plus the directory it runs
// in, so runInit can be exercised without cobra.
type initOptions struct {
	dir        string // directory to initialise; must be a git clone
	configPath string // explicit --config path, empty for <dir>/bees.toml
	repo       string
	remote     string
	label      string
	assignee   string
	print      bool
	noLabels   bool
}

// initDeps are the parts of init that talk to `gh`, so tests can replace them.
type initDeps struct {
	checkGH     func(ctx context.Context) error
	currentRepo func(ctx context.Context, dir string) (string, error)
	repoBranch  func(ctx context.Context, repo string) (string, error)
	syncLabels  func(ctx context.Context, cfg *config.Config) error
}

func defaultInitDeps() initDeps {
	return initDeps{
		checkGH:     versions.CheckGH,
		currentRepo: github.CurrentRepo,
		repoBranch:  func(ctx context.Context, repo string) (string, error) { return github.New(repo).DefaultBranch(ctx) },
		syncLabels:  syncLabels,
	}
}

func newInitCmd(g *globalFlags) *cobra.Command {
	var o initOptions
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create bees.toml, the state directory and the GitHub labels",
		Long: `init writes a commented bees.toml in the current directory (which must be a
git clone of the project), creates the state directory and creates the
workflow labels in the GitHub repository.

Everything is validated before anything is written: when init fails it leaves
the directory exactly as it found it.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			o.dir, o.configPath = cwd, g.config
			return runInit(cmd.Context(), o, defaultInitDeps())
		},
	}
	cmd.Flags().StringVar(&o.repo, "repo", "", "GitHub repository owner/name (default: derived from the remote at run time)")
	cmd.Flags().StringVar(&o.remote, "remote", config.DefaultRemote, "git remote the factory pushes to")
	cmd.Flags().StringVar(&o.label, "label", config.DefaultLabel, "visibility label")
	cmd.Flags().StringVar(&o.assignee, "assignee", "", "only see issues assigned to this login (\"@me\" for yourself)")
	cmd.Flags().BoolVar(&o.print, "print", false, "print the template instead of writing it")
	cmd.Flags().BoolVar(&o.noLabels, "no-labels", false, "do not create GitHub labels")
	return cmd
}

// runInit validates first and writes second: nothing touches disk until the
// rendered template has parsed and resolved, so a failed init leaves no
// half-initialised directory behind (#41).
func runInit(ctx context.Context, o initOptions, d initDeps) error {
	if err := d.checkGH(ctx); err != nil {
		return err
	}
	// render detects what it can so the placeholders in the template are
	// right; the values are only written as active settings when --repo was
	// given explicitly.
	render := func() (string, error) {
		repo := o.repo
		if repo == "" {
			if url, err := workspace.Git(ctx, o.dir, "remote", "get-url", o.remote); err == nil {
				repo, _ = config.ParseGitHubRepo(url)
			}
			if repo == "" {
				if r, err := d.currentRepo(ctx, o.dir); err == nil {
					repo = r
				}
			}
		}
		branch, _ := workspace.DefaultBranch(ctx, o.dir, o.remote)
		if branch == "" && repo != "" {
			branch, _ = d.repoBranch(ctx, repo)
		}
		return config.Template(config.TemplateData{Remote: o.remote, Repo: repo, DefaultBranch: branch, Label: o.label, Assignee: o.assignee, Explicit: o.repo != ""})
	}

	// --print writes nothing, so it does not need a git clone: it is how the
	// example config is generated.
	if o.print {
		text, err := render()
		if err != nil {
			return err
		}
		fmt.Print(text)
		return nil
	}
	if _, err := workspace.Git(ctx, o.dir, "rev-parse", "--show-toplevel"); err != nil {
		return fmt.Errorf("bees init must run inside a git clone of the project (%v)", err)
	}
	path := filepath.Join(o.dir, "bees.toml")
	if o.configPath != "" {
		path = o.configPath
	}
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%s already exists", path)
	}
	text, err := render()
	if err != nil {
		return err
	}
	cfg, err := config.Parse(text, path)
	if err != nil {
		return err
	}
	if err := cfg.Resolve(ctx); err != nil {
		return fmt.Errorf("%w (pass --repo owner/name, or set project.repo / project.default_branch after creating bees.toml with bees init --print)", err)
	}

	// Validated: from here on the writes happen.
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", path)
	if err := state.New(cfg.StateDir()).Init(); err != nil {
		return err
	}
	fmt.Println("created", cfg.StateDir())
	ignoreStateDir(ctx, cfg)
	if o.noLabels {
		return nil
	}
	// The local setup is complete and correct at this point, so a label
	// failure is not worth undoing; say how to finish rather than let the
	// user reach for init again, which would now refuse.
	if err := d.syncLabels(ctx, cfg); err != nil {
		return fmt.Errorf("%w (run bees labels sync to retry creating the labels)", err)
	}
	return nil
}

// ignoreStateDir makes sure the state directory is ignored by git. It is
// added to the repository's .gitignore (so every clone ignores it), not to
// the local .git/info/exclude.
func ignoreStateDir(ctx context.Context, cfg *config.Config) {
	rel, err := filepath.Rel(cfg.Dir(), cfg.StateDir())
	if err != nil || strings.HasPrefix(rel, "..") {
		return // outside the clone: nothing to ignore
	}
	if _, err := workspace.Git(ctx, cfg.Dir(), "check-ignore", "-q", rel); err == nil {
		return // already ignored
	}
	gitignore := filepath.Join(cfg.Dir(), ".gitignore")
	line := "/" + filepath.ToSlash(rel) + "/"
	existing, _ := os.ReadFile(gitignore)
	f, err := os.OpenFile(gitignore, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not update .gitignore:", err)
		return
	}
	defer func() { _ = f.Close() }()
	prefix := ""
	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		prefix = "\n"
	}
	if _, err := fmt.Fprintf(f, "%s# busybees state (mail, notes, session transcripts)\n%s\n", prefix, line); err != nil {
		fmt.Fprintln(os.Stderr, "warning: could not update .gitignore:", err)
		return
	}
	fmt.Println("added", line, "to .gitignore — commit it")
}

func syncLabels(ctx context.Context, cfg *config.Config) error {
	gh := github.New(cfg.Project.Repo)
	for _, l := range cfg.Labels().All() {
		if err := gh.EnsureLabel(ctx, l.Name, l.Color, l.Description); err != nil {
			return err
		}
		fmt.Println("label", l.Name)
	}
	return nil
}

// ---- labels ----------------------------------------------------------------

func newLabelsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "labels", Short: "Manage the workflow labels in GitHub"}
	cmd.AddCommand(&cobra.Command{
		Use:   "sync",
		Short: "Create or update the workflow labels in the repository",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			return syncLabels(cmd.Context(), cfg)
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "list",
		Short: "Print the workflow labels and their meaning",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			for _, l := range cfg.Labels().All() {
				fmt.Printf("%-22s %s\n", l.Name, l.Description)
			}
			return nil
		},
	})
	return cmd
}

// ---- run / tick / exec -----------------------------------------------------

func parseRoles(list string) (map[string]bool, error) {
	if strings.TrimSpace(list) == "" {
		return nil, nil
	}
	out := map[string]bool{}
	for _, r := range strings.Split(list, ",") {
		c, err := config.CanonicalRole(r)
		if err != nil {
			return nil, err
		}
		out[c] = true
	}
	return out, nil
}

func newRunCmd(g *globalFlags) *cobra.Command {
	var once bool
	var roles string
	cmd := &cobra.Command{
		Use:   "run",
		Short: "Run the factory until interrupted",
		Long: `run polls GitHub, keeps the workflow labels consistent and dispatches
Claude Code sessions: a pool of developer workers plus the product manager,
project manager and QA singletons. Ctrl-C stops polling and waits for running
sessions to finish.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(cmd.Context(), g)
			if err != nil {
				return err
			}
			s, err := a.scheduler()
			if err != nil {
				return err
			}
			s.Once = once
			if s.OnlyRoles, err = parseRoles(roles); err != nil {
				return err
			}
			return s.Run(cmd.Context())
		},
	}
	cmd.Flags().BoolVar(&once, "once", false, "do a single pass and exit when its sessions finish")
	cmd.Flags().StringVar(&roles, "roles", "", "comma-separated roles to run (default: all enabled)")
	return cmd
}

func newTickCmd(g *globalFlags) *cobra.Command {
	var roles string
	cmd := &cobra.Command{
		Use:   "tick",
		Short: "Do a single scheduler pass (same as run --once)",
		RunE: func(cmd *cobra.Command, args []string) error {
			a, err := newApp(cmd.Context(), g)
			if err != nil {
				return err
			}
			s, err := a.scheduler()
			if err != nil {
				return err
			}
			s.Once = true
			if s.OnlyRoles, err = parseRoles(roles); err != nil {
				return err
			}
			return s.Run(cmd.Context())
		},
	}
	cmd.Flags().StringVar(&roles, "roles", "", "comma-separated roles to run (default: all enabled)")
	return cmd
}

func newExecCmd(g *globalFlags) *cobra.Command {
	var issue, pr int
	cmd := &cobra.Command{
		Use:   "exec <role>",
		Short: "Run one session for a role right now (developer/reviewer need --issue or --pr)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := config.CanonicalRole(args[0])
			if err != nil {
				return err
			}
			a, err := newApp(cmd.Context(), g)
			if err != nil {
				return err
			}
			s, err := a.scheduler()
			if err != nil {
				return err
			}
			return s.RunRole(cmd.Context(), role, issue, pr)
		},
	}
	cmd.Flags().IntVar(&issue, "issue", 0, "issue number (developer, reviewer)")
	cmd.Flags().IntVar(&pr, "pr", 0, "pull request number (reviewer)")
	return cmd
}

// ---- status ----------------------------------------------------------------

func newStatusCmd(g *globalFlags) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show queues, running workers and unread mail",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			store := state.New(cfg.StateDir())
			st, err := store.LoadStatus()
			if err != nil {
				return err
			}
			a, err := newApp(cmd.Context(), g)
			if err != nil {
				return err
			}
			counts, _ := a.mail.Counts()
			today := todayTotal(store, time.Now())
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{"status": st, "unread_mail": counts, "today": today})
			}
			fmt.Printf("repo: %s   state: %s\n", cfg.Project.Repo, cfg.StateDir())
			if st.UpdatedAt.IsZero() {
				fmt.Println("scheduler: never run")
			} else {
				fmt.Printf("scheduler: pid %d, last poll %s ago\n", st.PID, time.Since(st.LastPoll).Round(time.Second))
			}
			fmt.Println(todayText(today))
			if cfg.Scheduler.WorkHoursEnabled() {
				// Always the live answer: a stored one is stale as soon
				// as the scheduler stops. status.json keeps its own record
				// for --json.
				in := cfg.Scheduler.InWorkHours(time.Now())
				yes := "no"
				if in {
					yes = "yes"
				}
				line := fmt.Sprintf("work hours: %s (%s)", yes, cfg.Scheduler.WorkHoursDescription())
				switch d := time.Until(st.NextPoll).Round(time.Second); {
				case st.NextPoll.IsZero():
				case d > 0:
					line += fmt.Sprintf("   next GitHub poll in %s", d)
				default:
					line += "   next GitHub poll due"
				}
				fmt.Println(line)
			}
			if st.LastError != "" {
				fmt.Println("last error:", st.LastError)
			}
			fmt.Println("\nqueues:")
			fmt.Print(queuesText(st))
			fmt.Println("\ndeveloper workers:")
			if len(st.Workers) == 0 {
				fmt.Println("  none")
			}
			for _, w := range st.Workers {
				round := fmt.Sprintf("round %d", w.Round)
				if w.Attempt > 1 {
					round += fmt.Sprintf(" attempt %d", w.Attempt)
				}
				fmt.Printf("  %-12s issue #%-5d %-10s %-20s since %s\n", w.Name, w.Issue, w.Stage, round, w.Since.Format(time.Kitchen))
			}
			fmt.Println("\nsingletons:")
			for _, r := range []string{config.RoleProductManager, config.RoleProjectManager, config.RoleQA} {
				rs, _ := store.Role(r)
				last := "never"
				if !rs.LastRun.IsZero() {
					last = time.Since(rs.LastRun).Round(time.Minute).String() + " ago"
				}
				fmt.Printf("  %-16s %-8s last run %s\n", r, st.Singletons[r], last)
			}
			fmt.Println("\nunread mail:")
			if len(counts) == 0 {
				fmt.Println("  none")
			}
			for _, r := range config.Roles {
				if counts[r] > 0 {
					fmt.Printf("  %-16s %d\n", r, counts[r])
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// queuesText renders the queue counts of a status, with the ready queue
// broken down by size ("ready  4  (xs 1, s 2, m 1)").
func queuesText(st state.Status) string {
	keys := make([]string, 0, len(st.Queues))
	for k := range st.Queues {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&b, "  %-14s %d", k, st.Queues[k])
		if k == "ready" {
			if sizes := readySizesText(st.ReadySizes); sizes != "" {
				fmt.Fprintf(&b, "  (%s)", sizes)
			}
		}
		b.WriteString("\n")
	}
	return b.String()
}

// readySizesText renders the ready-queue size breakdown, smallest first;
// issues with no size label (counted under "") are reported as "unsized".
func readySizesText(sizes map[string]int) string {
	var parts []string
	for _, size := range []string{"xs", "s", "m", "l", "xl", ""} {
		n, ok := sizes[size]
		if !ok || n == 0 {
			continue
		}
		name := size
		if name == "" {
			name = "unsized"
		}
		parts = append(parts, fmt.Sprintf("%s %d", name, n))
	}
	return strings.Join(parts, ", ")
}

// ---- config / prompts ------------------------------------------------------

func newConfigCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "config", Short: "Inspect bees.toml"}
	cmd.AddCommand(&cobra.Command{
		Use:   "validate",
		Short: "Check bees.toml for errors",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			if cfg.NeedsRewrite() {
				fmt.Printf("%s is valid (version %d; run `bees config migrate` to update it to version %d)\n", cfg.Path, cfg.MigratedFrom, config.CurrentVersion)
				return nil
			}
			fmt.Println(cfg.Path, "is valid")
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "migrate",
		Short: "Rewrite bees.toml to the current format version",
		Long: `migrate loads bees.toml, applies the migrations that bring it to the format
version this bees understands and writes the result back, keeping the original
as bees.toml.v<old>.bak. Comments are preserved. bees run, tick, exec and
status do the same automatically on startup.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			if !cfg.NeedsRewrite() {
				fmt.Printf("%s is already version %d\n", cfg.Path, config.CurrentVersion)
				return nil
			}
			from := cfg.MigratedFrom
			backup, err := cfg.Rewrite()
			if err != nil {
				return err
			}
			fmt.Printf("%s: migrated from version %d to %d (original kept as %s)\n", cfg.Path, from, config.CurrentVersion, backup)
			return nil
		},
	})
	cmd.AddCommand(&cobra.Command{
		Use:   "show [role]",
		Short: "Print the resolved settings for every role (or one)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			roles := config.Roles
			if len(args) == 1 {
				r, err := config.CanonicalRole(args[0])
				if err != nil {
					return err
				}
				roles = []string{r}
			}
			out := map[string]any{"project": cfg.Project, "filter": cfg.Filter, "scheduler": cfg.Scheduler, "roles": map[string]any{}}
			for _, r := range roles {
				rr, err := cfg.Role(r)
				if err != nil {
					return err
				}
				out["roles"].(map[string]any)[r] = rr
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(out)
		},
	})
	return cmd
}

func newPromptsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{Use: "prompts", Short: "Inspect role prompts"}
	var rendered bool
	show := &cobra.Command{
		Use:   "show <role>",
		Short: "Print a role's base prompt (or the full rendered system prompt with --rendered)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			role, err := config.CanonicalRole(args[0])
			if err != nil {
				return err
			}
			if !rendered {
				text, err := prompts.BaseSystemPrompt(role)
				if err != nil {
					return err
				}
				fmt.Print(text)
				return nil
			}
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			rr, err := cfg.Role(role)
			if err != nil {
				return err
			}
			store := state.New(cfg.StateDir())
			d := prompts.Data{
				Project: cfg.Project, Filter: cfg.Filter, Labels: cfg.Labels(), AutoMerge: cfg.Merge().AutoMerge, CommitFlags: cfg.CommitFlags(),
				WorkDir: "<worktree>", Branch: "<branch>", StateDir: store.Dir, SessionDir: "<session dir>",
				NotesFile: store.NotesPath(role),
				Issue:     &github.Issue{Number: 1, Title: "<issue>"}, PR: &github.PR{Number: 2, Title: "<pr>"},
				Round: 1, MaxRounds: cfg.Scheduler.MaxReviewRounds,
			}
			text, err := prompts.System(role, d, rr.Prompt)
			if err != nil {
				return err
			}
			fmt.Print(text)
			return nil
		},
	}
	show.Flags().BoolVar(&rendered, "rendered", false, "render the full system prompt with this project's settings")
	cmd.AddCommand(show)
	return cmd
}

// readBody reads a message body from --body, --body-file or stdin.
func readBody(body, file string) (string, error) {
	switch {
	case file == "-":
		var sb strings.Builder
		sc := bufio.NewScanner(os.Stdin)
		sc.Buffer(make([]byte, 0, 1024*1024), 16*1024*1024)
		for sc.Scan() {
			sb.WriteString(sc.Text())
			sb.WriteString("\n")
		}
		return sb.String(), sc.Err()
	case file != "":
		b, err := os.ReadFile(file)
		return string(b), err
	default:
		return body, nil
	}
}
