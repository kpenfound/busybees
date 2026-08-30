package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/text"
	"github.com/spf13/cobra"
)

// The dimensions `bees cost --by` can group the ledger by.
const (
	byRole  = "role"
	byIssue = "issue"
	byDay   = "day"
)

// noGroup stands in for entries that have nothing to group by: a session
// with no issue under `--by issue`.
const noGroup = "-"

// costGroup is one row of a cost report.
type costGroup struct {
	Group    string  `json:"group"`
	Sessions int     `json:"sessions"`
	Turns    int     `json:"turns"`
	CostUSD  float64 `json:"cost_usd"`
}

func newCostCmd(g *globalFlags) *cobra.Command {
	var (
		since  time.Duration
		by     string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "cost",
		Short: "Report what finished sessions have cost",
		Long: `Sums the session ledger (<state_dir>/ledger.jsonl), which records the turns
and cost every finished session reported.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			switch by {
			case byRole, byIssue, byDay:
			default:
				return fmt.Errorf("--by must be one of role, issue or day (got %q)", by)
			}
			cfg, err := loadConfig(g)
			if err != nil {
				return err
			}
			entries, err := state.New(cfg.StateDir()).ReadLedger(time.Now().Add(-since))
			if err != nil {
				return err
			}
			groups, total := groupCost(entries, by)
			if asJSON {
				return json.NewEncoder(os.Stdout).Encode(map[string]any{
					"since":  since.String(),
					"by":     by,
					"groups": groups,
					"total":  total,
				})
			}
			if len(entries) == 0 {
				fmt.Println("no sessions recorded")
				return nil
			}
			fmt.Print(costText(by, groups, total))
			return nil
		},
	}
	cmd.Flags().DurationVar(&since, "since", 24*time.Hour, "how far back to look (Go duration)")
	cmd.Flags().StringVar(&by, "by", byRole, "group by role, issue or day")
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON")
	return cmd
}

// groupCost sums ledger entries by one dimension. The groups are ordered
// for printing: issues numerically, days and roles alphabetically, with the
// ungrouped bucket last.
func groupCost(entries []state.LedgerEntry, by string) ([]costGroup, costGroup) {
	index := map[string]int{}
	var groups []costGroup
	var total costGroup
	for _, e := range entries {
		key := costKey(e, by)
		i, ok := index[key]
		if !ok {
			i = len(groups)
			index[key] = i
			groups = append(groups, costGroup{Group: key})
		}
		groups[i].Sessions++
		groups[i].Turns += e.Turns
		groups[i].CostUSD += e.CostUSD
		total.Sessions++
		total.Turns += e.Turns
		total.CostUSD += e.CostUSD
	}
	total.Group = "total"
	sort.Slice(groups, func(i, j int) bool {
		a, b := groups[i].Group, groups[j].Group
		if a == noGroup || b == noGroup {
			return b == noGroup && a != noGroup
		}
		if by == byIssue {
			return atoi(a) < atoi(b)
		}
		return a < b
	})
	return groups, total
}

// costKey is the group an entry belongs to.
func costKey(e state.LedgerEntry, by string) string {
	switch by {
	case byIssue:
		if e.Issue == 0 {
			return noGroup
		}
		return strconv.Itoa(e.Issue)
	case byDay:
		return e.Time.Local().Format("2006-01-02")
	default:
		if e.Role == "" {
			return noGroup
		}
		return e.Role
	}
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// costText renders the report as a table with a total row.
func costText(by string, groups []costGroup, total costGroup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s %8s %8s %10s\n", by, "sessions", "turns", "cost")
	for _, g := range groups {
		b.WriteString(costRow(g))
	}
	b.WriteString(costRow(total))
	return b.String()
}

func costRow(g costGroup) string {
	return fmt.Sprintf("%-16s %8d %8d %10s\n", g.Group, g.Sessions, g.Turns, fmt.Sprintf("$%.2f", g.CostUSD))
}

// todayTotal sums whatever the ledger recorded since the start of the
// current local day.
func todayTotal(store *state.Store, now time.Time) costGroup {
	entries, err := store.ReadLedger(startOfDay(now))
	if err != nil {
		return costGroup{Group: "today"}
	}
	_, total := groupCost(entries, byRole)
	total.Group = "today"
	return total
}

func startOfDay(t time.Time) time.Time {
	t = t.Local()
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// todayText is the `bees status` line summarising the day so far.
func todayText(total costGroup) string {
	return fmt.Sprintf("today: %s, %s, $%.2f",
		text.Count(total.Sessions, "session"), text.Count(total.Turns, "turn"), total.CostUSD)
}
