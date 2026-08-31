package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/workspace"
)

// commitProjectPrompt puts bees/prompts/<file> on a branch of the clone's
// origin. A branch other than the default one is created from whatever is
// checked out; the clone is left on main so the worktree the scheduler cuts
// can check the branch out.
func commitProjectPrompt(t *testing.T, clone, branch, file, body string) {
	t.Helper()
	ctx := context.Background()
	must := func(args ...string) {
		t.Helper()
		if _, err := workspace.Git(ctx, clone, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if branch != "main" {
		must("checkout", "-q", "-b", branch)
	}
	dir := filepath.Join(clone, "bees", "prompts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, file), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	must("add", "bees")
	must("commit", "-q", "-m", "project prompt on "+branch)
	must("push", "-q", "origin", branch)
	if branch != "main" {
		must("checkout", "-q", "main")
	}
}

// hasDegradedOp reports whether an operation is on the degraded list.
// degradedOp fails the test when it is not, which is the answer one of these
// tests is asserting.
func hasDegradedOp(st state.Status, op string) bool {
	for _, f := range st.Degraded {
		if f.Op == op {
			return true
		}
	}
	return false
}

// systemPromptOf returns the system prompt of the i-th session that ran.
func systemPromptOf(t *testing.T, h *harness, i int) string {
	t.Helper()
	order := h.sessionOrder()
	if i >= len(order) {
		t.Fatalf("no session %d in %v", i, order)
	}
	b, err := os.ReadFile(filepath.Join(h.store.SessionsDir(), order[i], "system-prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// A project keeps its role instructions in bees/prompts/ so a branch can
// carry its own - experimental instructions the reviewer sees in the diff.
// The session reads them from its worktree, so the branch's text is what
// reaches it and the default branch's is not.
func TestProjectPromptsComeFromTheSessionsBranch(t *testing.T) {
	h := newHarnessAt(t, devOnlyTOML, time.Now())
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	commitProjectPrompt(t, h.clone, "main", "developer.md", "MAIN SAYS: the old way.")
	commitProjectPrompt(t, h.clone, "bees/issue-1", "developer.md", "THE BRANCH SAYS: the new way.")
	seedReady(h, 1, "m", time.Now().Add(-time.Hour))

	if err := h.sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	sys := systemPromptOf(t, h, 0)
	if !strings.Contains(sys, "## Additional instructions from bees/prompts/developer.md") {
		t.Fatalf("developer session has no project prompt section:\n%s", sys)
	}
	if !strings.Contains(sys, "THE BRANCH SAYS: the new way.") {
		t.Errorf("the branch's own instructions did not reach the session:\n%s", sys)
	}
	if strings.Contains(sys, "MAIN SAYS: the old way.") {
		t.Errorf("the session was given the default branch's instructions:\n%s", sys)
	}
}

// The normal case: a repository with no bees/prompts/ directory. The session
// gets exactly the prompt it got before the feature existed, and the missing
// directory is completely silent - no warning, no degraded operation.
func TestNoProjectPromptsIsSilent(t *testing.T) {
	h := newHarnessAt(t, devOnlyTOML, time.Now())
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	seedReady(h, 1, "m", time.Now().Add(-time.Hour))

	if err := h.sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	sys := systemPromptOf(t, h, 0)
	if strings.Contains(sys, "Additional instructions from bees/") {
		t.Errorf("a repository with no bees/prompts/ rendered a project prompt section:\n%s", sys)
	}
	if strings.Contains(h.logs.String(), "project prompt") {
		t.Errorf("a missing bees/prompts/ directory logged something:\n%s", h.logs.String())
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if hasDegradedOp(st, "project-prompts") {
		t.Errorf("a missing bees/prompts/ directory was recorded as a degraded operation: %+v", st.Degraded)
	}
}

// A project prompt file bees cannot read must not take the session down: the
// session runs with the prompt it could build, and the failure is visible as
// a degraded operation instead. `bees doctor` is where it fails loudly.
func TestBrokenProjectPromptWarnsAndTheSessionStillRuns(t *testing.T) {
	h := newHarnessAt(t, devOnlyTOML, time.Now())
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true}
	commitProjectPrompt(t, h.clone, "main", "common.md", "Every role: speak plainly.")
	commitProjectPrompt(t, h.clone, "bees/issue-1", "developer.md",
		strings.Repeat("x", 64<<10+1))
	seedReady(h, 1, "m", time.Now().Add(-time.Hour))

	if err := h.sched.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 1 {
		t.Fatalf("developer sessions: got %d, want 1", n)
	}
	sys := systemPromptOf(t, h, 0)
	if !strings.Contains(sys, "Every role: speak plainly.") {
		t.Errorf("the readable project prompt file was dropped too:\n%s", sys)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if !hasDegradedOp(st, "project-prompts") {
		t.Errorf("an unusable project prompt file was not reported: %+v", st.Degraded)
	}
}
