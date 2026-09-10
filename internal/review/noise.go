package review

import (
	"slices"
	"strings"

	"github.com/kpenfound/busybees/internal/duplicates"
)

// The noise filter: the last thing between the judge's list and triage.
//
// A person who dismissed a finding once should not be asked about it again,
// and the reviewer notes (notes.go) are where the dismissals they have made
// are kept. Filter joins the findings against the rules a consolidation made
// of them: a finding that matches a rule is dropped, or ranked down one
// severity, before anybody reads it, and either way it is recorded as
// Silenced so a review can say what its notes hid.
//
// What a rule matches is its repository, its angle and its category, with
// `*` for whatever it is, and its text against the finding's title: the line
// a person read when they dismissed the finding, compared the way the judge
// compares two angles' wording of one finding. A rule with no text matches
// every finding in its repository, angle and category, which is how a whole
// category is silenced.
//
// The rules also reach the angle sessions before they look at anything
// (angles.go): a session told what has been dismissed before reports fewer
// of them, and the filter is what catches the rest.

// Silenced is one finding the reviewer notes acted on: enough of it to say
// what was hidden and which rule hid it, without keeping the finding itself.
type Silenced struct {
	// ID, Title, Angle and Category are the finding's, as it was when the
	// rule matched.
	ID       string `json:"id"`
	Title    string `json:"title"`
	Angle    string `json:"angle"`
	Category string `json:"category"`
	// Action is what the rule did: RuleDrop, and the finding is not in the
	// list at all, or RuleDownrank, and it is in it one severity lower.
	Action string `json:"action"`
	// Severity is the severity the finding had before the rule acted.
	Severity string `json:"severity"`
	// Rule is the rule that matched, as the notes file writes it.
	Rule string `json:"rule"`
}

// Filter is the noise filter over a review's findings: the list triage sees,
// and what the rules kept out of it or ranked down in it. repo is the
// repository under review, "owner/name", which is what a rule's repository
// is matched against.
//
// The findings come back ordered as the judge orders them, most severe
// first, so a finding ranked down moves to where its new severity puts it.
// Findings with no rule to match are returned as they are: an empty rule
// list changes nothing.
func Filter(findings []Finding, rules []Rule, repo string) ([]Finding, []Silenced) {
	kept := make([]Finding, 0, len(findings))
	var silenced []Silenced
	for _, f := range findings {
		rule, ok := matchRule(rules, repo, &f)
		if !ok {
			kept = append(kept, f)
			continue
		}
		silenced = append(silenced, Silenced{
			ID:       f.ID,
			Title:    f.Title,
			Angle:    f.Angle,
			Category: f.Category,
			Action:   rule.Action,
			Severity: f.Severity,
			Rule:     rule.Line(),
		})
		if rule.Action == RuleDrop {
			continue
		}
		f.Severity = downrankSeverity(f.Severity)
		kept = append(kept, f)
	}
	slices.SortStableFunc(kept, compareFindings)
	return kept, silenced
}

// matchRule is the rule that acts on a finding, and false when none does.
// A rule that drops beats one that ranks down however they are ordered in
// the file, so a person editing their notes does not have to think about
// order; between rules that do the same thing, the first one in the file
// acts.
func matchRule(rules []Rule, repo string, f *Finding) (Rule, bool) {
	var best Rule
	found := false
	for _, r := range rules {
		if !r.Matches(repo, f) {
			continue
		}
		if r.Action == RuleDrop {
			return r, true
		}
		if !found {
			best, found = r, true
		}
	}
	return best, found
}

// Matches reports whether the rule is about this finding of a review of
// repo: its repository, angle and category, and its text against the
// finding's title (see the file comment above).
func (r Rule) Matches(repo string, f *Finding) bool {
	if !matchesPattern(r.Repo, repo) || !matchesPattern(r.Angle, f.Angle) || !matchesPattern(r.Category, f.Category) {
		return false
	}
	if strings.TrimSpace(r.Text) == "" {
		return true
	}
	return duplicates.Score(r.Text, "", f.Title, "") >= duplicates.Threshold
}

// severityOrder is the severities a finding can have, least severe first.
var severityOrder = []string{SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh}

// downrankSeverity is one severity below s, and SeverityInfo for a finding
// already there: a rule ranks a finding down, it never drops one that only
// says downrank.
func downrankSeverity(s string) string {
	i := slices.Index(severityOrder, normaliseSeverity(s))
	if i <= 0 {
		return SeverityInfo
	}
	return severityOrder[i-1]
}

// noiseSection is what an angle session reviewing repo is told about the
// rules that name its angle: what a person has dismissed from it before, so
// it does not report the same thing again. It is empty when no rule does,
// and a rule with no text says nothing to a session: it silences a whole
// category, which is not something to tell a session it found.
func noiseSection(rules []Rule, repo, angle string) string {
	var out strings.Builder
	for _, r := range rules {
		if !matchesPattern(r.Repo, repo) || !matchesPattern(r.Angle, angle) || strings.TrimSpace(r.Text) == "" {
			continue
		}
		if out.Len() == 0 {
			out.WriteString("## Dismissed before\n\n")
			out.WriteString("The reviewer of this repository has dismissed findings like these, from your angle, in earlier reviews:\n\n")
		}
		out.WriteString("- " + strings.TrimSpace(r.Text) + "\n")
	}
	if out.Len() == 0 {
		return ""
	}
	out.WriteString("\nDo not report one of them again unless this change makes it newly wrong, and say what makes it so when you do. A finding that reads like one of them is dropped, or ranked down, before the reviewer sees it.\n")
	return out.String()
}
