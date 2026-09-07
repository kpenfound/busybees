package session

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/procs"
)

// TestContainerEndToEnd runs a real developer session inside a real
// container: docker, the image named by BEES_CONTAINER_E2E, the real claude
// in it, and the real bees binary on the host serving the tools over HTTP.
// It is skipped unless that variable is set, because the suite never
// invokes a container engine or claude; it is the check to run by hand
// after touching container.go, with GH_TOKEN and a claude credential
// (config.AgentCredentials) in the environment:
//
//	BEES_CONTAINER_E2E=<image> go test ./internal/session -run TestContainerEndToEnd -v
//
// The fixture is a bare repository inside the state directory (mounted, so
// the push from inside the container reaches it) with a linked worktree of
// a clone as the session's worktree. The session is asked to prove gh
// works with the factory's token, commit, push, write mail and report an
// outcome; the test reads each of those back on the host.
func TestContainerEndToEnd(t *testing.T) {
	image := os.Getenv("BEES_CONTAINER_E2E")
	if image == "" {
		t.Skip("set BEES_CONTAINER_E2E=<image> to run a real container session")
	}
	if os.Getenv("GH_TOKEN") == "" {
		t.Skip("GH_TOKEN is needed inside the container")
	}
	stateDir := t.TempDir()
	origin := filepath.Join(stateDir, "origin.git")
	clone := filepath.Join(t.TempDir(), "clone")
	worktree := filepath.Join(t.TempDir(), "wt")
	git := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "commit.gpgsign=false"}, args...)...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(origin, 0o755); err != nil {
		t.Fatal(err)
	}
	git(origin, "init", "-q", "--bare", "-b", "main")
	git(filepath.Dir(clone), "clone", "-q", origin, clone)
	if err := os.WriteFile(filepath.Join(clone, "README.md"), []byte("# fixture\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(clone, "add", "README.md")
	git(clone, "commit", "-q", "-m", "init")
	git(clone, "push", "-q", "origin", "HEAD:main")
	git(clone, "worktree", "add", "-q", worktree, "-b", "bees/e2e")

	beesBin := filepath.Join(t.TempDir(), "bees")
	if out, err := exec.Command("go", "build", "-o", beesBin, "../../cmd/bees").CombinedOutput(); err != nil {
		t.Fatalf("build bees: %v\n%s", err, out)
	}
	r := &Runner{
		BeesBin:     beesBin,
		SessionsDir: filepath.Join(stateDir, "sessions"),
		StateDir:    stateDir,
		Repo:        "kpenfound/busybees",
		Label:       "bees",
		GitHub:      config.GitHub{Login: "penfoundapps", Token: "$GH_TOKEN", GitName: "bees e2e", GitEmail: "bees@example.com"},
		AddDirs:     []string{stateDir},
	}
	role := config.ResolvedRole{Name: "developer", Model: "sonnet", MaxTurns: 30, Timeout: 10 * time.Minute,
		Sandbox: config.SandboxContainer, SandboxImage: image, Env: map[string]string{"E2E_MARKER": "from-role-env"}}
	prompt := `You are running inside a container. Do these steps in order with the Bash tool, one command per step. Never retry a failed command; if one fails, quote its error verbatim in your final message and skip to the last step.
1. Run: gh api user --jq .login
2. Run: printf '%s\n' "$E2E_MARKER" > marker.txt && git add marker.txt && git commit -q -m "e2e: hello from the container" && git log --oneline -1
3. Run: git push -q -u origin HEAD && git status -sb | head -1
4. Run: ls "$BEES_STATE_DIR" && echo ok > "$BEES_SESSION_DIR/wrote-from-inside" && echo written
5. Run: (touch /outside-the-box 2>&1 || true); ls -la /home/bees | head -3; echo "HOME=$HOME"; id -u
6. Use the mail_send tool: to project_manager, subject "e2e", body the login printed in step 1.
7. Call the done tool with status "pr-opened", pr 1 and a note holding the login from step 1 and the commit line from step 2.
Your final message must list the output of every step.`
	res, err := r.Run(context.Background(), Request{Name: "e2e-container", Role: role, WorkDir: worktree, SystemPrompt: "Answer briefly.", Prompt: prompt, Env: map[string]string{EnvIssue: "448"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("session dir: %s", res.SessionDir)
	t.Logf("result (%s, error=%v, turns=%d, cost=%.3f):\n%s", res.ErrorSubtype, res.IsError, res.NumTurns, res.CostUSD, res.ResultText)
	if stderr, err := os.ReadFile(filepath.Join(res.SessionDir, "stderr.log")); err == nil {
		t.Logf("stderr:\n%s", stderr)
	}
	if res.IsError {
		t.Fatalf("session failed: %+v", res)
	}
	if !res.HasOutcome || res.Outcome.Status != "pr-opened" || !strings.Contains(res.Outcome.Note, "penfoundapps") {
		t.Errorf("outcome reported through the HTTP server: %+v", res.Outcome)
	}
	if got := git(origin, "log", "--oneline", "-1", "bees/e2e"); !strings.Contains(got, "e2e: hello from the container") {
		t.Errorf("origin did not receive the push: %q", got)
	}
	if got := git(origin, "show", "bees/e2e:marker.txt"); got != "from-role-env" {
		t.Errorf("the role's env inside the container: %q", got)
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "wrote-from-inside")); err != nil {
		t.Errorf("the state dir was not written from inside: %v", err)
	}
	mail, err := os.ReadDir(filepath.Join(stateDir, "mail", "project_manager"))
	if err != nil || len(mail) != 1 {
		t.Errorf("mail from inside the container: %v %v", mail, err)
	}
	if _, err := os.Stat("/outside-the-box"); err == nil {
		t.Error("the session wrote to the host's root")
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, procs.ContainerIDFile)); err == nil {
		t.Error("container id left behind")
	}
}
