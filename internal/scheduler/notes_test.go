package scheduler

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/state"
)

func TestNeedsConsolidation(t *testing.T) {
	const every, maxBytes = 10, 32768
	cases := []struct {
		name     string
		rs       state.RoleState
		notesLen int
		want     bool
	}{
		{"first session of all", state.RoleState{}, 200, false},
		{"count not reached", state.RoleState{Sessions: 12, LastConsolidated: 10}, 200, false},
		{"count reached", state.RoleState{Sessions: 19, LastConsolidated: 10}, 200, true},
		{"count reached from zero", state.RoleState{Sessions: 9}, 200, true},
		{"size exceeded, count not reached", state.RoleState{Sessions: 3, LastConsolidated: 2}, maxBytes + 1, true},
		{"size at the limit is not exceeded", state.RoleState{Sessions: 3, LastConsolidated: 2}, maxBytes, false},
		{"first session with an oversized file", state.RoleState{}, maxBytes + 1, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := needsConsolidation(c.rs, c.notesLen, every, maxBytes); got != c.want {
				t.Errorf("needsConsolidation(%+v, %d) = %v, want %v", c.rs, c.notesLen, got, c.want)
			}
		})
	}
}

func TestConsolidateReason(t *testing.T) {
	if got := consolidateReason(200, 10, 32768); got != "every 10 sessions" {
		t.Errorf("count reason: %q", got)
	}
	if got := consolidateReason(200, 1, 32768); got != "every 1 session" {
		t.Errorf("singular count reason: %q", got)
	}
	if got := consolidateReason(40960, 10, 32768); got != "notes are 40 KB" {
		t.Errorf("size reason: %q", got)
	}
}

