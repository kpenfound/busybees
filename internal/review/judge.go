package review

import (
	core "github.com/kpenfound/busybees/core/review"
	"github.com/kpenfound/busybees/internal/duplicates"
)

// compareFindingsText keeps busybees' existing text policy and threshold.
func compareFindingsText(title, body, otherTitle, otherBody string) bool {
	return duplicates.Score(title, body, otherTitle, otherBody) >= duplicates.Threshold
}

func Judge(runs []AngleRun, project *Project) *Findings {
	return core.Judge(runs, project.core(), compareFindingsText)
}
func Merge(findings []Finding, project *Project) []Finding {
	return core.Merge(findings, project.core(), compareFindingsText)
}
func sameFinding(a, b *Finding) bool { return core.SameFinding(a, b, compareFindingsText) }

var compareFindings = core.CompareFindings
