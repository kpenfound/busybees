package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/skills"
)

// cacheDir is where skill clones and generated plugins live.
func cacheDir() string {
	if d := os.Getenv("BEES_CACHE_DIR"); d != "" {
		return d
	}
	return skills.DefaultCacheDir()
}

// skillRef is one configured skill reference and the roles that use it.
type skillRef struct {
	Ref   string
	Roles []string
}

// skillRefs returns the union of the skill references of every enabled role,
// in the order they first appear.
func skillRefs(cfg *config.Config) ([]skillRef, error) {
	var refs []skillRef
	at := map[string]int{}
	for _, name := range config.Roles {
		r, err := cfg.Role(name)
		if err != nil {
			return nil, err
		}
		if !r.Enabled {
			continue
		}
		for _, ref := range r.Skills {
			i, ok := at[ref]
			if !ok {
				at[ref] = len(refs)
				refs = append(refs, skillRef{Ref: ref})
				i = len(refs) - 1
			}
			refs[i].Roles = append(refs[i].Roles, name)
		}
	}
	return refs, nil
}

// skillRow is one line of `bees skills list`.
type skillRow struct {
	Commit string // short commit, or "not cached"
	Age    string // age of the last fetch
	Roles  string // comma-separated role names
	Ref    string
}

// listText renders the header and one aligned line per reference.
func listText(dir, policy string, rows []skillRow) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s  (refresh: %s)\n", dir, policy)
	if len(rows) == 0 {
		b.WriteString("no skills configured\n")
		return b.String()
	}
	var w [3]int
	cols := func(r skillRow) [3]string { return [3]string{r.Commit, r.Age, r.Roles} }
	for _, r := range rows {
		for i, c := range cols(r) {
			w[i] = max(w[i], len(c))
		}
	}
	for _, r := range rows {
		c := cols(r)
		fmt.Fprintf(&b, "%-*s  %-*s  %-*s  %s\n", w[0], c[0], w[1], c[1], w[2], c[2], r.Ref)
	}
	return b.String()
}

// humanAge renders how long ago something was fetched.
func humanAge(now, then time.Time) string {
	if then.IsZero() {
		return "unknown"
	}
	d := now.Sub(then)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// updateLine reports the result of updating one reference.
func updateLine(ref, before, after string, err error) string {
	switch {
	case err != nil:
		return fmt.Sprintf("failed %s: %v", ref, err)
	case before == "":
		return fmt.Sprintf("cloned %s %s", ref, after)
	case before == after:
		return fmt.Sprintf("unchanged %s %s", ref, before)
	default:
		return fmt.Sprintf("updated %s %s → %s", ref, before, after)
	}
}

func newSkillsCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "skills",
		Short: "Inspect and update the skill clones in the cache",
		Long: `skills works on the git repositories listed in the skills setting of
[global] and [roles.<name>]. They are cloned into the cache directory
(BEES_CACHE_DIR) and refreshed according to global.skills_refresh when a
session needs them. Neither subcommand runs a session or talks to GitHub.`,
	}
	cmd.AddCommand(newSkillsListCmd(g), newSkillsUpdateCmd(g))
	return cmd
}

func newSkillsListCmd(g *globalFlags) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "Print the configured skills, their commit and their age",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, m, refs, err := skillsSetup(g)
			if err != nil {
				return err
			}
			now := time.Now()
			rows := make([]skillRow, 0, len(refs))
			for _, r := range refs {
				spec, err := skills.Parse(r.Ref)
				if err != nil {
					return err
				}
				info, err := m.Info(cmd.Context(), spec)
				if err != nil {
					return err
				}
				row := skillRow{Commit: "not cached", Age: "-", Roles: strings.Join(r.Roles, ","), Ref: r.Ref}
				if info.Cached {
					row.Commit = firstNonEmptyStr(info.Commit, "unknown")
					row.Age = humanAge(now, info.FetchedAt)
				}
				rows = append(rows, row)
			}
			fmt.Print(listText(m.CacheDir, cfg.SkillsRefreshPolicy(), rows))
			return nil
		},
	}
}

func newSkillsUpdateCmd(g *globalFlags) *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "update [--all | <url>...]",
		Short: "Clone or pull the configured skills now",
		Long: `update pulls every configured skill (or the ones given, which must match a
configured reference verbatim), whatever global.skills_refresh says. Missing
clones are created.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			_, m, refs, err := skillsSetup(g)
			if err != nil {
				return err
			}
			if len(args) > 0 && all {
				return fmt.Errorf("--all takes no arguments")
			}
			if len(args) > 0 {
				refs, err = selectRefs(refs, args)
				if err != nil {
					return err
				}
			}
			if len(refs) == 0 {
				fmt.Println("no skills configured")
				return nil
			}
			failed := 0
			for _, r := range refs {
				spec, err := skills.Parse(r.Ref)
				if err != nil {
					return err
				}
				before, after, err := m.Update(cmd.Context(), spec)
				if err != nil {
					failed++
				}
				fmt.Println(updateLine(r.Ref, before, after, err))
			}
			if failed > 0 {
				return fmt.Errorf("%d of %d skills failed to update", failed, len(refs))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "update every configured skill (the default when no argument is given)")
	return cmd
}

// selectRefs keeps the configured references named in args, in the order of
// args, and errors on anything that is not configured.
func selectRefs(refs []skillRef, args []string) ([]skillRef, error) {
	byRef := map[string]skillRef{}
	known := make([]string, 0, len(refs))
	for _, r := range refs {
		byRef[r.Ref] = r
		known = append(known, r.Ref)
	}
	sort.Strings(known)
	var out []skillRef
	for _, a := range args {
		r, ok := byRef[strings.TrimSpace(a)]
		if !ok {
			if len(known) == 0 {
				return nil, fmt.Errorf("%q is not a configured skill: no skills are configured", a)
			}
			return nil, fmt.Errorf("%q is not a configured skill (configured: %s)", a, strings.Join(known, ", "))
		}
		out = append(out, r)
	}
	return out, nil
}

// skillsSetup loads bees.toml, the skill manager and the configured references.
func skillsSetup(g *globalFlags) (*config.Config, *skills.Manager, []skillRef, error) {
	cfg, err := loadConfig(g)
	if err != nil {
		return nil, nil, nil, err
	}
	refs, err := skillRefs(cfg)
	if err != nil {
		return nil, nil, nil, err
	}
	return cfg, skills.NewManager(cacheDir()), refs, nil
}

func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
