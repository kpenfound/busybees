package tui

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
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

// ansiProfile is termenv.ANSI, the profile the palette's ANSI 0-15 colours
// are emitted under. It is a plain int so this package does not import
// termenv, which is an indirect dependency and would churn go.mod.
const ansiProfile = 2

// sgr is the escape sequence a style paints with, taken from a render of the
// style's own. Comparing against this rather than against a literal escape
// sequence keeps the assertion about the palette rather than about how
// lipgloss spells it.
func sgr(t *testing.T, style lipgloss.Style) string {
	t.Helper()
	r := style.Render("x")
	i := strings.Index(r, "x")
	if i <= 0 {
		t.Fatalf("a style with the foreground %v rendered %q with no escape sequence: is the colour profile off?",
			style.GetForeground(), r)
	}
	return r[:i]
}

// paintedModel is a view with a row of every colour in it: one running
// session per role, one finished session per outcome class, and something in
// each of the two panels that wait for a person.
func paintedModel(t *testing.T) Model {
	t.Helper()
	msgs := []tea.Msg{
		tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight},
		started("pm", config.RoleProductManager, 0, 0, fixed, "claude-opus-5", false),
		started("pjm", config.RoleProjectManager, 0, 0, fixed, "claude-opus-5", false),
		started("dev", config.RoleDeveloper, 1, 0, fixed, "claude-opus-5", false),
		started("rev", config.RoleReviewer, 1, 9, fixed, "claude-opus-5", false),
		started("qa", config.RoleQA, 0, 0, fixed, "claude-sonnet-5", false),
		endedAs("dev-2", config.RoleDeveloper, 2, 0, "approved", "clean", 0.1, time.Second),
		endedAs("dev-3", config.RoleDeveloper, 3, 0, "changes-requested", "attention", 0.1, time.Second),
		endedAs("dev-4", config.RoleDeveloper, 4, 0, "failed", "bad", 0.1, time.Second),
		statusMsg{status: state.Status{
			NeedsHuman: []state.Escalated{escalated(5, "stuck", "why", fixed)},
			Approved:   []state.ApprovedPR{approvedPR(6, 7, "done", fixed)},
		}},
	}
	var m tea.Model = New(Deps{Repo: "acme/widgets", Now: func() time.Time { return fixed }})
	for _, msg := range msgs {
		m, _ = m.Update(msg)
	}
	return m.(Model)
}

// TestTheViewIsActuallyPainted is what says the palette reaches the screen.
// Every other test in this file asserts on a style object, and a style object
// cannot tell whether anything renders through it: replacing roleStyle(s.role)
// in nowPanel with an unstyled style deletes the feature and leaves the
// package green. `go test` is not a terminal, so lipgloss detects a profile
// with no colour in it and renders every style as a no-op; the profile is
// package-global state, no test in this package runs in parallel, and the
// defer puts back whatever was detected.
func TestTheViewIsActuallyPainted(t *testing.T) {
	saved := lipgloss.ColorProfile()
	lipgloss.SetColorProfile(ansiProfile)
	defer lipgloss.SetColorProfile(saved)

	m := paintedModel(t)
	_, inner := m.dims()

	// A running session's row is painted by its role.
	now := strings.Split(m.nowPanel(inner, len(m.sessions), 0), "\n")[1:]
	if len(now) != len(config.Roles) {
		t.Fatalf("the Now panel drew %d rows, want one per role", len(now))
	}
	for i, role := range config.Roles {
		if want := sgr(t, roleStyle(role)); !strings.HasPrefix(now[i], want) {
			t.Errorf("the %s row is painted %q, want the role's own %q", role, prefix(now[i]), want)
		}
	}

	// A finished session's row is painted by the class of its outcome.
	recent := strings.Split(m.recentPanel(inner, len(m.recent), 0), "\n")[1:]
	for i, outcome := range []string{"failed", "changes-requested", "approved"} {
		if want := sgr(t, outcomeStyle(outcome)); !strings.HasPrefix(recent[i], want) {
			t.Errorf("a %s row is painted %q, want its class's %q", outcome, prefix(recent[i]), want)
		}
	}

	// A panel's title and border are painted in the style View picks for it,
	// so the two panels that wait for a person are picked out on screen and
	// not merely in panelStyleOf.
	view := m.View()
	for _, p := range []struct {
		title string
		style lipgloss.Style
	}{
		{"Now", titleStyle}, {"Recent", titleStyle},
		{"Needs human", warnTitleStyle}, {"Approved PRs", cleanTitleStyle},
	} {
		// The border carries the title's colour without its weight, which
		// is what boxStyle builds it from.
		title, border := sgr(t, p.style), sgr(t, p.style.UnsetBold())
		if !strings.Contains(view, title+p.title) {
			t.Errorf("the %s panel's title is not painted %q", p.title, title)
		}
		if !strings.Contains(view, border+"╭") {
			t.Errorf("the %s panel's border is not painted %q to match its title", p.title, border)
		}
	}

	// Painting a row changes nothing about where it sits: the styles are
	// foreground-only and are applied after the row has been laid out and
	// cut, so the view a terminal with colour is given is the view without
	// it, with escape sequences added.
	lipgloss.SetColorProfile(saved)
	if stripped := plain(view); stripped != m.View() {
		t.Errorf("colour moved the view:\n--- painted, stripped ---\n%s\n--- unpainted ---\n%s", stripped, m.View())
	}
}

// prefix is the escape sequence a rendered line starts with, for a failure
// message that would otherwise print the whole row. A row that carries no
// escape sequence at all is the ordinary failure here, so it says that
// rather than printing eighty columns of unpainted text.
func prefix(line string) string {
	if loc := ansi.FindStringIndex(line); loc != nil && loc[0] == 0 {
		return line[:loc[1]]
	}
	return "no escape sequence"
}
