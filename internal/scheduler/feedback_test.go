package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// managersTOML runs both managers and nobody else: an issue that lands in the
// wrong queue shows up as the wrong session having been started.
const managersTOML = baseTOML + `
[roles.qa]
enabled = false
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
`

// An issue a person filed with the factory's label and nothing else is an
// idea, not a spec: it goes to the product manager as feedback, who decides
// whether it becomes a feature, a work item or nothing. It must not be
// specced by the project manager on the way.
func TestUnlabelledIssueGoesToTheProductManager(t *testing.T) {
	h := newHarness(t, managersTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", Body: "exports would be nice",
		State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}}, CreatedAt: time.Now().Add(-time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:feedback" {
		t.Fatalf("issue 1 label history: %q, want bees:feedback", got)
	}
	if github.HasLabel(h.gh.issues[1].Labels, "bees:triage") {
		t.Fatalf("issue 1 was triaged: %v", h.gh.issues[1].Labels)
	}
	// The relabel and the product manager's run happen in the same pass: the
	// issue is appended to the feedback list the prompt renders, so nothing
	// waits for the next poll.
	pdm := h.sessions(config.RoleProductManager)
	if len(pdm) != 1 {
		t.Fatalf("product manager sessions: %d, want 1", len(pdm))
	}
	prompt, err := os.ReadFile(filepath.Join(pdm[0], "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "## Feedback from people (1)") ||
		!strings.Contains(string(prompt), "#1: Filed from the GitHub UI") {
		t.Fatalf("product manager prompt:\n%s", prompt)
	}
	// Nothing in the triage queue, so the project manager has no work at all.
	if got := h.sessions(config.RoleProjectManager); len(got) != 0 {
		t.Fatalf("the project manager ran for an idea: %v", got)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Queues["triage"] != 0 || st.Queues[queueNoState] != 0 {
		t.Fatalf("queues %+v, want the issue in neither triage nor no_state", st.Queues)
	}
}

// The same for a factory that does not require its label: an issue visible by
// assignee alone gets the label *and* bees:feedback, in one edit.
func TestUnlabelledIssueWithoutTheBaseLabel(t *testing.T) {
	h := newHarness(t, noRolesTOML+`
[filter]
assignee = "kyle"
require_label = false
`)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", State: "OPEN",
		Assignees: []github.Author{{Login: "kyle"}}, CreatedAt: time.Now().Add(-time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:feedback,bees" {
		t.Fatalf("issue 1 label history: %q, want bees:feedback,bees", got)
	}
	if n := h.gh.callCount("issue edit"); n != 1 {
		t.Fatalf("issue edit called %d times, want one edit for both labels", n)
	}
}

// A person's bug report with no state label goes to the product manager too:
// bees:bug says what an issue *is* (a bug work item), not where it should go,
// so it is not a kind that keeps the issue out of the feedback route the way
// bees:feature and bees:feedback are. A person who wants the bug built as
// written labels it bees:triage as well, which is the documented fast path to
// the project manager. Decided on #221 rather than changed: see that issue.
func TestBugIssueWithNoStateLabelGoesToTheProductManager(t *testing.T) {
	h := newHarness(t, managersTOML)
	h.gh.issues[1] = &github.Issue{Number: 1, Title: "Export writes an empty file", Body: "bees export gives me 0 bytes",
		State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels:    []github.Label{{Name: "bees"}, {Name: "bees:bug"}},
		CreatedAt: time.Now().Add(-time.Hour)}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.history[1], ","); got != "bees:feedback" {
		t.Fatalf("issue 1 label history: %q, want bees:feedback", got)
	}
	// The kind label survives the relabel: the issue is still a bug report.
	if !github.HasLabel(h.gh.issues[1].Labels, "bees:bug") {
		t.Fatalf("issue 1 lost bees:bug: %v", h.gh.issues[1].Labels)
	}
	if github.HasLabel(h.gh.issues[1].Labels, "bees:triage") {
		t.Fatalf("issue 1 was triaged: %v", h.gh.issues[1].Labels)
	}
	pdm := h.sessions(config.RoleProductManager)
	if len(pdm) != 1 {
		t.Fatalf("product manager sessions: %d, want 1", len(pdm))
	}
	prompt, err := os.ReadFile(filepath.Join(pdm[0], "prompt.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prompt), "## Feedback from people (1)") ||
		!strings.Contains(string(prompt), "#1: Export writes an empty file") {
		t.Fatalf("product manager prompt:\n%s", prompt)
	}
	if got := h.sessions(config.RoleProjectManager); len(got) != 0 {
		t.Fatalf("the project manager ran for a bug report: %v", got)
	}
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.Queues["triage"] != 0 || st.Queues[queueNoState] != 0 {
		t.Fatalf("queues %+v, want the issue in neither triage nor no_state", st.Queues)
	}
}
