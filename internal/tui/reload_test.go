package tui

import (
	"slices"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/scheduler"
	"github.com/kpenfound/busybees/internal/state"
)

func dynamicProject(name string, generation uint64) Project {
	return Project{Path: "/" + name + "/bees.toml", Generation: generation, Name: name, Repo: "acme/" + name}
}

func updateModel(m Model, msg tea.Msg) Model {
	next, _ := m.Update(msg)
	return next.(Model)
}

func TestMachineMembershipPreservesSourcesStateAndSelection(t *testing.T) {
	a, b, c := dynamicProject("a", 1), dynamicProject("b", 2), dynamicProject("c", 3)
	kills := 0
	a.Kill = func(string) error { kills++; return nil }
	a.Status = func() (state.Status, error) { return state.Status{Queues: map[string]int{"ready": 7}}, nil }
	events := make(chan scheduler.Event, 2)
	a.Events = events
	m := New(Deps{Projects: []Project{a, b}})
	m = updateModel(m, in(0, started("a-live", config.RoleDeveloper, 12, 1, fixed, "opus", false)))
	m = updateModel(m, in(0, ended("a-old", config.RoleDeveloper, 12, 1, 5, 2)))
	m = updateModel(m, in(0, staged(12, "review", 2)))
	m = updateModel(m, m.refresh(0)())
	m.shown = 0
	original := m.projects[0]
	pendingEvent, pendingStatus := m.waitForEvent(0), m.refresh(0)
	// Replacement callbacks must not take over an unchanged incarnation.
	replacement := a
	replacement.Kill = func(string) error { t.Fatal("replacement control used"); return nil }
	b.Draining = true
	m = updateModel(m, projectsMsg{c, replacement, b})
	if m.shown != 0 || !slices.Equal(m.order, []int{2, 0, 1}) || m.projects[0] != original {
		t.Fatalf("identity lost on reorder: selected %d, order %v", m.shown, m.order)
	}
	if len(m.sessions) != 1 || len(m.recent) != 1 || original.spent[spendKey(12, config.RoleDeveloper)].cost != 2 || original.stages[12].round != 2 || original.status.Queues["ready"] != 7 {
		t.Fatal("reorder reset accumulated state")
	}
	if !strings.Contains(m.selector(), "b (draining)") {
		t.Fatal(m.selector())
	}
	events <- scheduler.Event{Kind: scheduler.EventStage, Stage: "checks", Round: 3, Work: ghwork.New(12, 0)}
	m = updateModel(m, pendingEvent())
	m = updateModel(m, pendingStatus())
	if original.stages[12].round != 3 {
		t.Fatal("existing subscription detached")
	}
	if err := m.projects[0].Kill("a-live"); err != nil || kills != 1 {
		t.Fatalf("control: %v, %d", err, kills)
	}
	m = updateModel(m, projectsMsg{replacement, c})
	if m.shown != 0 || m.projects[1] != nil {
		t.Fatal("retirement changed selected A or retained B")
	}
}

func TestMachineRetirementIgnoresLateResultsAndReaddedPath(t *testing.T) {
	a, b, c := dynamicProject("a", 1), dynamicProject("b", 2), dynamicProject("c", 3)
	b.Status = func() (state.Status, error) { return state.Status{Queues: map[string]int{"ready": 99}}, nil }
	events := make(chan scheduler.Event, 1)
	b.Events = events
	m := New(Deps{Projects: []Project{a, b}})
	m = updateModel(m, in(1, started("same", config.RoleDeveloper, 12, 1, fixed, "opus", false)))
	m.shown, m.cursor, m.confirmKill = 1, 100, sessionRef{project: 1, name: "same"}
	pendingEvent, pendingStatus := m.waitForEvent(1), m.refresh(1)
	b.Draining = true
	m = updateModel(m, projectsMsg{a, c, b})
	if m.shown != 1 || len(m.shownSessions()) != 1 || m.confirmKill.name != "" || m.cursor != 0 {
		t.Fatal("drain lost selection/session or kept cursor/kill confirmation")
	}
	m = updateModel(m, in(1, staged(12, "checks", 4)))
	m = updateModel(m, in(1, ended("same", config.RoleDeveloper, 12, 1, 8, 3)))
	if len(m.sessions) != 0 || m.recent[0].project != 1 || m.projects[1].stages[12].round != 4 {
		t.Fatal("final events were not attributed to draining B")
	}
	m = updateModel(m, projectsMsg{a, c})
	if m.shown != allProjects {
		t.Fatal("selected removal did not fall back to all")
	}
	newB := dynamicProject("b", 4)
	m = updateModel(m, projectsMsg{a, newB, c})
	newID := m.order[1]
	m = updateModel(m, in(newID, started("same", config.RoleDeveloper, 12, 1, fixed, "opus", false)))
	events <- scheduler.Event{Kind: scheduler.EventSessionEnded, Session: "same", Role: config.RoleDeveloper, CostUSD: 99, CostKnown: true, Work: ghwork.New(12, 0)}
	m = updateModel(m, pendingEvent())
	m = updateModel(m, pendingStatus())
	m = updateModel(m, turnsMsg{sessionRef{project: 1, name: "same"}: 99})
	if len(m.sessions) != 1 || m.sessions[0].turns != 0 || len(m.projects[newID].spent) != 0 || len(m.projects[newID].status.Queues) != 0 || len(m.recent) != 0 {
		t.Fatal("stale result crossed into the new incarnation")
	}
	m.shown = newID
	m = updateModel(m, projectsMsg{c})
	if m.shown != 2 || m.title() != "acme/c" {
		t.Fatal("sole-project fallback lost stable handle")
	}
	m = updateModel(m, projectsMsg{})
	_ = m.View() // A completed empty snapshot is safe too.
}

