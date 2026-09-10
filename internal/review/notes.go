package review

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/kpenfound/busybees/internal/duplicates"
	"github.com/kpenfound/busybees/internal/text"
)

// The reviewer notes: one markdown file of a person's own, outside any
// review, that says what reviews keep telling them and they keep saying no
// to. Triage appends a line to it every time a finding is dismissed, and
// Consolidate turns the lines that repeat into rules. The rules are what the
// noise filter drops or ranks down before triage sees it (noise.go) and what
// an angle session is told has been dismissed before (angles.go).
//
// The file is the person's to read and to edit, so it is markdown with two
// kinds of line in it and prose anywhere else:
//
//	# Reviewer notes
//
//	## Rules
//
//	<!-- bees:review:rules -->
//	- [acme/widgets] [style] [naming] drop: receiver names are short here (3 dismissals)
//	- [*] [test_coverage] [*] downrank: generated files carry no tests (2 dismissals)
//	<!-- /bees:review:rules -->
//
//	## Dismissals
//
//	- [acme/widgets] [style] [naming] receiver names are short here
//
// A dismissal is `[repo] [angle] [category] reason` and is read wherever it
// is in the file. A rule is `[repo] [angle] [category] action: text` and is
// read between the two markers alone: that block is the one part of the file
// bees writes, so a rule belongs in it and everything outside it survives a
// consolidation untouched. `*` in any of the three fields, or an empty pair
// of brackets, matches anything.
//
// Nothing in the block is ever deleted. Consolidate adds the patterns that
// are new and refreshes the count of the ones already there, and a rule whose
// text or action a person rewrote stays as they wrote it: they read the
// findings and bees did not.

// Names of the parts of the reviewer notes file.
const (
	// rulesBegin and rulesEnd delimit the rules, the one part of the file
	// consolidation writes.
	rulesBegin = "<!-- bees:review:rules -->"
	rulesEnd   = "<!-- /bees:review:rules -->"
	// rulesHeading is the heading the block is created under, in a file
	// that has no block.
	rulesHeading = "## Rules"
	// anyValue in a rule's repo, angle or category matches every value.
	anyValue = "*"
)

// What a rule does to a finding that matches it.
const (
	// RuleDrop keeps the finding out of triage.
	RuleDrop = "drop"
	// RuleDownrank lets the finding through one severity lower.
	RuleDownrank = "downrank"
)

// RuleActions lists the actions a rule can take, most severe first.
var RuleActions = []string{RuleDrop, RuleDownrank}

// How many dismissals of one pattern make a rule of it.
const (
	// MinDismissals is what a pattern needs before consolidation writes a
	// rule for it: one dismissal is a finding a person did not want, and
	// two are a habit.
	MinDismissals = 2
	// DropDismissals is what it needs for that rule to drop the finding
	// rather than rank it down.
	DropDismissals = 3
)

// notesTemplate is the file a first dismissal creates: the two sections, and
// the block the rules go in.
const notesTemplate = `# Reviewer notes

What reviews keep reporting and you keep dismissing. Every finding you
dismiss in triage is appended under Dismissals; ` + "`bees review consolidate`" + `
reads them and writes the patterns that repeat into the rules block below. A
finding that matches a rule is dropped, or ranked down, before it reaches
triage, and the angle sessions are told about it.

Both are yours to edit: rewrite a rule, change what it does, delete a line.
Rules are read between the markers only.

## Rules

` + rulesBegin + `
` + rulesEnd + `

## Dismissals

`

// Dismissal is one finding triage said no to: where it was found, what found
// it, what kind of problem it was, and why it was dismissed.
type Dismissal struct {
	// Repo is the repository the pull request belongs to, "owner/name".
	Repo string
	// Angle is the angle that reported the finding and Category the kind of
	// problem it called it.
	Angle    string
	Category string
	// Reason is what triage said when it dismissed the finding.
	Reason string
}

// DismissalOf is the note a dismissed finding leaves: the repository it was
// found in, the angle and category it was filed under, and the reason triage
// gave for dismissing it.
func DismissalOf(repo string, f *Finding, reason string) Dismissal {
	return Dismissal{Repo: repo, Angle: f.Angle, Category: f.Category, Reason: reason}
}

// Line is the dismissal as it is written in the file.
func (d Dismissal) Line() string {
	return fmt.Sprintf("- [%s] [%s] [%s] %s", d.Repo, d.Angle, d.Category, strings.TrimSpace(d.Reason))
}

// Rule is a dismissal pattern: what a review does with a finding that a
// person has said no to before.
type Rule struct {
	// Repo, Angle and Category are what the finding must be to match, each
	// one anyValue or empty for "whatever it is".
	Repo     string
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
	line := fmt.Sprintf("- [%s] [%s] [%s] %s: %s", or(r.Repo, anyValue), or(r.Angle, anyValue), or(r.Category, anyValue), r.Action, strings.TrimSpace(r.Text))
	if r.Count > 0 {
		line += fmt.Sprintf(" (%s)", text.Count(r.Count, "dismissal"))
	}
	return line
}

