package mcpserver

import (
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
)

// touched is what the session directory says this session changed on GitHub.
func (h *harness) touched() []int {
	h.t.Helper()
	got, err := session.TouchedIssues(h.sessionDir)
	if err != nil {
		h.t.Fatal(err)
	}
	return got
}

func wantTouched(t *testing.T, got []int, want ...int) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("touched issues %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("touched issues %v, want %v", got, want)
		}
	}
}

// Every tool that creates an issue or changes one records it in the session
// directory, so the scheduler can refresh its cached copy when the session
// ends instead of leaving the change invisible until the next poll
// (internal/scheduler, refreshTouched).
func TestTheToolsThatChangeAnIssueRecordIt(t *testing.T) {
	t.Run("issue_create", func(t *testing.T) {
		f := &fakeGH{}
		backend := &ghIssues{gh: f.client(t), filter: config.Filter{Label: "bees", Assignee: "kyle"}, labels: config.LabelsFor("bees")}
		h := newHarness(t, config.RoleProjectManager, Deps{Issues: backend})

		h.call("issue_create", map[string]any{"title": "Split off the parser", "body": "why", "related": 36})

		wantTouched(t, h.touched(), 90)
	})

	t.Run("issue_set_state", func(t *testing.T) {
		f := newFakeGitHub().issue(41, "bees:triage")
		h := newHarness(t, config.RoleProjectManager, Deps{GitHub: f})

		h.call("issue_set_state", map[string]any{"number": 41, "state": "ready", "size": "s"})

		wantTouched(t, h.touched(), 41)
	})

	t.Run("issue_edit_body", func(t *testing.T) {
		f := newFakeGitHub().issue(41, "bees:triage")
		h := newHarness(t, config.RoleProjectManager, Deps{GitHub: f})

		h.call("issue_edit_body", map[string]any{"number": 41, "body": "the refined body"})

		wantTouched(t, h.touched(), 41)
	})

	t.Run("issue_question", func(t *testing.T) {
		f := newFakeGitHub().issue(41, "bees:feature")
		h := newHarness(t, config.RoleProductManager, Deps{GitHub: f})

		h.call("issue_question", map[string]any{"number": 41, "waiting": true})

		wantTouched(t, h.touched(), 41)
	})
}

// Nothing is recorded for a change that did not happen: the refresh spends a
// `gh issue view` per issue on the list, and an issue whose labels and body
// are exactly what the last poll returned is a call for nothing.
func TestNothingIsRecordedWithoutAChange(t *testing.T) {
	t.Run("a tool that only reads", func(t *testing.T) {
		f := newFakeGitHub().issue(41, "bees:triage")
		h := newHarness(t, config.RoleProjectManager, Deps{GitHub: f})

		h.call("issue_view", map[string]any{"number": 41})
		h.call("comment", map[string]any{"number": 41, "body": "for a person"})

		wantTouched(t, h.touched())
	})

	t.Run("a refused edit", func(t *testing.T) {
		// Only an issue in triage is the project manager's to move.
		f := newFakeGitHub().issue(41, "bees:ready")
		h := newHarness(t, config.RoleProjectManager, Deps{GitHub: f})

		if res := h.callRaw("issue_set_state", map[string]any{"number": 41, "state": "ready", "size": "s"}); !res.IsError {
			t.Fatal("moving an issue that is not in triage must be refused")
		}

		wantTouched(t, h.touched())
	})
}
