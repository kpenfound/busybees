package tui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/config"
)

// r reloads the configuration from disk through the factory's own reload,
// off the view's goroutine: the footer says it is happening and then what
// came of it, and the header keeps the time of the last reload. The key is
// advertised in the footer, with and without a session running.
func TestRReloadsTheConfigurationAndTheViewSaysSo(t *testing.T) {
	reloads := 0
	var view tea.Model = New(Deps{Now: func() time.Time { return fixed }, Reload: func() error { reloads++; return nil }})
	view, _ = view.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	if got := plain(view.View()); !strings.Contains(got, "r reload") {
		t.Errorf("the footer does not advertise r with nothing running:\n%s", got)
	}
	view, _ = view.Update(started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed, "opus", false))
	if got := plain(view.View()); !strings.Contains(got, "r reload") {
		t.Errorf("the footer does not advertise r with a session running:\n%s", got)
	}

	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if reloads != 0 {
		t.Fatal("the key reloaded on the view's goroutine")
	}
	if got := plain(view.View()); !strings.Contains(got, "reloading the configuration") {
		t.Errorf("the footer does not say a reload is running:\n%s", got)
	}
	// A second press while one runs starts no other.
	view, second := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if second != nil {
		t.Error("a second r started a second reload")
	}
	msg := runCmd(t, cmd)
	if reloads != 1 {
		t.Fatalf("the command reloaded %d times, want 1", reloads)
	}
	if got, ok := msg.(reloadedMsg); !ok || got.err != nil || !got.at.Equal(fixed) {
		t.Fatalf("the reload reported %+v", msg)
	}
	view, _ = view.Update(msg)
	got := plain(view.View())
	if !strings.Contains(got, "configuration reloaded") {
		t.Errorf("the footer does not report the reload:\n%s", got)
	}
	if !strings.Contains(got, "config reloaded 10:03:08") {
		t.Errorf("the header does not show when the configuration was reloaded:\n%s", got)
	}
	// The hints come back on the next key; the header keeps the time.
	view, _ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	got = plain(view.View())
	if !strings.Contains(got, "config reloaded 10:03:08") || !strings.Contains(got, "r reload") {
		t.Errorf("the header lost the last reload or the footer its hints:\n%s", got)
	}
	// And the key works again once the first reload is done.
	if _, cmd = view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}}); cmd == nil {
		t.Error("r after a finished reload started nothing")
	}
}

// A reload the factory refuses — the file does not load, or changes a key
// that cannot change while it runs — leaves the previous configuration in
// force, and the view says so: the reason in the footer, the refusal in the
// header until a later reload is accepted.
func TestARefusedReloadKeepsThePreviousConfigurationAndSaysWhy(t *testing.T) {
	err := errors.New("bees.toml: project.state_dir cannot change while the factory runs; restart bees run to apply it")
	var view tea.Model = New(Deps{Now: func() time.Time { return fixed }, Reload: func() error { return err }})
	view, _ = view.Update(tea.WindowSizeMsg{Width: defaultWidth, Height: panelHeight})
	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	msg := runCmd(t, cmd)
	if got, ok := msg.(reloadedMsg); !ok || got.err != err {
		t.Fatalf("the refusal was not reported: %+v", msg)
	}
	view, _ = view.Update(msg)
	got := plain(view.View())
	for _, want := range []string{"previous configuration kept", "project.state_dir cannot change", "config reload refused 10:03:08"} {
		if !strings.Contains(got, want) {
			t.Errorf("the view does not say %q:\n%s", want, got)
		}
	}
	// A reload accepted later replaces the refusal in the header.
	later := fixed.Add(time.Minute)
	view, _ = view.Update(reloadedMsg{at: later})
	got = plain(view.View())
	if strings.Contains(got, "refused") || !strings.Contains(got, "config reloaded 10:04:08") {
		t.Errorf("an accepted reload did not replace the refusal:\n%s", got)
	}
}

// A view with nothing to reload through says so instead of pretending.
func TestRSaysWhenTheViewCannotReload(t *testing.T) {
	var view tea.Model = New(Deps{Now: func() time.Time { return fixed }})
	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	if cmd != nil {
		t.Error("r without a reload returned a command")
	}
	if got := plain(view.View()); !strings.Contains(got, "this view cannot reload the configuration") {
		t.Errorf("the footer does not say the view cannot reload:\n%s", got)
	}
	// And it does not advertise a key it has nothing behind.
	view, _ = view.Update(tea.KeyMsg{Type: tea.KeyDown})
	if got := plain(view.View()); strings.Contains(got, "r reload") {
		t.Errorf("a view that cannot reload advertises r:\n%s", got)
	}
}

