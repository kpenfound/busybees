package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

// The judge. Once the angle sessions have answered, Judge reads every
// answer into findings and Merge turns them into the one list a review
// triages: each finding's severity normalised to the four the schema knows
// and pinned where the project pins its category, a category the project
// turned off dropped, the same problem reported by two angles kept once,
// the list ordered most severe first, and every finding given its id. What
// the person reviewing has said no to before is not the judge's to weigh:
// their reviewer notes act on the list it made, after it (noise.go).
//
// Two findings are the same finding when they are anchored to the same
// place (the same file and side, with lines that overlap, or both without
// lines) and are either in the same category or read alike, and when both
// are unanchored and read alike. The one kept is the more severe, or the
// earlier in angle order when they tie; the other's angle is recorded on it
// in AlsoFrom, its sources are added, and its suggestion is taken when the
// one kept has none.

// severityRank orders the severities a finding can have, most severe
// highest. SeverityOff is not a rank: a finding never has it.
var severityRank = map[string]int{SeverityInfo: 0, SeverityLow: 1, SeverityMedium: 2, SeverityHigh: 3}

// severitySynonyms maps the words sessions use for a severity onto the
// four the schema has.
var severitySynonyms = map[string]string{
	"critical": SeverityHigh, "blocker": SeverityHigh, "blocking": SeverityHigh, "severe": SeverityHigh, "major": SeverityHigh, "error": SeverityHigh,
	"warning": SeverityMedium, "warn": SeverityMedium, "moderate": SeverityMedium, "normal": SeverityMedium,
	"minor": SeverityLow, "nit": SeverityLow, "nitpick": SeverityLow, "trivial": SeverityLow, "cosmetic": SeverityLow, "style": SeverityLow,
	"note": SeverityInfo, "informational": SeverityInfo, "information": SeverityInfo, "suggestion": SeverityInfo, "hint": SeverityInfo, "fyi": SeverityInfo,
}

// Judge merges what the angle sessions came to into the findings a review
// triages. Failed runs and answers ParseFindings cannot parse (including
// malformed findings fields) contribute nothing and are named in Findings.Skipped.
// Missing or null findings lists contribute no findings without being skipped.
// Parsed findings are merged with Merge under project's category pins. A nil
// project pins nothing.
func Judge(runs []AngleRun, project *Settings, alike Comparator) *Findings {
	var all []Finding
	var skipped []string
	for _, run := range runs {
		if run.Failed() {
			skipped = append(skipped, fmt.Sprintf("%s: the session failed: %s", run.Angle, run.Error))
			continue
		}
		findings, err := ParseFindings(run.Angle, run.SessionID, run.Answer)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s: %v", run.Angle, err))
			continue
		}
		all = append(all, findings...)
	}
	return &Findings{Items: Merge(all, project, alike), Skipped: skipped}
}

// Merge is the judge's list: findings, from any number of angles and
// sessions, normalised, pinned and dropped as project says, deduplicated,
// ordered most severe first and given their ids. It is what Judge runs
// over the angle sessions' answers and what a later session's findings are
// merged into the list with: an id is a hash of what places a finding, not
// its position, so merging the list again with new findings appended keeps
// the ids of the ones already in it.
func Merge(findings []Finding, project *Settings, alike Comparator) []Finding {
	if project == nil {
		project = &Settings{}
	}
	var kept []Finding
	for _, f := range findings {
		f.Category = strings.ToLower(strings.TrimSpace(f.Category))
		f.Severity = normaliseSeverity(f.Severity)
		if pinned := pinnedSeverity(project, f.Category); pinned != "" {
			if pinned == SeverityOff {
				continue
			}
			f.Severity = pinned
		}
		merged := false
		for i := range kept {
			if !SameFinding(&kept[i], &f, alike) {
				continue
			}
			kept[i] = mergeInto(kept[i], f)
			merged = true
			break
		}
		if !merged {
			kept = append(kept, f)
		}
	}
	slices.SortStableFunc(kept, CompareFindings)
	for i := range kept {
		kept[i].ID = findingID(&kept[i])
	}
	if kept == nil {
		return []Finding{}
	}
	return kept
}

