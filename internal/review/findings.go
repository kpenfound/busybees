package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A finding is one problem a review found: what is wrong, where, how bad,
// and what showed it. The angle sessions answer with findings in the JSON
// shape prompts/angle.md asks for; ParseFindings reads one session's answer
// into Finding values, and the judge (judge.go) merges every angle's into
// the one list a review triages, findings.json in the artifact directory.
//
// Every field a session may answer with is here under the same name, so
// what a session said and what the review keeps are the same shape, with
// three additions the judge fills in: the id, the angle and the session it
// came from.

// FindingsFile is the name the merged findings are written under inside a
// review's artifact directory.
const FindingsFile = "findings.json"

// Sides of a diff a finding anchors to.
const (
	// SideNew is the file as the change leaves it.
	SideNew = "new"
	// SideOld is a line the change removed.
	SideOld = "old"
)

// Finding is one problem a review found.
type Finding struct {
	// ID names the finding in triage. It is the first eight hex digits of
	// the SHA-256 of the file, side, lines and title, so it is the same on
	// every merge of the same findings and never repeats across the
	// findings one session adds after another.
	ID string `json:"id"`
	// Angle is the angle that found it, a name in BuiltinAngles, and
	// SessionID that angle's session (AngleRun.SessionID), which is what
	// the ask action resumes with a question about it.
	Angle     string `json:"angle"`
	SessionID string `json:"session_id,omitempty"`
	// Category is the kind of problem, in the angle's own word or two,
	// lowercased: "naming", "missing test". A project pins a category's
	// severity by this name (Project.Severity).
	Category string `json:"category"`
	// Severity is one of SeverityInfo, SeverityLow, SeverityMedium and
	// SeverityHigh, after the judge has normalised what the session said
	// and applied the project's pins. Never SeverityOff: a finding in a
	// category pinned off is dropped, not kept.
	Severity string `json:"severity"`
	// File, Lines and Side anchor the finding to the diff: the path in the
	// repository, the lines in it, and which side of the change they are
	// on. A finding about the change as a whole has none of them.
	File  string    `json:"file,omitempty"`
	Lines LineRange `json:"lines,omitzero"`
	Side  string    `json:"side,omitempty"`
	// Title is the finding in one line and Body what is wrong and why it
	// matters.
	Title string `json:"title"`
	Body  string `json:"body"`
	// Suggestion is the text that would replace the lines, when the angle
	// could write it.
	Suggestion string `json:"suggestion,omitempty"`
	// Evidence is what the angle read that shows the problem.
	Evidence string `json:"evidence,omitempty"`
	// Sources are where the rule or the requirement the finding is about
	// was read: "#12", "CONTRIBUTING.md".
	Sources []string `json:"sources,omitempty"`
	// AlsoFrom names the other angles that reported the same finding, when
	// the judge merged theirs into this one.
	AlsoFrom []string `json:"also_from,omitempty"`
}

// Anchored reports whether the finding points at a file.
func (f *Finding) Anchored() bool { return f.File != "" }

// LineRange is the lines a finding is about, both ends inclusive, 1-based.
// It is written as the two-element array the angle prompt asks for,
// [start, end], and read from that, from a one-element array, from a bare
// number and from {"start": n, "end": m}: every way a session has written
// it. The zero value is no lines at all.
type LineRange struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// IsZero reports whether the range holds no lines, which is what leaves it
// out of the JSON.
func (r LineRange) IsZero() bool { return r == LineRange{} }

// Overlaps reports whether the two ranges share a line. Two empty ranges
// share none.
func (r LineRange) Overlaps(o LineRange) bool {
	if r.IsZero() || o.IsZero() {
		return false
	}
	return r.Start <= o.End && o.Start <= r.End
}

// String is the range as a person writes it: "12" or "12-14".
func (r LineRange) String() string {
	switch {
	case r.IsZero():
		return ""
	case r.Start == r.End:
		return fmt.Sprint(r.Start)
	}
	return fmt.Sprintf("%d-%d", r.Start, r.End)
}

// MarshalJSON writes the range as [start, end].
func (r LineRange) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]int{r.Start, r.End})
}