// With notes_consolidate_every = 2 a developer's second session is asked to
// consolidate its notes, and the ask is recorded so the next one is not.
func TestNotesConsolidationIsAskedForOnSchedule(t *testing.T) {
	h := newHarness(t, baseTOML+"notes_consolidate_every = 2\n")
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN",
		HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// developer -> reviewer -> developer: only the developer's second
	// session is two sessions past the last (never made) consolidation.
	dev := h.sessions(config.RoleDeveloper)
	if len(dev) != 2 {
		t.Fatalf("developer sessions: %d", len(dev))
	}
	const ask = "Also consolidate your notes this session"
	if first := readFile(t, dev[0]+"/prompt.md"); strings.Contains(first, ask) {
		t.Errorf("first developer session was asked to consolidate:\n%s", first)
	}
	second := readFile(t, dev[1]+"/prompt.md")
	if !strings.Contains(second, "Also consolidate your notes this session (every 2 sessions)") {
		t.Errorf("second developer session was not asked to consolidate:\n%s", second)
	}
	for _, want := range []string{"`notes_read`", "`notes_write`"} {
		if !strings.Contains(second, want) {
			t.Errorf("the ask does not name %s:\n%s", want, second)
		}
	}
	if strings.Contains(second, h.store.NotesPath(config.RoleDeveloper)) {
		t.Errorf("the ask names the notes file, which the session cannot be told to edit:\n%s", second)
	}

	rs, err := h.store.Role(config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	if rs.Sessions != 2 || rs.LastConsolidated != 2 {
		t.Errorf("developer bookkeeping: sessions = %d, last_consolidated = %d; want 2 and 2", rs.Sessions, rs.LastConsolidated)
	}
	// The reviewer counts its own sessions in its own file.
	rev, err := h.store.Role(config.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	if rev.Sessions != len(h.sessions(config.RoleReviewer)) {
		t.Errorf("reviewer sessions: %d recorded, %d run", rev.Sessions, len(h.sessions(config.RoleReviewer)))
	}
}

// A singleton's run must not overwrite the session counters that live in the
// same file.
func TestSingletonRunKeepsSessionCounters(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.project_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n")
	h.sched.OnlyRoles = map[string]bool{config.RoleProductManager: true}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if n := len(h.sessions(config.RoleProductManager)); n != 1 {
		t.Fatalf("product manager sessions: %d", n)
	}
	rs, err := h.store.Role(config.RoleProductManager)
	if err != nil {
		t.Fatal(err)
	}
	if rs.LastRun.IsZero() {
		t.Error("the product manager's last run was not recorded")
	}
	if rs.Sessions != 1 {
		t.Errorf("product manager sessions recorded: %d, want 1", rs.Sessions)
	}
}

// A session's notes reach it through notes_read, not the prompt: what is
// in the notes file when a session starts is in neither of its prompts, and
// the system prompt names the tools it reads and writes them with instead.
// Their size still drives the consolidation ask: with notes_max_bytes below
// the file's size the session is asked, and the reason carries that size.
func TestNotesAreNotRenderedIntoThePrompts(t *testing.T) {
	h := newHarnessAt(t, strings.Replace(devOnlyTOML, "max_review_rounds = 3\n", "max_review_rounds = 3\nnotes_max_bytes = 64\n", 1), time.Now())
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	const sentinel = "THE NOTES SAY: run the e2e suite twice, and then a third time for luck"
	notes := "# developer notes\n\n- " + sentinel + "\n"
	if len(notes) <= 64 {
		t.Fatalf("fixture notes are %d bytes, want more than notes_max_bytes", len(notes))
	}
	if err := h.store.WriteNotes(config.RoleDeveloper, notes); err != nil {
		t.Fatal(err)
	}
	seedReady(h, 1, "m", time.Now().Add(-time.Hour))

	if err := h.sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 1 {
		t.Fatalf("developer sessions: %d, want 1", n)
	}
	sys, task := systemPromptOf(t, h, 0), promptOf(t, h, 0)
	if strings.Contains(sys+task, sentinel) {
		t.Errorf("the notes were rendered into a prompt:\n%s\n%s", sys, task)
	}
	for _, want := range []string{"`notes_read`", "`notes_write`"} {
		if !strings.Contains(sys, want) {
			t.Errorf("the system prompt does not name %s:\n%s", want, sys)
		}
	}
	if strings.Contains(sys+task, h.store.NotesPath(config.RoleDeveloper)) {
		t.Errorf("a prompt names the notes file:\n%s\n%s", sys, task)
	}
	if ask := "Also consolidate your notes this session (notes are " + byteSize(len(notes)) + ")"; !strings.Contains(task, ask) {
		t.Errorf("the task does not ask %q:\n%s", ask, task)
	}
}

// offFileNotes is a Notes backend holding a role's notes somewhere other
// than the notes file — what notes.backend = "neo4j" is — reporting a size
// the file under the state directory does not have.
type offFileNotes struct {
	size int64

	mu    sync.Mutex
	sized []string // the roles Size was asked about, in order
}

func (n *offFileNotes) ReadNotes(context.Context, string) (string, error) { return "", nil }
func (n *offFileNotes) WriteNotes(context.Context, string, string) error  { return nil }

func (n *offFileNotes) Size(_ context.Context, role string) (int64, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.sized = append(n.sized, role)
	return n.size, nil
}

// The notes_max_bytes trigger measures the notes through the configured
// backend, not the notes file: with the notes off the file system, a role
// whose backend reports 40 KB is asked to consolidate them even though the
// file under the state directory is a few dozen bytes.
func TestConsolidationMeasuresTheNotesBackend(t *testing.T) {
	notes := &offFileNotes{size: 40960}
	toml := strings.Replace(devOnlyTOML, "max_review_rounds = 3\n", "max_review_rounds = 3\nnotes_max_bytes = 1024\nnotes_consolidate_every = 0\n", 1)
	h := newHarnessAt(t, toml, time.Now(), func(d *Deps) { d.Notes = notes })
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	seedReady(h, 1, "m", time.Now().Add(-time.Hour))

	if err := h.sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 1 {
		t.Fatalf("developer sessions: %d, want 1", n)
	}
	if onDisk, err := h.store.NotesSize(config.RoleDeveloper); err != nil || onDisk >= 1024 {
		t.Fatalf("the notes file is %d bytes, %v; the file trigger must not be the one that fired", onDisk, err)
	}
	if !slices.Contains(notes.sized, config.RoleDeveloper) {
		t.Errorf("the backend was not asked for the developer's notes size: %v", notes.sized)
	}
	const ask = "Also consolidate your notes this session (notes are 40 KB)"
	if task := promptOf(t, h, 0); !strings.Contains(task, ask) {
		t.Errorf("the task does not ask %q:\n%s", ask, task)
	}
}
