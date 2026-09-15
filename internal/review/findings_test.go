package review

import (
	"strings"
)

func testFinding() Finding {
	return Finding{
		ID:         "1a2b3c4d",
		Angle:      AngleTests,
		SessionID:  "sess-test_coverage",
		Category:   "missing test",
		Severity:   SeverityHigh,
		File:       "internal/review/gather.go",
		Lines:      LineRange{Start: 12, End: 14},
		Side:       SideNew,
		Title:      "Gather has no test for a source that cannot read",
		Body:       "the acceptance criterion says a source that cannot read something does not fail the review, and nothing exercises it",
		Suggestion: "func TestASourceThatCannotReadDoesNotFailTheReview(t *testing.T) {",
		Evidence:   "gather_test.go has no test naming Skipped",
		Sources:    []string{"#566", "CONTRIBUTING.md"},
		AlsoFrom:   []string{AngleAcceptance},
	}
}

// sessionAnswer is what an angle session answers with: the JSON object the
// prompt asks for, with these findings in it.
func sessionAnswer(findings ...string) string {
	return `{"findings": [` + strings.Join(findings, ",") + `]}`
}
