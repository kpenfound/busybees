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
	// GroupRoles holds the per-role checks: the skills, MCP servers and shell
	// of every role in bees.toml. They are the expensive ones (a skill is
	// cloned, an MCP server is started), so `bees run`'s preflight skips them.
	GroupRoles = "roles"
	// GroupInternal holds the results the runner itself produces when a check
	// panics or overruns its timeout: a bug in bees rather than in the setup.
	GroupInternal = "doctor"
)

// Groups is the order the groups are printed in.
var Groups = []string{GroupToolchain, GroupConfig, GroupGitHub, GroupWorkspace, GroupRoles}

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

// Check inspects one thing and reports what it found, and may know how to
// repair what it finds.
type Check struct {
	// Run inspects. It must honour ctx, which the runner cancels after Timeout.
	Run func(ctx context.Context) Result
	// Fix repairs what Run found, or is nil when doctor cannot repair it.
	// `bees doctor --fix` runs it only for a check that did not pass.
	Fix Fix
	// Expensive marks a check that clones a repository or starts a process.
	// `bees doctor` and `bees init` run those too; the `bees run` preflight
	// does not, because it must not add a minute to every start.
	Expensive bool
	// Timeout raises the runner's per-check budget for a check that legitimately
	// needs longer than Timeout (an MCP server has MCPTimeout to answer). Zero
	// uses the runner's budget; a smaller value never shortens it.
	Timeout time.Duration
}

// CheapChecks returns the checks that are not marked expensive, in order.
// It is the subset `bees run` runs before it starts the scheduler.
func CheapChecks(checks []Check) []Check {
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		if !c.Expensive {
			out = append(out, c)
		}
	}
	return out
}

// Fix repairs what a check found. It returns one line per thing it did (or
// deliberately did not do, so a fix that declines can say why), and an error
// for what it could not do.
//
// A fix never stops at the first failure: it attempts every item and joins
// the per-item errors, so one unassignable issue does not strand the rest.
type Fix func(ctx context.Context) (actions []string, err error)

// FixOutcome is what one check's fix did.
type FixOutcome struct {
	// Check is the name of the check the fix belongs to.
	Check string
	// Actions is one line per thing the fix did or declined to do.
	Actions []string
	// Err is what the fix could not do; nil when everything worked.
	Err error
}

// ApplyFixes runs the fixes of the checks that did not pass, in check order,
// and reports what each one did. results must be the results Run returned for
// the same checks, in the same order; a check that passed, or that carries no
// fix, is left alone.
//
// A fix that fails does not stop the ones after it: `bees doctor --fix` is a
// repair pass, and a repository where one item cannot be assigned still wants
// the others repaired.
func ApplyFixes(ctx context.Context, checks []Check, results []Result) []FixOutcome {
	var out []FixOutcome
	for i, c := range checks {
		if c.Fix == nil || i >= len(results) || results[i].Status == Pass {
			continue
		}
		actions, err := c.Fix(ctx)
		out = append(out, FixOutcome{Check: results[i].Name, Actions: actions, Err: err})
	}
	return out
}

// FixText renders what the fixes did: one section per check, one line per
// action, and the failures marked with "!".
func FixText(outcomes []FixOutcome) string {
	var b strings.Builder
	for _, o := range outcomes {
		fmt.Fprintf(&b, "fixing %s\n", o.Check)
		for _, a := range o.Actions {
			fmt.Fprintf(&b, "  %s\n", a)
		}
		for _, e := range flatten(o.Err) {
			fmt.Fprintf(&b, "  ! %s\n", oneLine(e.Error()))
		}
	}
	if len(outcomes) > 0 {
		b.WriteString("\n")
	}
	return b.String()
}

// flatten splits an errors.Join back into its parts, so a fix that failed on
// three items prints three lines instead of one paragraph.
func flatten(err error) []error {
	if err == nil {
		return nil
	}
	if j, ok := err.(interface{ Unwrap() []error }); ok {
		var out []error
		for _, e := range j.Unwrap() {
			out = append(out, flatten(e)...)
		}
		return out
	}
	return []error{err}
}

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
	if c.Timeout > timeout {
		timeout = c.Timeout
	}
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
		done <- c.Run(cctx)
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
