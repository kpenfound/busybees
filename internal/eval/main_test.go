package eval

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/workspace"
)

// TestMain lets the test binary double as the eval's gh, when it is run as
// ShimCommand (the script the runner writes runs the bees binary, which in
// these tests is this one), and as a fake claude when FAKE_CLAUDE is set.
//
// The fake claude does what a role's session does through the tools a real
// one has: the developer commits the fix (answer.txt saying "fixed", or
// under FAKE_DEV_NOFIX a file nothing reads), pushes and opens a pull
// request with `gh pr create`, the reviewer submits its review with `gh pr review`, both
// through the gh on its PATH, and every other role reports done. The review
// pipeline's brief and angle sessions answer a brief and no findings.
// FAKE_COST is each session's cost, FAKE_DEV_HANG makes the developer hang
// and FAKE_DEV_FAIL makes it report failed.
func TestMain(m *testing.M) {
	if len(os.Args) > 3 && slices.Equal(os.Args[1:3], ShimCommand) {
		dir, _ := os.Getwd()
		os.Exit(Shim(context.Background(), os.Args[3], dir, os.Args[4:], os.Stdin, os.Stdout, os.Stderr))
	}
	if os.Getenv("FAKE_CLAUDE") == "1" {
		fakeClaude()
		os.Exit(0)
	}
	session.HostEnv = append(session.HostEnv, "FAKE_*")
	for _, k := range []string{session.EnvRole, session.EnvSessionDir, session.EnvStateDir, session.EnvRepo,
		session.EnvLabel, session.EnvIssue, session.EnvPR, session.EnvBranch, session.EnvConfig, session.EnvBin} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

func fakeClaude() {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "fake claude:", err)
		os.Exit(2)
	}
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		fmt.Println("[]")
		return
	}
	// A brief or angle session of the review pipeline: asked for json, not
	// stream-json.
	if i := slices.Index(os.Args, "--output-format"); i >= 0 && i+1 < len(os.Args) && os.Args[i+1] == "json" {
		prompt, _ := io.ReadAll(os.Stdin)
		lines := strings.Split(strings.TrimSpace(string(prompt)), "\n")
		answer := `{"findings":[]}`
		if last := lines[len(lines)-1]; !strings.Contains(last, "from the ") || !strings.Contains(last, " angle") {
			answer = `{"summary":"Fixes the answer.","size":"xs","acceptance_criteria":[],"touched_areas":[]}`
		}
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":%s,"session_id":"sid-review","num_turns":1,"total_cost_usd":0}`+"\n", strconv.Quote(answer))
		return
	}
	sessionDir := os.Getenv(session.EnvSessionDir)
	issue, _ := strconv.Atoi(os.Getenv(session.EnvIssue))
	pr, _ := strconv.Atoi(os.Getenv(session.EnvPR))
	repo := os.Getenv(session.EnvRepo)
	gh := func(stdin string, args ...string) string {
		cmd := exec.Command("gh", args...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Stderr = os.Stderr
		out, err := cmd.Output()
		if err != nil {
			fail(fmt.Errorf("gh %v: %w", args, err))
		}
		return string(out)
	}
	git := func(args ...string) {
		if _, err := workspace.Git(context.Background(), ".", args...); err != nil {
			fail(err)
		}
	}
	outcome := session.Outcome{Status: "done", Note: "ok"}
	switch os.Getenv(session.EnvRole) {
	case config.RoleDeveloper:
		if hang, _ := strconv.Atoi(os.Getenv("FAKE_DEV_HANG")); hang > 0 {
			time.Sleep(time.Duration(hang) * time.Second)
		}
		if os.Getenv("FAKE_DEV_FAIL") == "1" {
			outcome = session.Outcome{Status: "failed", Note: "cannot build"}
			break
		}
		file, content := "answer.txt", "fixed\n"
		if os.Getenv("FAKE_DEV_NOFIX") == "1" {
			file, content = "notes.txt", "looked at it\n"
		}
		if err := os.WriteFile(file, []byte(content), 0o644); err != nil {
			fail(err)
		}
		git("add", ".")
		git("-c", "user.email=bee@example.com", "-c", "user.name=bee", "-c", "commit.gpgsign=false", "commit", "-q", "-m", "work")
		git("push", "-q", "origin", "HEAD")
		if pr == 0 {
			// A relative body file, which the shim makes absolute for the
			// server that reads it.
			if err := os.WriteFile("pr-body.md", []byte(fmt.Sprintf("Fixes the answer.\n\nCloses #%d", issue)), 0o644); err != nil {
				fail(err)
			}
			url := strings.TrimSpace(gh("", "pr", "create", "-R", repo, "--base", "main", "--head", os.Getenv(session.EnvBranch),
				"--label", "bees", "--title", "Fix the answer", "--body-file", "pr-body.md"))
			var err error
			pr, err = strconv.Atoi(url[strings.LastIndex(url, "/")+1:])
			if err != nil {
				fail(err)
			}
			outcome = session.Outcome{Status: "pr-opened", Work: ghwork.New(issue, pr)}
		} else {
			outcome = session.Outcome{Status: "pr-updated", Work: ghwork.New(issue, pr)}
		}
	case config.RoleReviewer:
		gh("looks right\n\n<!-- bees:reviewer -->", "pr", "review", strconv.Itoa(pr), "-R", repo, "--comment", "--body-file", "-")
		outcome = session.Outcome{Status: "approved", Note: "lgtm"}
	}
	if err := session.WriteOutcome(sessionDir, outcome); err != nil {
		fail(err)
	}
	cost := firstNonEmpty(os.Getenv("FAKE_COST"), "0.01")
	fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"sid","num_turns":2,"total_cost_usd":%s}`+"\n", cost)
}