// lastLines is the last n lines of a rendered view: its footer.
func lastLines(view string, n int) []string {
	lines := strings.Split(view, "\n")
	return lines[len(lines)-n:]
}

// Why a reload was refused survives a terminal of ordinary width: the
// notice is wrapped rather than cut, so the key, the reason and every
// project's file are all on screen — for a machine reload that several
// projects refused too — and the view is still no taller than the terminal.
func TestARefusalIsReadableInA100ColumnTerminal(t *testing.T) {
	const dir = "/Users/somebody/src/github.com/acme/a-repository-with-a-long-name"
	err := errors.Join(
		errors.New("project.state_dir, scheduler.max_developers cannot change while the factory runs ("+dir+"/widgets/bees.toml); restart bees run to apply it"),
		errors.New("project.branch_prefix cannot change while the factory runs ("+dir+"/gadgets/bees.toml); restart bees run to apply it"),
	)
	var view tea.Model = New(Deps{Now: func() time.Time { return fixed }, Reload: func() error { return err }})
	view, _ = view.Update(tea.WindowSizeMsg{Width: 100, Height: panelHeight})
	view, _ = view.Update(started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed, "opus", false))
	view, cmd := view.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'r'}})
	view, _ = view.Update(runCmd(t, cmd))
	got := plain(view.View())
	for _, want := range []string{
		"previous configuration kept", "project.state_dir", "scheduler.max_developers", "widgets/bees.toml",
		"; project.branch_prefix cannot change", "gadgets/bees.toml", "restart bees run",
	} {
		if !strings.Contains(strings.Join(strings.Fields(got), " "), want) {
			t.Errorf("the refusal lost %q at 100 columns:\n%s", want, got)
		}
	}
	lines := strings.Split(got, "\n")
	if len(lines) > panelHeight {
		t.Errorf("the wrapped notice made the view %d lines in a %d-line terminal", len(lines), panelHeight)
	}
	for _, line := range lines {
		if w := len([]rune(line)); w > 100 {
			t.Errorf("a %d-column line in a 100-column view: %q", w, line)
		}
	}
	if !strings.Contains(got, "busybees") || !strings.Contains(got, "developer") {
		t.Errorf("the wrapped notice cost the header or the Now panel:\n%s", got)
	}
	// A notice longer than the footer may grow is cut on its last line.
	if n := len(wrap(strings.Repeat("word ", 200), 40, maxNoticeLines)); n != maxNoticeLines {
		t.Errorf("a long notice took %d lines, want %d", n, maxNoticeLines)
	}
}

// With every key on offer the hints are wider than a 100-column terminal.
// The footer stays one line: it goes without the hints that only move
// around the view, never the ones that change what the factory does, and a
// terminal wide enough shows them all.
func TestTheHintsFitTheTerminalByDroppingTheLeastNeeded(t *testing.T) {
	d := Deps{Now: func() time.Time { return fixed }, Reload: func() error { return nil }, SetPaused: func(bool) {}}
	at := func(width int) (Model, string) {
		var view tea.Model = New(d)
		view, _ = view.Update(tea.WindowSizeMsg{Width: width, Height: panelHeight})
		view, _ = view.Update(started("developer-issue-12-r1", config.RoleDeveloper, 12, 0, fixed, "opus", false))
		return view.(Model), lastLines(plain(view.View()), 1)[0]
	}
	m, footer := at(100)
	if w := len([]rune(m.footer())); w <= 100 {
		t.Fatalf("every hint fits in %d columns: this test needs a footer wider than the terminal", w)
	}
	if w := len([]rune(footer)); w > 100 {
		t.Errorf("the footer is %d columns in a 100-column terminal: %q", w, footer)
	}
	if !strings.HasPrefix(footer, "↑↓ select") {
		t.Errorf("the footer took more than one line: %q", footer)
	}
	for _, want := range []string{"k stop session", "p pause", "r reload", "q or ctrl-c stops (sessions finish)"} {
		if !strings.Contains(footer, want) {
			t.Errorf("the 100-column footer lost %q: %q", want, footer)
		}
	}
	if strings.Contains(footer, "o GitHub") || strings.Contains(footer, "…") {
		t.Errorf("the 100-column footer was cut rather than shortened: %q", footer)
	}
	if _, wide := at(140); wide != m.footer() {
		t.Errorf("a 140-column footer is %q, want every hint: %q", wide, m.footer())
	}
	// Narrower than the keys that are never dropped: cut, still one line.
	if _, narrow := at(40); len([]rune(narrow)) > 40 || !strings.HasSuffix(narrow, "…") {
		t.Errorf("a 40-column footer: %q", narrow)
	}
}
