package reviewtui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/kpenfound/busybees/internal/review"
)

// The screen's palette, and every style built from it. This file is the
// only place in the package that names a colour: a person who wants to
// repaint the triage screen changes it here and nowhere else, and
// TestOnlyTheThemeNamesColours keeps it that way. It is the same rule
// internal/tui keeps for `bees run`'s view, and a palette of its own: a diff
// line and a finding's severity mean nothing to the factory view, and the
// factory view's roles and outcomes mean nothing here.
//
// The colours are the terminal's own, ANSI 0-15, rather than hex values, for
// the reason internal/tui/theme.go gives: the person has already chosen a
// palette their terminal is readable in, and these are entries of it.
const (
	// colorTitle paints the panel titles and borders: the furniture of the
	// screen, told apart from what it holds.
	colorTitle = lipgloss.Color("6") // cyan

	// The two colours a diff is read by, the ones every diff viewer uses.
	colorAdded   = lipgloss.Color("2") // green
	colorRemoved = lipgloss.Color("1") // red
	// colorMarked paints the lines the finding is about, in the diff and in
	// its gutter, and colorSuggestion the text the finding would put in
	// their place.
	colorMarked     = lipgloss.Color("3") // yellow
	colorSuggestion = lipgloss.Color("5") // magenta

	// One colour per severity, most alarming first, so the header can be
	// read at a glance. Info is left uncoloured: it is the severity that
	// asks for the least attention, and no colour is what says so.
	colorHigh   = lipgloss.Color("1") // red
	colorMedium = lipgloss.Color("3") // yellow
	colorLow    = lipgloss.Color("4") // blue

	// colorWarn paints what the person has to read before pressing the next
	// key: an action the queue refused, an error.
	colorWarn = lipgloss.Color("3") // yellow
)

// The styles the whole package paints with. Every style a row is rendered
// through is foreground-only, no Width, no Padding, no Margin, because a
// row is coloured after it has been laid out and cut to its pane's width,
// and anything that changed its size would undo both.
var (
	titleStyle      = lipgloss.NewStyle().Bold(true).Foreground(colorTitle)
	plainTitleStyle = lipgloss.NewStyle().Bold(true)
	hintStyle       = lipgloss.NewStyle().Faint(true)
	warnStyle       = lipgloss.NewStyle().Foreground(colorWarn)
	panelStyle      = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)

	fileStyle       = lipgloss.NewStyle().Bold(true)
	hunkStyle       = lipgloss.NewStyle().Faint(true)
	addedStyle      = lipgloss.NewStyle().Foreground(colorAdded)
	removedStyle    = lipgloss.NewStyle().Foreground(colorRemoved)
	markedStyle     = lipgloss.NewStyle().Bold(true).Foreground(colorMarked)
	suggestionStyle = lipgloss.NewStyle().Foreground(colorSuggestion)

	highStyle   = lipgloss.NewStyle().Bold(true).Foreground(colorHigh)
	mediumStyle = lipgloss.NewStyle().Foreground(colorMedium)
	lowStyle    = lipgloss.NewStyle().Foreground(colorLow)
)

// rowStyle is the style a row of the diff pane is painted in, by its kind
// (view.go). A kind not named here is drawn unstyled.
func rowStyle(kind string) lipgloss.Style {
	switch kind {
	case rowFile:
		return fileStyle
	case rowHunk, rowNote:
		return hunkStyle
	case rowMarked:
		return markedStyle
	case rowSuggestion:
		return suggestionStyle
	case review.LineAdded:
		return addedStyle
	case review.LineRemoved:
		return removedStyle
	}
	return lipgloss.NewStyle()
}

// severityStyle is the style a finding's severity is painted in. Info, and
// anything the schema does not have, is drawn unstyled.
func severityStyle(severity string) lipgloss.Style {
	switch severity {
	case review.SeverityHigh:
		return highStyle
	case review.SeverityMedium:
		return mediumStyle
	case review.SeverityLow:
		return lowStyle
	}
	return lipgloss.NewStyle()
}

// boxStyle is the border a panel is drawn with, painted to match the style
// its title is drawn in, so that a border's colour is named in one file.
func boxStyle(title lipgloss.Style) lipgloss.Style {
	return panelStyle.BorderForeground(title.GetForeground())
}
