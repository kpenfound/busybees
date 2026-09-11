package review

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// BriefFile is the name the brief is written under inside a review's
// artifact directory, which WriteBrief and ReadBrief are given.
const BriefFile = "brief.json"

// Brief is what the distiller made of the context bundle: the starting
// context of every session after it.
//
// It is angle-agnostic. An angle session reads the same brief whether it
// checks acceptance criteria, tests, style or side effects, so nothing here
// is written for one of them: the criteria are what the change says it does,
// the style rules are what this project asks of any change, and the touched
// areas are what it changed. What each angle makes of that is the angle's
// own business.
//
// Half of it is the distiller's reading of the bundle (Summary, Size,
// AcceptanceCriteria, StyleRules, TouchedAreas) and half is what bees
// already knew (Ref, Title, Sources, NotGathered, SessionID): the session is
// not asked for a fact the gather has, so it cannot get one wrong.
type Brief struct {
	// Ref is the pull request under review and Title its title, both from
	// the bundle.
	Ref   Ref    `json:"ref"`
	Title string `json:"title,omitempty"`

	// Summary is what the change does, in the distiller's words.
	Summary string `json:"summary"`
	// Size is how large the change is, one of Sizes, as the distiller
	// judged it from the change's scope (the files and lines it touches)
	// and its risk (which parts of the project those are).
	Size string `json:"size"`
	// AcceptanceCriteria are what the change says it does: the criteria of
	// the issues it closes, the promises of its own description.
	AcceptanceCriteria []Point `json:"acceptance_criteria,omitempty"`
	// StyleRules are the rules the project's own style sources put on a
	// change like this one, the ones that apply and not the whole of what
	// the files say.
	StyleRules []Point `json:"style_rules,omitempty"`
	// TouchedAreas are the parts of the project the change touched.
	TouchedAreas []TouchedArea `json:"touched_areas,omitempty"`

	// Sources are the context sources the bundle was gathered from, and
	// NotGathered what those sources could not read (Bundle.Skipped), so a
	// session reading the brief knows what nobody looked at.
	Sources     []string `json:"sources,omitempty"`
	NotGathered []string `json:"not_gathered,omitempty"`
	// SessionID is the distiller session's own id, which is what a later
	// session would be resumed from.
	SessionID string `json:"session_id,omitempty"`
}

// Sizes are the sizes a brief can give a change, smallest first.
var Sizes = []string{"xs", "s", "m", "l", "xl"}

// Point is one statement in the brief and where it came from: a criterion
// and the issue that asked for it, a style rule and the file it is written
// in.
type Point struct {
	Text string `json:"text"`
	// Source is where the statement was read, in the distiller's words:
	// "#567", "CLAUDE.md", "the pull request body". It is empty when the
	// distiller named none.
	Source string `json:"source,omitempty"`
}

// TouchedArea is one part of the project the change touched.
type TouchedArea struct {
	// Name is the area in the project's own terms: a package, a command, a
	// document.
	Name string `json:"name"`
	// Paths are the files the change touched in it.
	Paths []string `json:"paths,omitempty"`
	// Summary is what the change did there.
	Summary string `json:"summary,omitempty"`
}

