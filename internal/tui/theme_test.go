package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// colourTokens are the ways a colour can be named in this package. They are
// built by concatenation so that this file, which has to hold them as data,
// does not match its own search — the same reason theme.go itself is the one
// file the sweep below skips.
var colourTokens = []string{
	"lipgloss." + "Color(",
	"lipgloss." + "AdaptiveColor",
	"." + "Foreground(",
	"." + "BorderForeground(",
}

// TestOnlyTheThemeNamesColours is the palette pin: the whole point of
// theme.go is that a person repainting `bees run` has one file to read, and
// a colour set anywhere else would be invisible to them. It reads the
// package's own sources rather than parsing them, because the claim is about
// what a reader finds, not about what the compiler sees.
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

// TestRolesAreToldApartByColour pins that the Now panel can be read by who
// is working: five roles, five different colours. It asserts on the style
// rather than on a rendered row because `go test` is not a terminal, so
// lipgloss renders every colour as nothing at all and an assertion about the
// escape sequences would pass whatever the colours were. GetForeground
// answers with the colour that was stored whatever the profile.
func TestRolesAreToldApartByColour(t *testing.T) {
	seen := map[lipgloss.TerminalColor]string{}
	for _, role := range config.Roles {
		c := roleStyle(role).GetForeground()
		if c == unstyled() {
			t.Errorf("%s has no colour", role)
			continue
		}
		if other, ok := seen[c]; ok {
			t.Errorf("%s and %s share the colour %v", other, role, c)
		}
		seen[c] = role
	}
	if got := len(seen); got != len(config.Roles) {
		t.Errorf("%d distinct role colours, want %d", got, len(config.Roles))
	}
	if got := roleStyle("nobody").GetForeground(); got != unstyled() {
		t.Errorf("an unknown role got the colour %v, want none", got)
	}
}

// TestOutcomeClassesAreToldApartByColour pins the three answers a person
// scanning the Recent panel is asking: did it work, does it want me, did it
// break. One named outcome from each class, plus the empty string a session
// that reported nothing leaves behind — outcomeText renders that as "no
// outcome", and it is a failure.
func TestOutcomeClassesAreToldApartByColour(t *testing.T) {
	clean := outcomeStyle("approved").GetForeground()
	attention := outcomeStyle("changes-requested").GetForeground()
	bad := outcomeStyle("failed").GetForeground()
	for _, c := range []struct {
		name  string
		color lipgloss.TerminalColor
	}{{"clean", clean}, {"attention", attention}, {"bad", bad}} {
		if c.color == unstyled() {
			t.Errorf("the %s class has no colour", c.name)
		}
	}
	if clean == attention || clean == bad || attention == bad {
		t.Errorf("the three outcome classes are not three colours: clean %v, attention %v, bad %v",
			clean, attention, bad)
	}
	if got := outcomeStyle("").GetForeground(); got != bad {
		t.Errorf("a session that reported no outcome got %v, want the failure colour %v", got, bad)
	}
	if got := outcomeStyle("dithering").GetForeground(); got != unstyled() {
		t.Errorf("an unknown outcome got the colour %v, want none", got)
	}
}

// TestEveryOutcomeIsColoured walks every status every role may report, so a
// status added to session.validOutcomes later cannot quietly fall through to
// the unstyled default and leave one kind of row reading as plain text.
func TestEveryOutcomeIsColoured(t *testing.T) {
	var walked []string
	for _, role := range config.Roles {
		for _, status := range session.ValidOutcomes(role) {
			if slices.Contains(walked, status) {
				continue
			}
			walked = append(walked, status)
			if outcomeStyle(status).GetForeground() == unstyled() {
				t.Errorf("%s reports %q and it has no colour: add it to outcomeStyle", role, status)
			}
		}
	}
	if len(walked) == 0 {
		t.Fatal("walked no outcomes: session.ValidOutcomes answered nothing for any role")
	}
}

// unstyled is the foreground of a style that was never given one, which is
// what roleStyle and outcomeStyle answer for something they do not know.
func unstyled() lipgloss.TerminalColor { return lipgloss.NewStyle().GetForeground() }

// TestTheWaitingPanelsArePickedOut pins how the two panels that hold what is
// waiting for a person are drawn: in a colour of their own while they hold
// anything, and like every other panel when they are empty, so a view where
// nothing needs a person reads as ordinary. The colour is on the panel and
// not on its rows, which is what keeps it clear of the row layout.
func TestTheWaitingPanelsArePickedOut(t *testing.T) {
	plainTitle := titleStyle.GetForeground()

	var m Model
	for i, title := range panelTitles {
		if got := m.panelStyleOf(i).GetForeground(); got != plainTitle {
			t.Errorf("the empty %s panel is drawn in %v, want the ordinary title colour %v", title, got, plainTitle)
		}
	}

	m.status.NeedsHuman = []state.Escalated{{Issue: 1}}
	m.status.Approved = []state.ApprovedPR{{PR: 2}}
	needsHuman := m.panelStyleOf(panelNeedsHuman).GetForeground()
	approved := m.panelStyleOf(panelApproved).GetForeground()
	if needsHuman == plainTitle {
		t.Errorf("a Needs human panel with something in it is drawn like every other panel (%v)", needsHuman)
	}
	if approved == plainTitle {
		t.Errorf("an Approved PRs panel with something in it is drawn like every other panel (%v)", approved)
	}
	if needsHuman == approved {
		t.Errorf("both waiting panels are drawn in %v: what is stuck and what is done read alike", needsHuman)
	}
	// The panels that are not about waiting for a person keep their colour
	// whatever those two hold.
	for _, i := range []int{0, 1} {
		if got := m.panelStyleOf(i).GetForeground(); got != plainTitle {
			t.Errorf("the %s panel is drawn in %v, want the ordinary title colour %v", panelTitles[i], got, plainTitle)
		}
	}
}
