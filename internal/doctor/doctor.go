// Package doctor checks that the machine, the configuration and the GitHub
// repository are in a state the factory can actually run in, before a session
// discovers it the hard way.
//
// A check is a value (see Check), so the same set can be run by `bees doctor`,
// printed by `bees status` or driven by a future TUI. Checks never panic and
// never block: the runner (Run) gives each one Timeout to finish and turns a
// panic or an overrun into a failing result.
package doctor

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Status is the outcome of one check. Only Fail changes the exit code;
// Warn means "this will probably bite you" and is worth printing.
type Status int

// Check outcomes.
const (
	Pass Status = iota
	Warn
	Fail
)

var statusNames = [...]string{Pass: "pass", Warn: "warn", Fail: "fail"}

func (s Status) String() string {
	if int(s) < 0 || int(s) >= len(statusNames) {
		return "unknown"
	}
	return statusNames[s]
}

// Mark is the character the table prints in front of a check.
func (s Status) Mark() string {
	switch s {
	case Pass:
		return "✓"
	case Warn:
		return "!"
	case Fail:
		return "✗"
	default:
		return "?"
	}
}

// MarshalText renders the status as its name, so --json is readable.
func (s Status) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

// UnmarshalText parses a status name, so --json output round-trips.
func (s *Status) UnmarshalText(text []byte) error {
	for i, name := range statusNames {
		if name == string(text) {
			*s = Status(i)
			return nil
		}
	}
	return fmt.Errorf("unknown check status %q", text)
}

// Check groups.
const (
	GroupToolchain = "toolchain"
	GroupConfig    = "config"
	GroupGitHub    = "github"
	GroupWorkspace = "workspace"
	// GroupInternal holds the results the runner itself produces when a check
	// panics or overruns its timeout: a bug in bees rather than in the setup.
	GroupInternal = "doctor"
)

// Groups is the order the groups are printed in.
var Groups = []string{GroupToolchain, GroupConfig, GroupGitHub, GroupWorkspace}

// Result is what one check found.
type Result struct {
	Name   string `json:"name"`
	Group  string `json:"group"`
	Status Status `json:"status"`
	// Detail is what was found ("gh 2.69.0, scopes: repo, workflow").
	Detail string `json:"detail"`
	// Remediation is what to do about it. Printed only for Warn and Fail,
	// where it is never empty.
	Remediation string `json:"remediation,omitempty"`
}

// Check inspects one thing and reports what it found. It must honour ctx,
// which the runner cancels after Timeout.
type Check func(ctx context.Context) Result

// Timeout is the budget the runner gives a single check.
const Timeout = 10 * time.Second

// graceFactor sets how much longer the runner waits for a check that has not
// returned after its context was cancelled: Timeout/graceFactor.
const graceFactor = 5

func pass(name, group, detail string) Result {
	return Result{Name: name, Group: group, Status: Pass, Detail: detail}
}

func warn(name, group, detail, remediation string) Result {
	return Result{Name: name, Group: group, Status: Warn, Detail: detail, Remediation: remediation}
}

func fail(name, group, detail, remediation string) Result {
	return Result{Name: name, Group: group, Status: Fail, Detail: detail, Remediation: remediation}
}

// Run executes checks in order and returns one result each. A check that
// panics or ignores its cancelled context is reported as a failure instead of
// taking doctor down with it.
func Run(ctx context.Context, checks []Check) []Result {
	return RunWith(ctx, checks, Timeout)
}

// RunWith is Run with a different per-check budget (tests).
func RunWith(ctx context.Context, checks []Check, timeout time.Duration) []Result {
	out := make([]Result, 0, len(checks))
	for i, c := range checks {
		out = append(out, runOne(ctx, i, c, timeout))
	}
	return out
}

func runOne(ctx context.Context, i int, c Check, timeout time.Duration) Result {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	done := make(chan Result, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				done <- fail(fmt.Sprintf("check %d", i+1), GroupInternal, fmt.Sprintf("panicked: %v", r),
					"this is a bug in bees; please report it with the output of `bees doctor --json`")
			}
		}()
		done <- c(cctx)
	}()
	select {
	case r := <-done:
		return r
	case <-cctx.Done():
	}
	// The check was cancelled; give it a moment to notice and report.
	select {
	case r := <-done:
		return r
	case <-time.After(timeout / graceFactor):
		return fail(fmt.Sprintf("check %d", i+1), GroupInternal, fmt.Sprintf("did not finish within %s", timeout),
			"re-run `bees doctor`; if it keeps hanging, report it as a bug")
	}
}

// Failures returns the number of checks that failed. `bees doctor` exits 1
// when it is not zero; warnings do not change the exit code.
func Failures(results []Result) int {
	n := 0
	for _, r := range results {
		if r.Status == Fail {
			n++
		}
	}
	return n
}

// Text renders the results as the table `bees doctor` prints: one section per
// group, a mark, the check name and what it found, with the remediation on
// the next line for everything that is not a pass.
func Text(results []Result) string {
	width := 0
	for _, r := range results {
		if len(r.Name) > width {
			width = len(r.Name)
		}
	}
	var b strings.Builder
	for i, g := range groupOrder(results) {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(g + "\n")
		for _, r := range results {
			if r.Group != g {
				continue
			}
			fmt.Fprintf(&b, "  %s %-*s  %s\n", r.Status.Mark(), width, r.Name, r.Detail)
			if r.Status != Pass && r.Remediation != "" {
				fmt.Fprintf(&b, "      → %s\n", r.Remediation)
			}
		}
	}
	if len(results) > 0 {
		b.WriteString("\n" + Summary(results) + "\n")
	}
	return b.String()
}

// Summary is the closing line: how many checks ran and how they went.
func Summary(results []Result) string {
	var passed, warned, failed int
	for _, r := range results {
		switch r.Status {
		case Warn:
			warned++
		case Fail:
			failed++
		default:
			passed++
		}
	}
	return fmt.Sprintf("%s: %d passed, %d warnings, %d failed",
		plural(len(results), "check"), passed, warned, failed)
}

func plural(n int, word string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, word)
	}
	return fmt.Sprintf("%d %ss", n, word)
}

// groupOrder returns the groups present in results: the known ones first, in
// the order of Groups, then anything else alphabetically.
func groupOrder(results []Result) []string {
	seen := map[string]bool{}
	for _, r := range results {
		seen[r.Group] = true
	}
	var out []string
	for _, g := range Groups {
		if seen[g] {
			out = append(out, g)
			delete(seen, g)
		}
	}
	var rest []string
	for g := range seen {
		rest = append(rest, g)
	}
	sort.Strings(rest)
	return append(out, rest...)
}