// Notes is a reviewer notes file read into memory.
type Notes struct {
	// Path is the file, absolute, and set even when it does not exist
	// (Loaded says which).
	Path string
	// Loaded reports whether Path existed. Notes that were never written
	// are not an error: nothing has been dismissed yet.
	Loaded bool
	// Rules are the rules in the block, in the order they are written.
	Rules []Rule
	// Dismissals are every dismissal line in the file, oldest first.
	Dismissals []Dismissal
	// Skipped are the lines in the block that are not rules: a person's
	// note to themselves, an action misspelt. They are kept as they are and
	// written back under the rules, so a consolidation loses nothing.
	Skipped []string
}

// ReadNotes reads the reviewer notes at path. A file that is not there reads
// as empty notes, with Loaded false: a person who has dismissed nothing has
// no notes, which is not a failure.
func ReadNotes(path string) (*Notes, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	n := &Notes{Path: abs}
	data, err := os.ReadFile(abs)
	if os.IsNotExist(err) {
		return n, nil
	}
	if err != nil {
		return nil, err
	}
	n.Loaded = true
	inBlock := false
	for _, line := range strings.Split(string(data), "\n") {
		switch trimmed := strings.TrimSpace(line); {
		case trimmed == rulesBegin:
			inBlock = true
			continue
		case trimmed == rulesEnd:
			inBlock = false
			continue
		case trimmed == "":
			continue
		case inBlock:
			if r, ok := parseRule(line); ok {
				n.Rules = append(n.Rules, r)
			} else {
				n.Skipped = append(n.Skipped, trimmed)
			}
		default:
			if d, ok := parseDismissal(line); ok {
				n.Dismissals = append(n.Dismissals, d)
			}
		}
	}
	return n, nil
}

