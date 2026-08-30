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
// as a bee's or a person's.
func issueText(i github.Issue, parent *github.Parent, l config.Labels) string {
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
			c.Author.Login, author(c.Body), c.CreatedAt.UTC().Format(timeFormat), strings.TrimRight(c.Body, "\n"))
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

// author says who wrote a comment: the bees role whose marker it carries, or
// a person. Bees and humans share one GitHub account, so the login is not a
// signal and the marker is the only one there is. A comment may quote an
// earlier one, marker included, so the *last* marker is the author's own —
// withMarker appends it at the end — and any earlier one is quoted context.
func author(body string) string {
	i := strings.LastIndex(body, github.BeesMarker)
	if i < 0 {
		return "human"
	}
	rest := body[i+len(github.BeesMarker):]
	role, _, ok := strings.Cut(rest, "-->")
	if !ok {
		return "human"
	}
	return "bee: " + strings.TrimSpace(role)
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
