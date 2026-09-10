package review

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/kpenfound/busybees/internal/duplicates"
)

// The judge. Once the angle sessions have answered, Judge reads every
// answer into findings and Merge turns them into the one list a review
// triages: each finding's severity normalised to the four the schema knows
// and pinned where the project pins its category, a category the project
// turned off dropped, the same problem reported by two angles kept once,
// the list ordered most severe first, and every finding given its id.
//
// It is deterministic code, not a session. The factory's own merge step,
// the mixture-of-experts assembler (internal/scheduler/bestofn.go with
// task/developer_moe_assemble.md), was the starting point named for it and
// is not reused, because it is not a merge of findings: it is a developer
// session that reads the expert branches of an issue and writes one branch
// and one pull request out of them. Its input is commits and its output is
// commits; nothing in it produces or consumes a list, so there is nothing
// for an adapter to adapt. What the assembler is for, combining solutions
// that deliberately differ, is not what the judge is for either: two angles
// that report the same line are not two solutions to pick between, they
// are one finding said twice, and telling that apart is a text comparison a
// session would only make slower and less repeatable. The comparison
// itself is internal/duplicates.Score, the same one QA's bug filing uses to
// tell a new bug from one already filed.
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
// triages. A run that failed, and one whose answer holds no findings list,
// contributes nothing and is named in Findings.Skipped; the rest are read
// with ParseFindings and merged with Merge under project's category pins. A
// nil project pins nothing.
func Judge(runs []AngleRun, project *Project) *Findings {
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
	return &Findings{Items: Merge(all, project), Skipped: skipped}
}

// Merge is the judge's list: findings, from any number of angles and
// sessions, normalised, pinned and dropped as project says, deduplicated,
// ordered most severe first and given their ids. It is what Judge runs
// over the angle sessions' answers and what a later session's findings are
// merged into the list with: an id is a hash of what places a finding, not
// its position, so merging the list again with new findings appended keeps
// the ids of the ones already in it.
func Merge(findings []Finding, project *Project) []Finding {
	if project == nil {
		project = &Project{}
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
			if !sameFinding(&kept[i], &f) {
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
	slices.SortStableFunc(kept, compareFindings)
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
func pinnedSeverity(project *Project, category string) string {
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

// sameFinding reports whether b reports what a reports: the same place,
// and the same kind of problem or the same words for it (see the package
// comment above).
func sameFinding(a, b *Finding) bool {
	if a.Anchored() != b.Anchored() {
		return false
	}
	alike := duplicates.Score(a.Title, a.Body, b.Title, b.Body) >= duplicates.Threshold
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

// compareFindings orders the list: most severe first, then by file and
// line with the findings about the change as a whole after the anchored
// ones, then in angle order, then by title.
func compareFindings(a, b Finding) int {
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
