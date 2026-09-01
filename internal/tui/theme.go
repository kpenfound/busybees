package tui

import (
	"github.com/charmbracelet/lipgloss"

	"github.com/kpenfound/busybees/internal/config"
)

// The view's palette, and every style built from it. This file is the only
// place in the package that names a colour: a person who wants to repaint
// `bees run` changes it here and nowhere else, and TestOnlyTheThemeNamesColours
// keeps it that way.
//
// The colours are the terminal's own, ANSI 0-15, rather than hex values. A
// person running `bees run` has already chosen a palette their terminal is
// readable in, whether it is light or dark, and these are the entries of it:
// asking for "yellow" gets whatever yellow they picked. Hand-picked hex pairs
// would need two sets to stay readable and would override the choice they had
// already made.
const (
	// colorTitle paints the header's name and every panel's title and
	// border: the furniture of the view, told apart from what it holds.
	colorTitle = lipgloss.Color("6") // cyan

	// The three colours a finished session is read by, and they are the
	// three a person scanning the Recent panel is really asking about: did
	// it work, does it want me, did it break. colorWarn is what the header
	// says a status.json it could not read in, and what the Needs human
	// panel is drawn in while it holds anything — the same question in a
	// different place.
	colorClean     = lipgloss.Color("2") // green: it worked
	colorAttention = lipgloss.Color("3") // yellow: it wants a person
	colorBad       = lipgloss.Color("1") // red: it failed
	colorWarn      = colorAttention

	// One colour per role, so the Now panel can be read as who is working
	// rather than by spelling out each row. None of them is red or yellow:
	// a role is not a verdict, and a developer's row must never read as an
	// alarm.
	colorProductManager = lipgloss.Color("5")  // magenta
	colorProjectManager = lipgloss.Color("4")  // blue
	colorDeveloper      = lipgloss.Color("6")  // cyan
	colorReviewer       = lipgloss.Color("2")  // green
	colorQA             = lipgloss.Color("11") // bright yellow
)

// The styles the whole package paints with. Every style a row is rendered
// through is foreground-only — no Width, no Padding, no Margin — because a
// row is coloured after it has been laid out and cut to the panel's width,
// and anything that changed its size would undo both.
var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorTitle)
	headerStyle = lipgloss.NewStyle().Faint(true)
	hintStyle   = lipgloss.NewStyle().Faint(true)
	warnStyle   = lipgloss.NewStyle().Foreground(colorWarn)
	panelStyle  = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)

	// The titles of the two panels that hold what is waiting for a person,
	// drawn while they hold anything. Picking the panel out rather than its
	// rows is what keeps this away from the row layout entirely.
	warnTitleStyle  = lipgloss.NewStyle().Bold(true).Foreground(colorWarn)
	cleanTitleStyle = lipgloss.NewStyle().Bold(true).Foreground(colorClean)

	cleanStyle     = lipgloss.NewStyle().Foreground(colorClean)
	attentionStyle = lipgloss.NewStyle().Foreground(colorAttention)
	badStyle       = lipgloss.NewStyle().Foreground(colorBad)
)

// roleColors is the one place a role is given its colour. A role missing
// from it is drawn unstyled rather than sharing another role's colour, which
// would say something false.
var roleColors = map[string]lipgloss.Color{
	config.RoleProductManager: colorProductManager,
	config.RoleProjectManager: colorProjectManager,
	config.RoleDeveloper:      colorDeveloper,
	config.RoleReviewer:       colorReviewer,
	config.RoleQA:             colorQA,
}

// roleStyle is the colour a running session's row is drawn in. An unknown
// role gets no colour at all: the row still names it, so nothing is lost.
func roleStyle(role string) lipgloss.Style {
	c, ok := roleColors[role]
	if !ok {
		return lipgloss.NewStyle()
	}
	return lipgloss.NewStyle().Foreground(c)
}

// outcomeStyle is the colour a finished session's row is drawn in, by what
// the session reported. The three classes are the three answers a person is
// scanning for, and every status in session.validOutcomes is in one of them
// — TestEveryOutcomeIsColoured is what says so, so a status added later
// cannot quietly lose its colour.
//
// The empty string is what a session that never reported an outcome leaves
// behind, which outcomeText renders as "no outcome": a failure, and coloured
// as one.
func outcomeStyle(outcome string) lipgloss.Style {
	switch outcome {
	case "pr-opened", "pr-updated", "approved", "done":
		return cleanStyle
	case "changes-requested", "question", "idle":
		return attentionStyle
	case "failed", "":
		return badStyle
	}
	return lipgloss.NewStyle()
}

// boxStyle is the border a panel is drawn with, painted to match the style
// its title is drawn in. It is here rather than in panel() so that colour is
// named in one file: panel() is handed a style and never has to know that a
// border has a colour of its own.
func boxStyle(title lipgloss.Style) lipgloss.Style {
	return panelStyle.BorderForeground(title.GetForeground())
}