// UnmarshalJSON reads every shape a range has been written in. A range that
// makes no sense (an end before its start, a line 0 or below) reads as no
// lines rather than as an error: a session that got the lines wrong still
// found something.
func (r *LineRange) UnmarshalJSON(data []byte) error {
	data = []byte(strings.TrimSpace(string(data)))
	var got LineRange
	switch {
	case len(data) == 0 || string(data) == "null":
	case data[0] == '[':
		var list []int
		if err := json.Unmarshal(data, &list); err != nil {
			return err
		}
		switch len(list) {
		case 0:
		case 1:
			got = LineRange{list[0], list[0]}
		default:
			got = LineRange{list[0], list[1]}
		}
	case data[0] == '{':
		var obj struct{ Start, End int }
		if err := json.Unmarshal(data, &obj); err != nil {
			return err
		}
		got = LineRange{obj.Start, obj.End}
		if got.End == 0 {
			got.End = got.Start
		}
	default:
		var n int
		if err := json.Unmarshal(data, &n); err != nil {
			return err
		}
		got = LineRange{n, n}
	}
	if got.Start <= 0 || got.End < got.Start {
		got = LineRange{}
	}
	*r = got
	return nil
}

// Findings is what the judge made of the angle sessions: the one list a
// review triages, and the angles it could not read.
type Findings struct {
	// Items are the findings, deduplicated, severity-normalised, most
	// severe first. Empty for a change with nothing wrong.
	Items []Finding `json:"findings"`
	// Skipped names the angles that reported nothing the judge could read,
	// each with why: a session that failed, an answer with no findings
	// list in it. A review with a skipped angle is a review nobody looked
	// at from that angle, which is worth saying when the findings are.
	Skipped []string `json:"skipped,omitempty"`
}

// findingDraft is a finding as a session answers with one: the fields
// prompts/angle.md asks for, before the judge has normalised them.
type findingDraft struct {
	Category   string    `json:"category"`
	Severity   string    `json:"severity"`
	File       string    `json:"file"`
	Lines      LineRange `json:"lines"`
	Side       string    `json:"side"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	Suggestion string    `json:"suggestion"`
	Evidence   string    `json:"evidence"`
	Sources    []string  `json:"sources"`
}

// ParseFindings reads the findings out of what an angle session answered:
// the "findings" list of the JSON object in it, marked as found by angle
// and sessionID. Every finding comes back as the session said it, trimmed,
// with its category lowercased and its anchor made consistent (no lines or
// side without a file, a side that is SideNew or SideOld); the judge does
// the rest. An answer with no JSON object in it, or an object that is not a
// findings list, is an error: the session did not review.
//
// A finding with neither title nor body is dropped: there is nothing to
// show a person. One with a body and no title is kept under its first line.
func ParseFindings(angle, sessionID, text string) ([]Finding, error) {
	obj, ok := jsonObject(text)
	if !ok {
		return nil, errors.New("the session answered with no JSON object")
	}
	var answer struct {
		Findings []findingDraft `json:"findings"`
	}
	if err := json.Unmarshal([]byte(obj), &answer); err != nil {
		return nil, fmt.Errorf("the session's JSON object is not a findings list: %w", err)
	}
	findings := make([]Finding, 0, len(answer.Findings))
	for _, d := range answer.Findings {
		f := Finding{
			Angle:      angle,
			SessionID:  sessionID,
			Category:   strings.ToLower(strings.TrimSpace(d.Category)),
			Severity:   strings.TrimSpace(d.Severity),
			File:       strings.TrimSpace(d.File),
			Lines:      d.Lines,
			Side:       strings.ToLower(strings.TrimSpace(d.Side)),
			Title:      strings.TrimSpace(d.Title),
			Body:       strings.TrimSpace(d.Body),
			Suggestion: strings.TrimSpace(d.Suggestion),
			Evidence:   strings.TrimSpace(d.Evidence),
			Sources:    trimAll(d.Sources),
		}
		if f.Title == "" {
			f.Title, _, _ = strings.Cut(f.Body, "\n")
			f.Title = strings.TrimSpace(f.Title)
		}
		if f.Title == "" {
			continue
		}
		if !f.Anchored() {
			f.Lines, f.Side = LineRange{}, ""
		} else if f.Side != SideOld {
			f.Side = SideNew
		}
		findings = append(findings, f)
	}
	return findings, nil
}

// WriteFindings writes the merged findings into a review's artifact
// directory, creating it when it is not there.
func WriteFindings(dir string, f *Findings) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if f.Items == nil {
		f.Items = []Finding{}
	}
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, FindingsFile), append(data, '\n'), 0o644)
}

// ReadFindings reads the merged findings back out of a review's artifact
// directory.
func ReadFindings(dir string) (*Findings, error) {
	path := filepath.Join(dir, FindingsFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var f Findings
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	if f.Items == nil {
		f.Items = []Finding{}
	}
	return &f, nil
}
