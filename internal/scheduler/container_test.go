package scheduler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// fakeEngine writes the two scripts a container session needs on the host:
// a docker that records its arguments in the session directory and runs the
// command after the image (the fake developer, this test binary), and a
// bees whose `mcp serve --listen` reports an address and waits.
func fakeEngine(t *testing.T) (docker, bees string) {
	t.Helper()
	dir := t.TempDir()
	docker = filepath.Join(dir, "docker")
	bees = filepath.Join(dir, "bees")
	dockerScript := `#!/bin/sh
case "$1" in rm|network) exit 0 ;; esac
printf '%s\n' "$@" > "$BEES_SESSION_DIR/docker-args.txt"
while [ $# -gt 0 ]; do
  case "$1" in
  --cidfile) echo "cid-1" > "$2"; shift 2 ;;
  ghcr.io/acme/bees:1) shift; break ;;
  *) shift ;;
  esac
done
exec "$@"
`
	beesScript := `#!/bin/sh
echo "listening on 127.0.0.1:45678"
exec sleep 60
`
	for p, body := range map[string]string{docker: dockerScript, bees: beesScript} {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return docker, bees
}

// A container developer goes through the whole worker the way an unboxed
// one does — the session runs inside the engine, commits, pushes and reports
// pr-opened, and the issue moves to review — and its prompt tells it the
// bees binary is not there.
func TestContainerDeveloperRunsThroughTheEngine(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test")
	h := newHarness(t, baseTOML+`
[github]
login = "bot"
token = "ghp_test"
[roles.developer]
sandbox = "container"
sandbox_image = "ghcr.io/acme/bees:1"
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
`)
	docker, bees := fakeEngine(t)
	h.sched.runner.DockerBin = docker
	h.sched.runner.BeesBin = bees
	h.sched.runner.ContainerListen = "127.0.0.1:0"
	h.sched.runner.GitHub = h.sched.cfg.GitHub
	seedReady(h, 7, "s", time.Now().Add(-time.Hour))
	runPass(t, h)
	h.sched.wg.Wait()

	// The fake reviewer requests changes once, so the developer ran twice,
	// both times in the engine; the reviewer, unboxed, did not.
	dirs := h.sessions(config.RoleDeveloper)
	if len(dirs) != 2 {
		t.Fatalf("developer sessions: %v", dirs)
	}
	for _, dir := range dirs {
		args, err := os.ReadFile(filepath.Join(dir, "docker-args.txt"))
		if err != nil {
			t.Fatalf("the developer did not run through the engine: %v", err)
		}
		for _, want := range []string{"run\n--rm\n", "--cidfile\n" + filepath.Join(dir, "container-id"), "ghcr.io/acme/bees:1\n" + os.Args[0] + "\n-p\n"} {
			if !strings.Contains(string(args), want) {
				t.Errorf("docker args missing %q:\n%s", want, args)
			}
		}
		prompt, _ := os.ReadFile(filepath.Join(dir, "system-prompt.md"))
		if !strings.Contains(string(prompt), "without the `bees` binary") {
			t.Error("the container developer's prompt still offers the bees commands")
		}
	}
	for _, dir := range h.sessions(config.RoleReviewer) {
		if _, err := os.Stat(filepath.Join(dir, "docker-args.txt")); err == nil {
			t.Errorf("the reviewer ran in the engine: %s", dir)
		}
	}
	if !strings.Contains(strings.Join(h.gh.history[7], ","), "bees:approved") {
		t.Errorf("issue 7 did not reach approved: %v", h.gh.history[7])
	}
}
