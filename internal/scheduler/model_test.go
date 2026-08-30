package scheduler

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// modelBySizeTOML runs the developer on haiku for xs work items and leaves
// every other size on the default model.
const modelBySizeTOML = baseTOML + `
[roles.developer]
model_by_size = { xs = "haiku" }
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

// seedCounter presets one of the fake claude's counters, so that the next
// session sees attempt n+1. Seeding "review" with 1 makes the reviewer approve
// straight away instead of asking for changes first.
func seedCounter(t *testing.T, h *harness, name string, n int) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(h.store.Dir, "fake-"+name), []byte(fmt.Sprint(n)), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestDeveloperModelBySize checks the size label on the issue picks the
// developer's model, and that no other role is affected by it.
func TestDeveloperModelBySize(t *testing.T) {
	for _, tc := range []struct {
		name, size, want string
	}{
		{"xs uses the override", "bees:size/xs", "haiku"},
		{"another size uses the model", "bees:size/m", config.DefaultModel},
		{"an unsized issue uses the model", "", config.DefaultModel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, modelBySizeTOML)
			labels := []github.Label{{Name: "bees"}, {Name: "bees:ready"}}
			if tc.size != "" {
				labels = append(labels, github.Label{Name: tc.size})
			}
			h.gh.issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: labels, CreatedAt: time.Now()}
			h.gh.prs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
			seedCounter(t, h, "review", 1) // the reviewer approves its first review

			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			if err := h.sched.Run(ctx); err != nil {
				t.Fatal(err)
			}
			if got := h.sessionFlag(t, config.RoleDeveloper, "--model"); got != tc.want {
				t.Errorf("developer --model: got %q want %q", got, tc.want)
			}
			// The reviewer looks at the same issue and keeps its own model.
			if got := h.sessionFlag(t, config.RoleReviewer, "--model"); got != config.DefaultModel {
				t.Errorf("reviewer --model: got %q want %q", got, config.DefaultModel)
			}
		})
	}
}
