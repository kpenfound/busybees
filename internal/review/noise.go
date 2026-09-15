package review

import core "github.com/kpenfound/busybees/core/review"

type Silenced = core.Silenced

func Filter(findings []Finding, rules []Rule, repo string) ([]Finding, []Silenced) {
	return core.Filter(findings, coreRules(rules), repo, compareFindingsText)
}
func (r Rule) core() core.Rule {
	return core.Rule{Scope: r.Repo, Angle: r.Angle, Category: r.Category, Action: r.Action, Text: r.Text, Count: r.Count}
}
func coreRules(rules []Rule) []core.Rule {
	out := make([]core.Rule, len(rules))
	for i, r := range rules {
		out[i] = r.core()
	}
	return out
}
func (r Rule) Matches(repo string, f *Finding) bool {
	return r.core().Matches(repo, f, compareFindingsText)
}
