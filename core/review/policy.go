package review

import (
	"fmt"
	"strings"
)

// Comparator reports whether two title/body pairs describe the same problem.
// It is also used for reviewer-note text matching. Nil disables text matching;
// anchored findings with overlapping lines and the same category still merge.
type Comparator func(title, body, otherTitle, otherBody string) bool

const (
	RuleDrop     = "drop"
	RuleDownrank = "downrank"
	anyValue     = "*"
)

func count(n int, noun string) string {
	if n != 1 {
		noun += "s"
	}
	return fmt.Sprintf("%d %s", n, noun)
}

// Rule is a dismissal pattern: what a review does with a finding that a
// person has said no to before.
type Rule struct {
	// Scope, Angle and Category are what the finding must be to match, each
	// one anyValue or empty for "whatever it is".
	Scope    string
	Angle    string
	Category string
	// Action is RuleDrop or RuleDownrank.
	Action string
	// Text is the dismissal reason the rule was made from, which a finding's
	// title is compared against. A rule with no text matches every finding
	// its repo, angle and category match: that is how a whole category is
	// silenced.
	Text string
	// Count is how many dismissals the rule was made from, refreshed by
	// every consolidation. It says how sure a person was, and nothing reads
	// it back.
	Count int
}

// Line is the rule as it is written in the file.
func (r Rule) Line() string {
	line := fmt.Sprintf("- [%s] [%s] [%s] %s: %s", or(r.Scope, anyValue), or(r.Angle, anyValue), or(r.Category, anyValue), r.Action, strings.TrimSpace(r.Text))
	if r.Count > 0 {
		line += fmt.Sprintf(" (%s)", count(r.Count, "dismissal"))
	}
	return line
}

// matchesPattern reports whether a rule's field matches a finding's value.
// An empty field and anyValue match anything; the rest match whatever the
// case.
func matchesPattern(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	return pattern == "" || pattern == anyValue || strings.EqualFold(pattern, strings.TrimSpace(value))
}

// or is s, or fallback when s is empty.
func or(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