func TestMachineRemovedWatchFollowsFinalTranscriptAndDisablesMessages(t *testing.T) {
	a, b := dynamicProject("a", 1), dynamicProject("b", 2)
	b.Send = func(string, int, int, string, string) error { t.Fatal("messaged removed project"); return nil }
	dir := t.TempDir()
	writeTranscript(t, dir, "working")
	m := New(Deps{Projects: []Project{a, b}})
	m = updateModel(m, in(1, watched("b-live", config.RoleDeveloper, 12, 1, dir)))
	m = m.open(m.sessions[0])
	m = updateModel(m, m.readTail()())
	m.watching.composing, m.watching.draft = true, "unfinished message"
	pendingTail := m.readTail()
	b.Draining = true
	m = updateModel(m, projectsMsg{a, b})
	if m.sender() != nil || m.watching.composing || !strings.Contains(m.sessionFooter(), "messaging disabled") {
		t.Fatal("draining watch still offers messaging")
	}
	m = updateModel(m, projectsMsg{a})
	writeTranscript(t, dir, "working", "final output")
	m = updateModel(m, pendingTail())
	if !strings.Contains(header(m.View()), "acme/b (removed)") || !strings.Contains(m.View(), "final output") || m.watching == nil || !m.watching.ended {
		t.Fatal("retirement broke transcript following")
	}
	if m.deliver("message") != nil {
		t.Fatal("retired watch produced a send command")
	}
	m = updateModel(m, tea.KeyMsg{Type: tea.KeyEsc})
	if m.watching != nil || m.shown != 0 {
		t.Fatal("Escape failed after retirement")
	}
	_ = m.View()
}

func TestMachineProjectInputAndRetirementCancelEventRead(t *testing.T) {
	updates := make(chan []Project, 1)
	a := dynamicProject("a", 1)
	done := make(chan struct{})
	a.Events, a.Done = make(chan scheduler.Event), done
	m := New(Deps{Projects: []Project{a}, ProjectUpdates: updates})
	updates <- []Project{a, dynamicProject("c", 2)}
	m = updateModel(m, m.waitForProjects()())
	if len(m.projects) != 2 {
		t.Fatal("input was not consumed")
	}
	pending := m.waitForEvent(0)
	close(done)
	if pending() != nil {
		t.Fatal("retired subscription delivered an event")
	}
}

func TestMachineCoalescedReaddRetiresOldIncarnation(t *testing.T) {
	a, b := dynamicProject("a", 1), dynamicProject("b", 2)
	m := New(Deps{Projects: []Project{a, b}})
	m = updateModel(m, in(1, started("old", config.RoleDeveloper, 12, 1, fixed, "opus", false)))
	m.shown = 1
	m = m.open(m.sessions[0])
	// A slow UI may see only the replacement snapshot, with the same path
	// and a different generation, skipping both drain and removal snapshots.
	m = updateModel(m, projectsMsg{a, dynamicProject("b", 3)})
	if m.projects[1] != nil || m.order[1] == 1 || len(m.sessions) != 0 || m.shown != allProjects || m.sender() != nil || !m.watching.ended {
		t.Fatal("coalesced reload reused the removed incarnation")
	}
}