// AppendDismissal records a dismissal in the reviewer notes at path, which is
// created, with its sections, when it is not there. The line goes at the end
// of the file: dismissals are read wherever they are, so nothing has to be
// found first.
func AppendDismissal(path string, d Dismissal) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return err
	}
	var head string
	switch data, err := os.ReadFile(abs); {
	case os.IsNotExist(err):
		head = notesTemplate
	case err != nil:
		return err
	case len(data) > 0 && !strings.HasSuffix(string(data), "\n"):
		head = "\n"
	}
	f, err := os.OpenFile(abs, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.WriteString(head + d.Line() + "\n"); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// Write writes the rules back into the file, replacing what is between the
// two markers and leaving every other line of it alone. Notes with no block
// in them get one, under a heading of its own at the end of the file, and
// notes that are not there at all are created from the template first.
func (n *Notes) Write() error {
	if err := os.MkdirAll(filepath.Dir(n.Path), 0o755); err != nil {
		return err
	}
	data, err := os.ReadFile(n.Path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	body := string(data)
	if os.IsNotExist(err) {
		body = notesTemplate
	}
	begin, end := strings.Index(body, rulesBegin), strings.Index(body, rulesEnd)
	if begin >= 0 && end > begin {
		body = body[:begin] + n.block() + body[end+len(rulesEnd):]
	} else {
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		body += "\n" + rulesHeading + "\n\n" + n.block() + "\n"
	}
	return os.WriteFile(n.Path, []byte(body), 0o644)
}

// block is the rules block as it is written: the rules, then the lines of it
// that are not rules, between the markers.
func (n *Notes) block() string {
	var out strings.Builder
	out.WriteString(rulesBegin + "\n")
	for _, r := range n.Rules {
		out.WriteString(r.Line() + "\n")
	}
	for _, line := range n.Skipped {
		out.WriteString(line + "\n")
	}
	out.WriteString(rulesEnd)
	return out.String()
}

// Consolidate turns the dismissals that repeat into rules: dismissals of the
// same repository, angle and category whose reasons read alike are one
// pattern, and a pattern dismissed MinDismissals times or more is a rule,
// dropping the finding from DropDismissals dismissals and ranking it down
// below that.
//
// It adds and refreshes, and never deletes or rewrites: a pattern already
// ruled on keeps the action and the text it has, whoever last wrote them,
// and only its count moves. What it changed comes back as the rules it
// added and the rules whose count it refreshed; writing them to the file is
// Write.
func (n *Notes) Consolidate() (added, refreshed []Rule) {
	for _, c := range n.clusters() {
		if len(c.reasons) < MinDismissals {
			continue
		}
		if i := n.ruleFor(c); i >= 0 {
			if n.Rules[i].Count == len(c.reasons) {
				continue
			}
			n.Rules[i].Count = len(c.reasons)
			refreshed = append(refreshed, n.Rules[i])
			continue
		}
		r := c.rule()
		n.Rules = append(n.Rules, r)
		added = append(added, r)
	}
	return added, refreshed
}

// cluster is one dismissal pattern: the dismissals of one repository, angle
// and category whose reasons read alike, in the order they were written.
type cluster struct {
	repo     string
	angle    string
	category string
	reasons  []string
}

// rule is the rule a cluster makes: what a person kept saying, and how sure
// they said it. The text is the reason they gave first, the wording the ones
// after it were found to read like.
func (c *cluster) rule() Rule {
	action := RuleDownrank
	if len(c.reasons) >= DropDismissals {
		action = RuleDrop
	}
	return Rule{
		Repo:     or(c.repo, anyValue),
		Angle:    or(c.angle, anyValue),
		Category: or(c.category, anyValue),
		Action:   action,
		Text:     c.reasons[0],
		Count:    len(c.reasons),
	}
}

// clusters groups the dismissals into patterns: same repository, angle and
// category, and a reason that reads like one already in the cluster. The
// comparison is internal/duplicates.Score, the one the judge tells two
// angles' wording of a finding apart by.
func (n *Notes) clusters() []*cluster {
	var out []*cluster
	for _, d := range n.Dismissals {
		reason := strings.TrimSpace(d.Reason)
		var found *cluster
		for _, c := range out {
			if c.repo == d.Repo && c.angle == d.Angle && c.category == d.Category && alike(c.reasons, reason) {
				found = c
				break
			}
		}
		if found == nil {
			found = &cluster{repo: d.Repo, angle: d.Angle, category: d.Category}
			out = append(out, found)
		}
		found.reasons = append(found.reasons, reason)
	}
	return out
}

// ruleFor is the rule already written for a cluster, and -1 when there is
// none: one whose repository, angle and category the cluster's match and
// whose text reads like any of the reasons in it, so a rule a person
// reworded is still recognised as the pattern it came from. A rule with no
// text matches its whole category, this cluster among them.
func (n *Notes) ruleFor(c *cluster) int {
	for i, r := range n.Rules {
		if !matchesPattern(r.Repo, c.repo) || !matchesPattern(r.Angle, c.angle) || !matchesPattern(r.Category, c.category) {
			continue
		}
		if strings.TrimSpace(r.Text) == "" || alike(c.reasons, r.Text) {
			return i
		}
	}
	return -1
}

// alike reports whether s reads like any of the texts.
func alike(texts []string, s string) bool {
	for _, t := range texts {
		if duplicates.Score(t, "", s, "") >= duplicates.Threshold {
			return true
		}
	}
	return false
}

// matchesPattern reports whether a rule's field matches a finding's value.
// An empty field and anyValue match anything; the rest match whatever the
// case.
func matchesPattern(pattern, value string) bool {
	pattern = strings.TrimSpace(pattern)
	return pattern == "" || pattern == anyValue || strings.EqualFold(pattern, strings.TrimSpace(value))
}

// notesLine reads the `[repo] [angle] [category] rest` a dismissal and a rule
// are both written as, with or without the markdown bullet a person's editor
// puts in front of it.
var notesLine = regexp.MustCompile(`^\s*(?:[-*]\s+)?\[([^\]]*)\]\s*\[([^\]]*)\]\s*\[([^\]]*)\]\s*(.*)$`)

// countSuffix is the "(3 dismissals)" a rule is written with, which is
// rewritten on every consolidation and is not part of the rule's text.
var countSuffix = regexp.MustCompile(`\s*\((\d+) dismissals?\)\s*$`)

// parseDismissal reads a dismissal line, and reports false for a line that is
// not one: the file is a person's, and its prose is not an error.
func parseDismissal(line string) (Dismissal, bool) {
	m := notesLine.FindStringSubmatch(line)
	if m == nil {
		return Dismissal{}, false
	}
	return Dismissal{
		Repo:     strings.TrimSpace(m[1]),
		Angle:    strings.TrimSpace(m[2]),
		Category: strings.TrimSpace(m[3]),
		Reason:   strings.TrimSpace(m[4]),
	}, true
}

// parseRule reads a rule line, and reports false for a line in the block that
// is not one: an action bees does not know is a line to keep, not a rule to
// act on.
func parseRule(line string) (Rule, bool) {
	m := notesLine.FindStringSubmatch(line)
	if m == nil {
		return Rule{}, false
	}
	action, body, ok := strings.Cut(m[4], ":")
	action = strings.ToLower(strings.TrimSpace(action))
	if !ok || (action != RuleDrop && action != RuleDownrank) {
		return Rule{}, false
	}
	r := Rule{
		Repo:     strings.TrimSpace(m[1]),
		Angle:    strings.TrimSpace(m[2]),
		Category: strings.TrimSpace(m[3]),
		Action:   action,
		Text:     strings.TrimSpace(body),
	}
	if c := countSuffix.FindStringSubmatch(r.Text); c != nil {
		r.Count, _ = strconv.Atoi(c[1])
		r.Text = strings.TrimSpace(countSuffix.ReplaceAllString(r.Text, ""))
	}
	return r, true
}

// or is s, or fallback when s is empty.
func or(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}
