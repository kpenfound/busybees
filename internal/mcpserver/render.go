package mcpserver

import (
	"fmt"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// timeFormat is how issue_view dates a comment: minutes are as precise as a
// role ever needs, and UTC keeps the output the same wherever bees runs.
const timeFormat = "2006-01-02 15:04"

// issueText renders an issue the way a role needs to read it: what the
// factory thinks it is (labels, milestone, the feature it belongs to), the
// body, then the whole conversation oldest first with every comment marked
// as a bee's or a person's. actsAs is the login the factory acts as, which
// author needs; see there.
func issueText(i github.Issue, parent *github.Parent, l config.Labels, actsAs string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s\n", i.Number, i.Title)
	if meta := issueMeta(i, l); meta != "" {
		fmt.Fprintf(&b, "%s\n", meta)
	}
	if m := i.MilestoneTitle(); m != "" {
		fmt.Fprintf(&b, "milestone: %s\n", m)
	}
	if parent != nil {
		fmt.Fprintf(&b, "parent: #%d %s\n", parent.Number, parent.Title)
	}
	fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(i.Body, "\n"))
	if len(i.Comments) == 0 {
		b.WriteString("\nno comments\n")
		return b.String()
	}
	fmt.Fprintf(&b, "\ncomments (%d):\n", len(i.Comments))
	for _, c := range i.Comments {
		fmt.Fprintf(&b, "\n%s (%s) · %s\n%s\n",
			c.Author.Login, author(actsAs, c.Author.Login, c.Body), c.CreatedAt.UTC().Format(timeFormat),
			strings.TrimRight(c.Body, "\n"))
	}
	return b.String()
}

// issueMeta is the one line that says what the factory makes of an issue:
// its workflow state, its size and what kind of issue it is.
func issueMeta(i github.Issue, l config.Labels) string {
	var parts []string
	if s := stateOf(i, l); s != "" {
		parts = append(parts, "state: "+short(s, l))
	}
	for _, name := range l.SizeLabels() {
		if github.HasLabel(i.Labels, name) {
			parts = append(parts, "size: "+strings.TrimPrefix(name, l.Base+":size/"))
		}
	}
	var kinds []string
	for _, name := range []string{l.Feature, l.Feedback, l.Bug, l.Proposal, l.Priority, l.Question} {
		if github.HasLabel(i.Labels, name) {
			kinds = append(kinds, short(name, l))
		}
	}
	if len(kinds) > 0 {
		parts = append(parts, "kind: "+strings.Join(kinds, ", "))
	}
	return strings.Join(parts, " · ")
}

// short drops the factory's label prefix ("bees:ready" -> "ready").
func short(label string, l config.Labels) string { return strings.TrimPrefix(label, l.Base+":") }

// author says who wrote a comment, by the same rule the orchestrator uses
// (github.IsBee): the marker, and — where [github] gives the factory a login
// of its own — the comment's author.
//
// A comment carrying a role's marker is that role's. github.BeeRole reads the
// marker positionally, so a marker quoted mid-body is context and only a body
// whose last line is one — where withMarker puts it — counts. A comment the
// factory's own login posted without a marker is a bee's with no role to
// name: that is the orchestrator itself, whose escalation comment carries no
// marker. Everything else is a person's, including a person quoting a marker,
// because the login only ever says yes.
//
// actsAs is empty on every configuration without [github], and then this is
// exactly the marker rule.
func author(actsAs, commentAuthor, body string) string {
	if role, ok := github.BeeRole(body); ok {
		return "bee: " + role
	}
	if github.IsBee(actsAs, commentAuthor, body) {
		return "bee"
	}
	return "human"
}

// prText renders a pull request: what it is, whether its required checks are
// green, and everything a person has said on it.
func prText(p github.PR, checks []github.Check, activity []github.Activity) string {
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s\n", p.Number, p.Title)
	fmt.Fprintf(&b, "%s → %s", p.HeadRefName, p.BaseRefName)
	if p.IsDraft {
		b.WriteString(" (draft)")
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "checks: %s\n", checksLine(checks))
	fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(p.Body, "\n"))
	if len(activity) == 0 {
		b.WriteString("\nno reviews or comments from people\n")
		return b.String()
	}
	fmt.Fprintf(&b, "\nfrom people (%d):\n", len(activity))
	for _, a := range activity {
		parts := []string{a.Kind, a.Author}
		if a.State != "" {
			parts = append(parts, strings.ToLower(a.State))
		}
		if a.Path != "" {
			parts = append(parts, fmt.Sprintf("%s:%d", a.Path, a.Line))
		}
		fmt.Fprintf(&b, "\n%s\n%s\n", strings.Join(parts, " · "), strings.TrimRight(a.Body, "\n"))
	}
	return b.String()
}

// checksLine summarises the required checks, naming the ones that failed —
// which is the part the reader has to act on.
func checksLine(checks []github.Check) string {
	if len(checks) == 0 {
		return "no required checks"
	}
	status := github.Summarize(checks)
	failed := github.Failed(checks)
	if len(failed) == 0 {
		return string(status)
	}
	names := make([]string, 0, len(failed))
	for _, c := range failed {
		names = append(names, c.Name)
	}
	return fmt.Sprintf("%s (%s)", status, strings.Join(names, ", "))
}