// pinnedSeverity is the severity project pins category to, whatever case
// the file wrote the category in, and "" when it pins none.
func pinnedSeverity(project *Settings, category string) string {
	if pinned := project.Severity(category); pinned != "" {
		return pinned
	}
	for _, name := range sortedNames(project.Categories) {
		if strings.EqualFold(strings.TrimSpace(name), category) {
			return project.Categories[name]
		}
	}
	return ""
}

// normaliseSeverity is the schema's word for what a session said: the
// four severities as they are, their synonyms mapped, and SeverityMedium
// for a word the judge does not know or no word at all, so a finding whose
// severity could not be read ranks where a person sees it rather than at
// the bottom where it is skipped.
func normaliseSeverity(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if _, ok := severityRank[s]; ok {
		return s
	}
	if mapped, ok := severitySynonyms[s]; ok {
		return mapped
	}
	return SeverityMedium
}

// SameFinding reports whether b reports what a reports: the same place,
// and the same kind of problem or the same words for it (see the package
// comment above).
func SameFinding(a, b *Finding, compare Comparator) bool {
	if a.Anchored() != b.Anchored() {
		return false
	}
	alike := compare != nil && compare(a.Title, a.Body, b.Title, b.Body)
	if !a.Anchored() {
		return alike
	}
	if a.File != b.File || a.Side != b.Side {
		return false
	}
	samePlace := a.Lines.Overlaps(b.Lines) || (a.Lines.IsZero() && b.Lines.IsZero())
	return samePlace && (a.Category == b.Category || alike)
}

// mergeInto folds dup into kept, the two being the same finding: the more
// severe one's text is what stays, the other's angle, sources and, when the
// stayed one has none, suggestion go with it.
func mergeInto(kept, dup Finding) Finding {
	if severityRank[dup.Severity] > severityRank[kept.Severity] {
		kept, dup = dup, kept
	}
	for _, angle := range append([]string{dup.Angle}, dup.AlsoFrom...) {
		if angle != "" && angle != kept.Angle && !slices.Contains(kept.AlsoFrom, angle) {
			kept.AlsoFrom = append(kept.AlsoFrom, angle)
		}
	}
	for _, s := range dup.Sources {
		if !slices.Contains(kept.Sources, s) {
			kept.Sources = append(kept.Sources, s)
		}
	}
	if kept.Suggestion == "" {
		kept.Suggestion = dup.Suggestion
	}
	return kept
}

// CompareFindings orders the list: most severe first, then by file and
// line with the findings about the change as a whole after the anchored
// ones, then in angle order, then by title.
func CompareFindings(a, b Finding) int {
	if d := severityRank[b.Severity] - severityRank[a.Severity]; d != 0 {
		return d
	}
	if a.Anchored() != b.Anchored() {
		if a.Anchored() {
			return -1
		}
		return 1
	}
	if d := strings.Compare(a.File, b.File); d != 0 {
		return d
	}
	if d := a.Lines.Start - b.Lines.Start; d != 0 {
		return d
	}
	if d := slices.Index(BuiltinAngles, a.Angle) - slices.Index(BuiltinAngles, b.Angle); d != 0 {
		return d
	}
	return strings.Compare(a.Title, b.Title)
}

// findingID is the finding's id: the first eight hex digits of the SHA-256
// of what places it, so the same finding gets the same id on every merge.
// The angle is not part of it: two angles reporting one place under one
// title are one finding, merged before ids are given.
func findingID(f *Finding) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{f.File, f.Side, f.Lines.String(), f.Title}, "\x00")))
	return hex.EncodeToString(sum[:4])
}
