package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"
)

// ReportFile is the name of a run's JSON report in its directory.
const ReportFile = "report.json"

// Report is one `bees eval` run: what it ran with and how each case did.
type Report struct {
	Started time.Time `json:"started"`
	Dir     string    `json:"dir"`
	// Role is the role a per-role run ran in isolation, and "" for a
	// whole-factory run.
	Role    string       `json:"role,omitempty"`
	Profile Selection    `json:"profile"`
	Cases   []CaseResult `json:"cases"`
}

// CaseResult is how one case did.
type CaseResult struct {
	Case string `json:"case"`
	// Role is the role a per-role case ran in isolation.
	Role string `json:"role,omitempty"`
	// Pass is true when every check passed.
	Pass bool `json:"pass"`
	// Score is the mean of the case's graded checks, and nil when it
	// declared none.
	Score *float64 `json:"score,omitempty"`
	// Stop is why the run stopped: StopDone, StopTimeout, ...
	Stop   string  `json:"stop"`
	Error  string  `json:"error,omitempty"`
	Checks []Check `json:"checks"`
	// CostUSD is what the sessions reported costing; CostUnknown counts
	// the sessions that reported no cost, which it leaves out.
	CostUSD         float64 `json:"cost_usd"`
	CostUnknown     int     `json:"cost_unknown_sessions,omitempty"`
	Turns           int     `json:"turns"`
	Sessions        int     `json:"sessions"`
	DurationSeconds float64 `json:"duration_seconds"`
	Profile         string  `json:"profile"`
	// Dir holds the case's fixture, state and test output.
	Dir string `json:"dir"`
}

// Check is one mechanical check of a case, and where to look when it
// failed.
type Check struct {
	// Name is the requirement the check stands for.
	Name string `json:"name"`
	// Failure says what was observed instead, and is what a failing
	// check is reported as. A check with no Failure is reported by its
	// Name, which then has to read as a failure on its own.
	Failure string `json:"failure,omitempty"`
	Pass    bool   `json:"pass"`
	Detail  string `json:"detail,omitempty"`
	// Score is what a grader session scored the check, between 0 and 1,
	// and nil for a mechanical check. Detail holds the grader's reasons.
	Score *float64 `json:"score,omitempty"`
}

// meanScore is a case's score: the mean of its graded checks, and nil when
// it declared none.
func meanScore(checks []Check) *float64 {
	sum, n := 0.0, 0
	for _, c := range checks {
		if c.Score != nil {
			sum += *c.Score
			n++
		}
	}
	if n == 0 {
		return nil
	}
	mean := sum / float64(n)
	return &mean
}

// scoreText is a score as the table shows it, and "-" for none.
func scoreText(score *float64) string {
	if score == nil {
		return "-"
	}
	return fmt.Sprintf("%.2f", *score)
}

// Failed is the line a failing check is reported as.
func (c Check) Failed() string {
	if c.Failure != "" {
		return c.Failure
	}
	return c.Name
}

// Pass reports whether every case passed.
func (r *Report) Pass() bool {
	for _, c := range r.Cases {
		if !c.Pass {
			return false
		}
	}
	return len(r.Cases) > 0
}

// Path is where Write puts the report.
func (r *Report) Path() string { return filepath.Join(r.Dir, ReportFile) }

// Write writes the report into its directory as JSON.
func (r *Report) Write() error {
	if err := os.MkdirAll(r.Dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.Path(), append(b, '\n'), 0o644)
}

// Table is the report as a person reads it: one row per case, then what
// failed.
func (r *Report) Table() string {
	var b strings.Builder
	w := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "CASE\tRESULT\tSCORE\tSTOP\tCOST\tTURNS\tDURATION\tPROFILE")
	for _, c := range r.Cases {
		result := "pass"
		if !c.Pass {
			result = "FAIL"
		}
		cost := fmt.Sprintf("$%.2f", c.CostUSD)
		if c.CostUnknown > 0 {
			cost += "+?"
		}
		d := time.Duration(c.DurationSeconds * float64(time.Second)).Round(time.Second)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%d\t%s\t%s\n", c.Case, result, scoreText(c.Score), c.Stop, cost, c.Turns, d, c.Profile)
	}
	_ = w.Flush()
	for _, c := range r.Cases {
		if c.Error != "" {
			fmt.Fprintf(&b, "\n%s: %s", c.Case, c.Error)
		}
		for _, check := range c.Checks {
			if check.Pass {
				continue
			}
			fmt.Fprintf(&b, "\n%s: %s", c.Case, check.Failed())
			if check.Detail != "" {
				fmt.Fprintf(&b, " (%s)", check.Detail)
			}
		}
	}
	if b.String()[b.Len()-1] != '\n' {
		b.WriteString("\n")
	}
	return b.String()
}
