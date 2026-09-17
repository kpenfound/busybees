package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// reloadTOML runs the developer alone, one at a time, on the model the
// test replaces.
const reloadTOML = `
version = 1
[project]
repo = "acme/widgets"
[scheduler]
poll_interval = "1s"
max_developers = 1
max_review_rounds = 3
[roles.developer]
model = "opus"
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
`

// loadReload writes text as the harness's bees.toml and loads it the way
// `bees run` loads a reload: read and resolved, nothing else.
func loadReload(t *testing.T, h *harness, text string) *config.Config {
	t.Helper()
	path := filepath.Join(h.clone, "bees.toml")
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	return cfg
}

// A reload takes effect on the next dispatch and on nothing already
// running: the developer session started before it keeps the model it was
// started with, and the one dispatched after it runs on the new one.
func TestReloadChangesWhatTheNextDispatchUsesAndNotARunningSession(t *testing.T) {
	h := newHarness(t, reloadTOML)
	seedReady(h, 2, "s", time.Now().Add(-30*time.Minute))
	first, release, cancel, done := startHeldSession(t, h, func(dir string) bool {
		_, err := os.Stat(filepath.Join(dir, "args.txt"))
		return err == nil
	}, "the first developer session to start")
	defer cancel()
	if got := argValue(argsOf(t, first), "--model"); got != "opus" {
		t.Fatalf("the first session runs on %q, want opus", got)
	}

	next := loadReload(t, h, strings.Replace(reloadTOML, `model = "opus"`, `model = "sonnet"`, 1))
	if err := h.sched.Reload(next); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	// The reload asks for a pass at once, and the pass puts it in force.
	waitFor(t, 10*time.Second, "the reload to be applied", func() bool {
		return strings.Contains(h.logs.String(), "configuration reloaded")
	})
	if h.sched.config() != next {
		t.Fatal("the reloaded configuration is not the one in force")
	}

	// The held session is released: its slot frees and issue 2 goes out.
	if err := os.WriteFile(release, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	var second string
	waitFor(t, 30*time.Second, "the second developer session to start", func() bool {
		for _, dir := range h.sessions(config.RoleDeveloper) {
			if dir == first || !strings.Contains(filepath.Base(dir), "issue-2") {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, "args.txt")); err == nil {
				second = dir
				return true
			}
		}
		return false
	})
	if got := argValue(argsOf(t, second), "--model"); got != "sonnet" {
		t.Errorf("the session dispatched after the reload runs on %q, want sonnet", got)
	}
	// The first session's command line is what it was: the reload reached
	// no process already running.
	if got := argValue(argsOf(t, first), "--model"); got != "opus" {
		t.Errorf("the running session's model changed to %q", got)
	}
	cancel()
	if err := waitRun(t, done); err != nil {
		t.Fatalf("Run: %v", err)
	}
}

// A reload that changes a key the running factory was built from is
// refused, naming every such key, and the configuration in force stays
// what it was — after the next pass too, which is where an accepted reload
// would have landed.
func TestReloadRefusesAFixedKeyAndKeepsThePreviousConfig(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	before := h.sched.config()
	ctx := context.Background()

	changed := loadReload(t, h, strings.NewReplacer(
		"max_developers = 2", "max_developers = 5\nworkspace_root = \"/tmp/elsewhere\"",
		`repo = "acme/widgets"`, "repo = \"acme/widgets\"\nstate_dir = \"elsewhere\"\nbranch_prefix = \"other/\"",
	).Replace(noRolesTOML))
	err := h.sched.Reload(changed)
	if err == nil {
		t.Fatal("a reload changing the state directory was accepted")
	}
	for _, key := range []string{"project.state_dir", "project.branch_prefix", "scheduler.max_developers", "scheduler.workspace_root", "restart bees run", changed.Path} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("the refusal does not name %s: %v", key, err)
		}
	}
	// The keys come before the file, so a view that cuts the line keeps them.
	if strings.Index(err.Error(), "project.state_dir") > strings.Index(err.Error(), changed.Path) {
		t.Errorf("the refusal names the file before the keys: %v", err)
	}
	if strings.Contains(err.Error(), "poll_interval") {
		t.Errorf("the refusal names a key that did not change: %v", err)
	}
	if _, err := h.sched.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if h.sched.config() != before {
		t.Fatal("a refused reload replaced the configuration in force")
	}
	if err := h.sched.Reload(nil); err == nil {
		t.Error("a nil configuration was accepted")
	}

	// A reload changing only live keys is accepted, and in force from the
	// next pass, not before it.
	accepted := loadReload(t, h, strings.Replace(noRolesTOML, `poll_interval = "1s"`, `poll_interval = "7s"`, 1))
	if err := h.sched.Reload(accepted); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if h.sched.config() != before {
		t.Fatal("the reload was applied before a pass started")
	}
	if _, err := h.sched.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if got := h.sched.config(); got != accepted || got.Scheduler.PollInterval.Duration != 7*time.Second {
		t.Fatalf("the pass did not put the reload in force: %+v", got.Scheduler.PollInterval)
	}
	if !strings.Contains(h.logs.String(), "configuration reloaded") {
		t.Error("the applied reload was not logged")
	}
}
