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
}
