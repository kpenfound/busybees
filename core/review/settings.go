package review

import "slices"

// The angles a review runs from, each one a session of its own reading the
// review brief, as many of them as the change's size calls for (sizeAngles
// in angles.go). A project turns one off, or back on, under [angles].
const (
	// AngleQuickGeneral gives a small change one light pass for whatever is
	// wrong with it.
	AngleQuickGeneral = "quick_general"
	// AngleGeneral reviews the change as a whole, thoroughly, not narrowed
	// to one concern.
	AngleGeneral = "general"
	// AngleDocs checks the comments and the prose that describe the code
	// against the code as the change leaves it.
	AngleDocs = "docs"
	// AngleTests checks the tests and the documentation the change owes.
	AngleTests = "test_coverage"
	// AngleAcceptance checks the change against the acceptance criteria of
	// what it says it does.
	AngleAcceptance = "acceptance_criteria"
	// AngleSideEffects checks what the change breaks elsewhere.
	AngleSideEffects = "side_effects"
)

// BuiltinAngles lists the angles a review can run, in the order they are
// fanned out. A review runs the ones its change's size calls for that the
// project enables (anglesFor in angles.go).
var BuiltinAngles = []string{AngleQuickGeneral, AngleGeneral, AngleDocs, AngleTests, AngleAcceptance, AngleSideEffects}

// Severities a category can be pinned to, most severe last. SeverityOff
// drops the category's findings instead of ranking them.
const (
	SeverityOff    = "off"
	SeverityInfo   = "info"
	SeverityLow    = "low"
	SeverityMedium = "medium"
	SeverityHigh   = "high"
)

// Severities lists the accepted severity values, in the order they are
// printed.
var Severities = []string{SeverityOff, SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh}

// Settings controls angle selection and category severity independently of
// configuration files or acquisition policy. Missing angles are enabled.
type Settings struct {
	Angles     map[string]bool
	Categories map[string]string
}

// EnabledAngles lists the angles this project enables, in BuiltinAngles
// order. A review runs the ones among them its change's size calls for
// (anglesFor in angles.go).
func (p *Settings) EnabledAngles() []string {
	angles := make([]string, 0, len(BuiltinAngles))
	for _, a := range BuiltinAngles {
		if on, ok := p.Angles[a]; ok && !on {
			continue
		}
		angles = append(angles, a)
	}
	return angles
}

// Severity is the severity this project pins findings in category to, and ""
// when it pins none. A category pinned to SeverityOff is dropped.
func (p *Settings) Severity(category string) string { return p.Categories[category] }

// sortedNames orders a map's keys, so a file with several bad ones reports
// them in the same order every time.
func sortedNames[V any](m map[string]V) []string {
	names := make([]string, 0, len(m))
	for k := range m {
		names = append(names, k)
	}
	slices.Sort(names)
	return names
}
