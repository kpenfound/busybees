package mcpserver

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/state"
)

// fakeNotes is a Notes backend that fails: the error paths need one, the
// success path runs against the real file backend.
type fakeNotes struct{ err error }

func (f fakeNotes) ReadNotes(context.Context, string) (string, error) { return "", f.err }
func (f fakeNotes) WriteNotes(context.Context, string, string) error  { return f.err }

// fileNotesHarness is a harness whose notes tools are backed by the notes
// files under its own state dir, wired the way `bees mcp serve` wires them.
func fileNotesHarness(t *testing.T, role string) (*harness, *state.Store) {
	t.Helper()
	stateDir := t.TempDir()
	st := state.New(stateDir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	env := Env{Role: role, StateDir: stateDir, SessionDir: sessionDir, Issue: 36, PR: 72}
	client, srv, err := Connect(context.Background(), env, Deps{Notes: FileNotes(st)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = srv.Wait()
	})
	return &harness{t: t, client: client, env: env, sessionDir: sessionDir}, st
}

// A role's notes round-trip through the file backend byte for byte: the
// first read of a role that never ran is the section skeleton, a write
// replaces the whole file `bees notes show` reads, and an edit made behind
// the tool's back (a person, `bees notes add`) is what the next read sees.
func TestNotesRoundTripThroughTheFileStore(t *testing.T) {
	for _, role := range config.Roles {
		h, st := fileNotesHarness(t, role)
		if got := h.call("notes_read", nil); got != state.NotesSkeleton(role) {
			t.Errorf("role %q: first read = %q, want the skeleton %q", role, got, state.NotesSkeleton(role))
		}
		const notes = "# notes\n\n## Project facts\n\n- run `dagger check` before committing\n\n## Decisions\n\n- keep the file backend\n"
		if got := h.call("notes_write", map[string]any{"text": notes}); !strings.Contains(got, "notes replaced") {
			t.Errorf("role %q: write result %q", role, got)
		}
		if got, err := st.ReadNotes(role); err != nil || got != notes {
			t.Errorf("role %q: file after the write = %q, %v; want %q", role, got, err, notes)
		}
		if got := h.call("notes_read", nil); got != notes {
			t.Errorf("role %q: read back %q, want %q", role, got, notes)
		}
		// A second write keeps nothing of the first.
		h.call("notes_write", map[string]any{"text": "# rewritten\n"})
		if got, _ := st.ReadNotes(role); got != "# rewritten\n" {
			t.Errorf("role %q: second write left %q", role, got)
		}
		if err := st.AppendNotes(role, "added by a person"); err != nil {
			t.Fatal(err)
		}
		if got := h.call("notes_read", nil); got != "# rewritten\n\n- added by a person\n" {
			t.Errorf("role %q: read after an edit behind the tool = %q", role, got)
		}
	}
}

// An empty text is refused: the write replaces the whole file and nothing
// archives what it replaces, so a slip would erase the role's memory.
func TestNotesWriteRefusesAnEmptyText(t *testing.T) {
	h, st := fileNotesHarness(t, config.RoleDeveloper)
	h.call("notes_write", map[string]any{"text": "keep me\n"})
	for _, empty := range []string{"", "  \n\t"} {
		res := h.callRaw("notes_write", map[string]any{"text": empty})
		if !res.IsError || !strings.Contains(resultText(res), "complete text") {
			t.Errorf("text %q: %v %q", empty, res.IsError, resultText(res))
		}
	}
	if got, _ := st.ReadNotes(config.RoleDeveloper); got != "keep me\n" {
		t.Errorf("an empty write changed the file: %q", got)
	}
}

// Without a backend the tools are unavailable, the same way the issue and
// GitHub tools are; a backend that cannot answer passes its error through;
// and outside a session there is no role whose notes they would be.
func TestNotesToolsWithoutABackend(t *testing.T) {
	for name, deps := range map[string]Deps{
		"nil":      {},
		"erroring": {Notes: fakeNotes{err: errors.New("notes: no such file")}},
	} {
		h := newHarness(t, config.RoleQA, deps)
		for tool, args := range map[string]map[string]any{
			"notes_read":  nil,
			"notes_write": {"text": "t"},
		} {
			res := h.callRaw(tool, args)
			if !res.IsError {
				t.Errorf("%s/%s: expected an error, got %q", name, tool, resultText(res))
			}
			if name == "nil" && !strings.Contains(resultText(res), "bees.toml") {
				t.Errorf("%s/%s: %q does not say why", name, tool, resultText(res))
			}
		}
	}
	h, st := fileNotesHarness(t, "")
	for tool, args := range map[string]map[string]any{
		"notes_read":  nil,
		"notes_write": {"text": "t"},
	} {
		res := h.callRaw(tool, args)
		if !res.IsError || !strings.Contains(resultText(res), "BEES_ROLE") {
			t.Errorf("no role/%s: %v %q", tool, res.IsError, resultText(res))
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(st.Dir, "notes")); len(entries) != 0 {
		t.Errorf("a call without a role wrote %d notes files", len(entries))
	}
}

// The descriptions carry the rule the prompt no longer states in full: the
// notes are not in the prompt, so read first, and a write is the whole text.
func TestNotesToolDescriptionsCarryTheRules(t *testing.T) {
	h := newHarness(t, "", Deps{})
	want := map[string][]string{
		"notes_read":  {"only memory", "Nothing renders them into the prompt", "start of a session"},
		"notes_write": {"whole text", "notes_read", "before you report your outcome", "empty text is refused"},
	}
	for _, tool := range h.tools() {
		phrases, ok := want[tool.Name]
		if !ok {
			continue
		}
		for _, p := range phrases {
			if !strings.Contains(tool.Description, p) {
				t.Errorf("%s description lacks %q:\n%s", tool.Name, p, tool.Description)
			}
		}
		delete(want, tool.Name)
	}
	for name := range want {
		t.Errorf("no %s tool", name)
	}
}
