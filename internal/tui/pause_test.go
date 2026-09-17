package tui

import (
	"slices"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/state"
)

// pauses records what p asked the factory to do, in order.
type pauses struct{ calls []bool }

func (p *pauses) set(paused bool) { p.calls = append(p.calls, paused) }

func pKey() tea.Msg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("p")} }

// update drives a model through msgs and returns it.
func update(t *testing.T, d Deps, msgs ...tea.Msg) Model {
	t.Helper()
	if d.Now == nil {
		d.Now = func() time.Time { return fixed }
	}
	var m tea.Model = New(d)
	m, _ = m.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	for _, msg := range msgs {
		m, _ = m.Update(msg)
	}
	return m.(Model)
}

// p pauses dispatch and p again resumes it, the footer offering whichever is
// next and the header saying the factory is paused by hand while it is.
func TestPTogglesAManualPause(t *testing.T) {
	var got pauses
	d := Deps{Repo: "acme/widgets", SetPaused: got.set}

	m := update(t, d)
	if !strings.Contains(m.footer(), "p pause") {
		t.Errorf("the running footer does not offer p pause: %q", m.footer())
	}

	m = update(t, d, pKey())
	if !slices.Equal(got.calls, []bool{true}) {
		t.Fatalf("SetPaused calls after one p: %v, want [true]", got.calls)
	}
	if h := header(plain(m.View())); !strings.Contains(h, "⏸ paused by hand (p resumes)") {
		t.Errorf("the header does not say the factory is paused by hand: %q", h)
	}
	m.notice = ""
	if !strings.Contains(m.footer(), "p resume") {
		t.Errorf("the paused footer does not offer p resume: %q", m.footer())
	}

	got.calls = nil
	m = update(t, d, pKey(), pKey())
	if !slices.Equal(got.calls, []bool{true, false}) {
		t.Fatalf("SetPaused calls after two p: %v, want [true false]", got.calls)
	}
	if strings.Contains(header(plain(m.View())), "⏸") {
		t.Errorf("the header still shows a pause after the resume: %q", header(plain(m.View())))
	}
	m.notice = ""
	if !strings.Contains(m.footer(), "p pause") {
		t.Errorf("the resumed footer does not offer p pause: %q", m.footer())
	}
}

// A view that cannot pause neither offers p nor pretends to have paused.
func TestPWithoutSetPausedIsRefused(t *testing.T) {
	m := update(t, Deps{Repo: "acme/widgets"})
	if strings.Contains(m.footer(), "p pause") {
		t.Errorf("the footer offers p with no way to pause: %q", m.footer())
	}
	m = update(t, Deps{Repo: "acme/widgets"}, pKey())
	if m.paused || !strings.Contains(m.footer(), "cannot pause") {
		t.Errorf("p without SetPaused: paused %v, footer %q", m.paused, m.footer())
	}
}

// While the daily budget pause holds the factory p is refused with a notice
// and not offered; a manual pause set before it stays set, and is offered
// for resuming once the budget pause lifts.
func TestPIsRefusedUnderTheBudgetPause(t *testing.T) {
	var got pauses
	d := Deps{Repo: "acme/widgets", SetPaused: got.set}
	budget := statusMsg{status: state.Status{BudgetPaused: true, DaySpendUSD: 6, DayBudgetUSD: 5}}

	m := update(t, d, budget)
	if strings.Contains(m.footer(), "p pause") || strings.Contains(m.footer(), "p resume") {
		t.Errorf("the footer offers p under the budget pause: %q", m.footer())
	}
	m = update(t, d, budget, pKey())
	if len(got.calls) != 0 || m.paused {
		t.Errorf("p under the budget pause: SetPaused calls %v, paused %v", got.calls, m.paused)
	}
	if !strings.Contains(m.footer(), "unavailable while the daily budget pause") {
		t.Errorf("the footer does not say why p was refused: %q", m.footer())
	}

	got.calls = nil
	m = update(t, d, pKey(), budget, pKey(), statusMsg{status: state.Status{DayBudgetUSD: 5}})
	if !slices.Equal(got.calls, []bool{true}) || !m.paused {
		t.Fatalf("pause, budget pause, p, budget lifts: SetPaused calls %v, paused %v", got.calls, m.paused)
	}
	m.notice = ""
	if !strings.Contains(m.footer(), "p resume") {
		t.Errorf("after the budget pause lifted the footer does not offer p resume: %q", m.footer())
	}
	if h := header(plain(m.View())); !strings.Contains(h, "paused by hand") {
		t.Errorf("after the budget pause lifted the header does not say paused by hand: %q", h)
	}
}

// In a daemon's view p pauses the whole machine whichever project the
// selector shows, and is refused only while every project is under its
// budget pause: a pause by hand still stops the others.
func TestPInADaemonsViewPausesTheMachine(t *testing.T) {
	var got pauses
	d := twoProjects()
	d.SetPaused = got.set

	m := update(t, d, right(), pKey())
	if !slices.Equal(got.calls, []bool{true}) {
		t.Fatalf("p with foo selected: SetPaused calls %v, want [true]", got.calls)
	}
	next, _ := m.Update(right())
	mm := next.(Model)
	mm.notice = ""
	if !strings.Contains(mm.footer(), "p resume") {
		t.Errorf("with bar selected the footer does not offer p resume: %q", mm.footer())
	}
	if h := header(plain(mm.View())); !strings.Contains(h, "paused by hand") {
		t.Errorf("bar's header does not say paused by hand: %q", h)
	}

	got.calls = nil
	fooBudget := in(0, statusMsg{status: state.Status{BudgetPaused: true}})
	m = update(t, d, fooBudget, pKey())
	if !slices.Equal(got.calls, []bool{true}) || !m.paused {
		t.Errorf("p with only foo under its budget pause: SetPaused calls %v, paused %v", got.calls, m.paused)
	}

	got.calls = nil
	barBudget := in(1, statusMsg{status: state.Status{BudgetPaused: true}})
	m = update(t, d, fooBudget, barBudget, pKey())
	if len(got.calls) != 0 || !strings.Contains(m.footer(), "unavailable") {
		t.Errorf("p with every project under its budget pause: SetPaused calls %v, footer %q", got.calls, m.footer())
	}
}
