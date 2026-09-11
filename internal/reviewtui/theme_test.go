package reviewtui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/kpenfound/busybees/internal/review"
)

// colourTokens are the ways a colour can be named in this package. They are
// built by concatenation so that this file, which has to hold them as data,
// does not match its own search, the same reason theme.go itself is the one
// file the sweep below skips.
var colourTokens = []string{
	"lipgloss." + "Color(",
	"lipgloss." + "AdaptiveColor",
	"." + "Foreground(",
	"." + "Background(",
	"." + "BorderForeground(",
	"." + "BorderBackground(",
}

// TestOnlyTheThemeNamesColours is the palette pin, the one internal/tui
// keeps for its own theme.go: a person repainting the triage screen has one
// file to read, and a colour set anywhere else would be invisible to them.
func TestOnlyTheThemeNamesColours(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package directory: %v", err)
	}
	var swept int
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || name == "theme.go" {
			continue
		}
		swept++
		b, err := os.ReadFile(filepath.Clean(name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, tok := range colourTokens {
			if strings.Contains(string(b), tok) {
				t.Errorf("%s names a colour with %q: every colour belongs in theme.go", name, tok)
			}
		}
	}
	if swept == 0 {
		t.Fatal("swept no files: the package's sources were not found")
	}
}

// unstyled is the foreground of a style that has none.
func unstyled() lipgloss.TerminalColor { return lipgloss.NewStyle().GetForeground() }

// distinct fails the test unless every named style has a colour of its own.
func distinct(t *testing.T, styles map[string]lipgloss.Style) {
	t.Helper()
	seen := map[lipgloss.TerminalColor]string{}
	for name, s := range styles {
		c := s.GetForeground()
		if c == unstyled() {
			t.Errorf("%s has no colour", name)
			continue
		}
		if other, ok := seen[c]; ok {
			t.Errorf("%s and %s share the colour %v", other, name, c)
		}
		seen[c] = name
	}
}

func TestSeveritiesAreToldApartByColour(t *testing.T) {
	distinct(t, map[string]lipgloss.Style{
		review.SeverityHigh:   severityStyle(review.SeverityHigh),
		review.SeverityMedium: severityStyle(review.SeverityMedium),
		review.SeverityLow:    severityStyle(review.SeverityLow),
	})
	if got := severityStyle(review.SeverityInfo).GetForeground(); got != unstyled() {
		t.Errorf("info is coloured %v, want none", got)
	}
}

func TestDiffRowsAreToldApartByColour(t *testing.T) {
	distinct(t, map[string]lipgloss.Style{
		review.LineAdded:   rowStyle(review.LineAdded),
		review.LineRemoved: rowStyle(review.LineRemoved),
		rowMarked:          rowStyle(rowMarked),
		rowSuggestion:      rowStyle(rowSuggestion),
	})
	if got := rowStyle(review.LineContext).GetForeground(); got != unstyled() {
		t.Errorf("a context line is coloured %v, want none", got)
	}
}

// ansiProfile is termenv.ANSI, the profile the palette's ANSI 0-15 colours
// are emitted under, as a plain int so this package does not import
// termenv.
const ansiProfile = 2

// sgr is the escape sequence a style paints with, taken from a render of
// the style's own.
func sgr(t *testing.T, style lipgloss.Style) string {
	t.Helper()
	r := style.Render("x")
	i := strings.Index(r, "x")
	if i <= 0 {
		t.Fatalf("a style with the foreground %v rendered %q with no escape sequence: is the colour profile off?", style.GetForeground(), r)
	}
	return r[:i]
}

// TestTheScreenIsActuallyPainted pins that the styles reach the screen: a
// marked line, an added line, the severity and the focused pane's title and
// border are painted in their own styles, and painting moves nothing.
func TestTheScreenIsActuallyPainted(t *testing.T) {
	saved := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(ansiProfile)
	defer lipgloss.SetColorProfile(saved)

	m := screen(t, queueOf(t, judged(t)))
	v := view(m)
	rows, _ := diffRows(m.(Model).view)
	var marked, added, removed, suggestion string
	first := func(have *string, text string) {
		if *have == "" {
			*have = text
		}
	}
	for _, r := range rows {
		switch r.kind {
		case rowMarked:
			first(&marked, r.text)
		case review.LineAdded:
			first(&added, r.text)
		case review.LineRemoved:
			first(&removed, r.text)
		case rowSuggestion:
			first(&suggestion, r.text)
		}
	}
	for _, want := range []struct{ what, painted string }{
		{"a marked line", markedStyle.Render(marked)},
		{"an added line", addedStyle.Render(added)},
		{"a removed line", removedStyle.Render(removed)},
		{"a suggestion line", suggestionStyle.Render(suggestion)},
		{"the severity", severityStyle(review.SeverityHigh).Render("high")},
		{"the focused pane's title", sgr(t, titleStyle) + "▸ Diff · gather.go"},
		{"the focused pane's border", sgr(t, titleStyle.UnsetBold()) + "╭"},
		{"the other pane's title", sgr(t, plainTitleStyle) + "Finding"},
	} {
		if !strings.Contains(v, want.painted) {
			t.Errorf("%s is not painted %q:\n%s", want.what, want.painted, v)
		}
	}
	// The footer's notice is painted as a warning.
	m, _ = press(m, "d", "enter")
	if !strings.Contains(view(m), warnStyle.Render("a dismissal needs a reason: it is what your reviewer notes are made of")) {
		t.Errorf("the refusal is not painted as a warning:\n%s", view(m))
	}

	lipgloss.SetColorProfile(saved)
	if stripped := plain(v); stripped != view(screen(t, queueOf(t, judged(t)))) {
		t.Errorf("colour moved the screen:\n--- painted, stripped ---\n%s\n--- unpainted ---\n%s", stripped, view(screen(t, queueOf(t, judged(t)))))
	}
}
