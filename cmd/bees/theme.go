package main

import "github.com/charmbracelet/lipgloss"

// The palette of the review progress view (reviewprogress.go), the one
// thing this package draws with lipgloss. Every colour the package names is
// here, and TestOnlyTheThemeNamesColours keeps it that way, the rule
// internal/tui and internal/reviewtui each keep for their own theme.go: a
// person repainting the view has one file to read. The colours are the
// terminal's own ANSI ones, for the reason those files give: the person
// has already chosen a terminal theme, and these fit whatever it is.
var (
	colorSpinner  = lipgloss.Color("6") // cyan
	colorFinished = lipgloss.Color("2") // green
	colorFailed   = lipgloss.Color("1") // red
)

var (
	styleSpinner  = lipgloss.NewStyle().Foreground(colorSpinner)
	styleFinished = lipgloss.NewStyle().Foreground(colorFinished)
	styleFailed   = lipgloss.NewStyle().Foreground(colorFailed)
)
