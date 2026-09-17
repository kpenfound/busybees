package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/text"
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

// costGroup is one row of a cost report. Unknown is how many of its
// sessions reported no cost: they are in Sessions and Turns, and CostUSD is
// what the rest cost, so a row with Unknown > 0 is short by what nobody
// knows.
type costGroup struct {
	Group    string  `json:"group"`
	Sessions int     `json:"sessions"`
	Turns    int     `json:"turns"`
	CostUSD  float64 `json:"cost_usd"`
	Unknown  int     `json:"unknown,omitempty"`
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
		total.Sessions++
		total.Turns += e.Turns
		if e.CostUnknown {
			groups[i].Unknown++
			total.Unknown++
			continue
		}
		groups[i].CostUSD += e.CostUSD
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
		if ghwork.Issue(e.Work) == 0 {
			return noGroup
		}
		return strconv.Itoa(ghwork.Issue(e.Work))
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

// costText renders the report as a table with a total row. A session that
// reported no cost is never shown as $0.00: a row of nothing else reads
// `unknown` in the cost column, a row that mixes them marks its known sum
// with a `+`, and a line under the total says how many there were.
func costText(by string, groups []costGroup, total costGroup) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%-16s %8s %8s %10s\n", by, "sessions", "turns", "cost")
	for _, g := range groups {
		b.WriteString(costRow(g))
	}
	b.WriteString(costRow(total))
	if total.Unknown > 0 {
		fmt.Fprintf(&b, "%s reported no cost (+): not in the totals\n", text.Count(total.Unknown, "session"))
	}
	return b.String()
}

func costRow(g costGroup) string {
	return fmt.Sprintf("%-16s %8d %8d %10s\n", g.Group, g.Sessions, g.Turns, costCell(g))
}

// costCell is the cost column of one row.
func costCell(g costGroup) string {
	switch g.Unknown {
	case 0:
		return fmt.Sprintf("$%.2f", g.CostUSD)
	case g.Sessions:
		return "unknown"
	default:
		return fmt.Sprintf("$%.2f+", g.CostUSD)
	}
}

// todayTotal sums whatever the ledger recorded since the start of the
// current local day. A ledger that cannot be read is the error, with an
// empty total beside it: the line and the JSON report the error rather than
// a day that cost nothing.
func todayTotal(store *state.Store, now time.Time) (costGroup, error) {
	entries, err := store.ReadLedger(startOfDay(now))
	if err != nil {
		return costGroup{Group: "today"}, err
	}
	_, total := groupCost(entries, byRole)
	total.Group = "today"
	return total, nil
}

// todayReport is the `today` object of `bees status --json`: the total, or
// why there is none.
type todayReport struct {
	costGroup
	Error string `json:"error,omitempty"`
}

func todayJSON(total costGroup, err error) todayReport {
	r := todayReport{costGroup: total}
	if err != nil {
		r.Error = err.Error()
	}
	return r
}

func startOfDay(t time.Time) time.Time {
	t = t.Local()
	y, m, d := t.Date()
	return time.Date(y, m, d, 0, 0, 0, 0, t.Location())
}

// todayText is the `bees status` line summarising the day so far. Sessions
// that reported no cost are counted and named, and not in the dollars. A
// ledger that could not be read is named instead of a total.
func todayText(total costGroup, err error) string {
	if err != nil {
		return "today: ledger unreadable: " + err.Error()
	}
	line := fmt.Sprintf("today: %s, %s, $%.2f",
		text.Count(total.Sessions, "session"), text.Count(total.Turns, "turn"), total.CostUSD)
	if total.Unknown > 0 {
		line += fmt.Sprintf(" (%s of unknown cost)", text.Count(total.Unknown, "session"))
	}
	return line
}