// Text renders the brief as the markdown a session reads. It is what an
// angle session is given, with the diff, instead of the raw bundle.
func (b *Brief) Text() string {
	var out strings.Builder
	fmt.Fprintf(&out, "# Review brief: %s\n\n", b.Ref)
	if b.Title != "" {
		fmt.Fprintf(&out, "%s\n", b.Title)
	}
	fmt.Fprintf(&out, "%s\n", b.Ref.URL())
	if b.Summary != "" {
		fmt.Fprintf(&out, "\n## Summary\n\n%s\n", b.Summary)
	}
	points(&out, "Acceptance criteria", b.AcceptanceCriteria)
	points(&out, "Style rules", b.StyleRules)
	if len(b.TouchedAreas) > 0 {
		out.WriteString("\n## Touched areas\n")
		for _, a := range b.TouchedAreas {
			fmt.Fprintf(&out, "\n### %s\n\n", a.Name)
			for _, p := range a.Paths {
				fmt.Fprintf(&out, "- %s\n", p)
			}
			if a.Summary != "" {
				fmt.Fprintf(&out, "\n%s\n", a.Summary)
			}
		}
	}
	if len(b.Sources) > 0 {
		fmt.Fprintf(&out, "\n## Gathered from\n\n%s\n", strings.Join(b.Sources, ", "))
	}
	if len(b.NotGathered) > 0 {
		out.WriteString("\n## Not gathered\n\n")
		for _, s := range b.NotGathered {
			fmt.Fprintf(&out, "- %s\n", s)
		}
	}
	return out.String()
}

// points renders one list of statements, and nothing at all when the list is
// empty: a heading with nothing under it reads as a distiller that had
// something to say and did not.
func points(out *strings.Builder, heading string, list []Point) {
	if len(list) == 0 {
		return
	}
	fmt.Fprintf(out, "\n## %s\n\n", heading)
	for _, p := range list {
		if p.Source == "" {
			fmt.Fprintf(out, "- %s\n", p.Text)
			continue
		}
		fmt.Fprintf(out, "- %s (%s)\n", p.Text, p.Source)
	}
}

// Validate checks that the distiller produced a brief at all. A session that
// answered with an empty object parses and is worth nothing, so the summary
// is required: it is the one part every reader of the brief uses. So is the
// size, one of Sizes: it is what decides how much of a review the change
// gets.
func (b *Brief) Validate() error {
	if strings.TrimSpace(b.Summary) == "" {
		return errors.New("the brief has no summary of the change")
	}
	if b.Size == "" {
		return errors.New("the brief has no size of the change")
	}
	if !slices.Contains(Sizes, b.Size) {
		return fmt.Errorf("the brief sizes the change %q, which is not one of %s", b.Size, strings.Join(Sizes, ", "))
	}
	return nil
}

// normalise trims the distiller's text and drops the statements that carry
// none, so an empty entry in the list it answered with does not reach a
// session as a bullet with nothing on it. The size is lowercased too: "M" is
// the size m, answered in capitals.
func (b *Brief) normalise() {
	b.Summary = strings.TrimSpace(b.Summary)
	b.Size = strings.ToLower(strings.TrimSpace(b.Size))
	b.AcceptanceCriteria = trimPoints(b.AcceptanceCriteria)
	b.StyleRules = trimPoints(b.StyleRules)
	areas := b.TouchedAreas[:0]
	for _, a := range b.TouchedAreas {
		a.Name = strings.TrimSpace(a.Name)
		a.Summary = strings.TrimSpace(a.Summary)
		a.Paths = trimAll(a.Paths)
		if a.Name == "" {
			continue
		}
		areas = append(areas, a)
	}
	b.TouchedAreas = areas
	if len(b.TouchedAreas) == 0 {
		b.TouchedAreas = nil
	}
}

func trimPoints(list []Point) []Point {
	out := list[:0]
	for _, p := range list {
		p.Text = strings.TrimSpace(p.Text)
		p.Source = strings.TrimSpace(p.Source)
		if p.Text == "" {
			continue
		}
		out = append(out, p)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func trimAll(list []string) []string {
	out := list[:0]
	for _, s := range list {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WriteBrief writes the brief into a review's artifact directory, creating
// the directory when it is not there. What else the directory holds, and
// how a review finds it again, is artifact.go.
func WriteBrief(dir string, b *Brief) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, BriefFile), append(data, '\n'), 0o644)
}

// ReadBrief reads the brief back out of a review's artifact directory.
func ReadBrief(dir string) (*Brief, error) {
	path := filepath.Join(dir, BriefFile)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var b Brief
	if err := json.Unmarshal(data, &b); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return &b, nil
}
