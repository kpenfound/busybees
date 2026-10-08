package eval

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/mail"
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
// pipeline's brief and angle sessions answer a brief and no findings, and a
// grader session answers FAKE_SCORE. Run as `codex app-server` instead of
// `codex exec --json ...`, it does the same work over the JSON-RPC
// conversation on stdin and stdout instead (fakeCodexAppServer).
// FAKE_DEV_SEES_PR makes the developer write what the GitHub it was given
// says about the pull request it was handed into seen-pr.txt in the state
// directory, which is how a test reads what a session saw.
// FAKE_COST is each session's cost, FAKE_DEV_HANG and FAKE_REVIEW_HANG make
// the developer or the reviewer hang that many seconds, and FAKE_DEV_FAIL
// makes the developer report failed. FAKE_PM and FAKE_QA make those two
// roles do the work a per-role case grades them on: the project manager
// refines #1, closes #3 as a duplicate and answers the developer about #2,
// and QA files a bug and reports to the product manager.
func TestMain(m *testing.M) {
	if len(os.Args) > 3 && slices.Equal(os.Args[1:3], ShimCommand) {
		dir, _ := os.Getwd()
		os.Exit(Shim(context.Background(), os.Args[3], dir, os.Args[4:], os.Stdin, os.Stdout, os.Stderr))
	}
	if os.Getenv("FAKE_CLAUDE") == "1" {
		fakeClaude()
		os.Exit(0)
	}
	session.HostEnv = append(session.HostEnv, "FAKE_*", "GORACE")
	// Built with -race, every process sleeps a second as it exits
	// (atexit_sleep_ms), and every fake session is one.
	if os.Getenv("GORACE") == "" {
		_ = os.Setenv("GORACE", "atexit_sleep_ms=0")
	}
	for _, k := range []string{session.EnvRole, session.EnvSessionDir, session.EnvStateDir, session.EnvRepo,
		session.EnvLabel, session.EnvIssue, session.EnvPR, session.EnvBranch, session.EnvConfig, session.EnvBin} {
		_ = os.Unsetenv(k)
	}
	os.Exit(m.Run())
}

// isReviewSession reports whether this process was started as a read-only
// session through the shared restricted execution — a grader, or the review
// pipeline's brief or one of its angles: claude is held to empty setting
// sources, opencode to its pure mode and pi to its read-only tool set. An
// ordinary role session carries none of those. Codex, restricted or not,
// always runs as `codex app-server`, so fakeClaude dispatches it to
// fakeCodexAppServer before this check is ever reached; that function
// tells a review session apart by thread/start's sandbox instead.
func isReviewSession() bool {
	switch {
	case slices.Contains(os.Args, "--setting-sources"),
		slices.Contains(os.Args, "--no-tools"),
		len(os.Args) > 1 && os.Args[1] == "--pure":
		return true
	}
	return false
}

// sendMail is a fake session writing to the mailbox, which a real one does
// through the built-in MCP server's mail_send.
func sendMail(fail func(error), from, to, subject, body string, issue int) {
	sendMailAbout(fail, from, to, subject, body, issue, 0)
}

// sendMailAbout is sendMail for a message about a pull request as well as
// an issue, the way the reviewer's feedback to the developer is.
func sendMailAbout(fail func(error), from, to, subject, body string, issue, pr int) {
	box := mail.Open(filepath.Join(os.Getenv(session.EnvStateDir), "mail"))
	if _, err := box.Send(mail.Message{From: from, To: to, Subject: subject, Body: body, Work: ghwork.New(issue, pr)}); err != nil {
		fail(err)
	}
}

func fakeClaude() {
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "fake claude:", err)
		os.Exit(2)
	}
	// codex's app server, started as `codex app-server` instead of `codex
	// exec --json ...`: the conversation runs over stdin and stdout instead
	// of args and a stream of exec's own events, but the scripted work it
	// does is the same.
	if len(os.Args) > 1 && os.Args[1] == "app-server" {
		fakeCodexAppServer(fail)
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		fmt.Println("[]")
		return
	}
	// The configuration inventory a restricted opencode run probes with
	// before the model: what a CLI honoring the inline restrictions
	// resolves.
	// The search for custom tools a held opencode turn runs first, with
	// the executable started as its JavaScript runtime.
	if os.Getenv("BUN_BE_BUN") == "1" {
		fmt.Println(agenttest.OpenCodeNoCustomTools)
		return
	}
	if len(os.Args) > 3 && os.Args[1] == "--pure" && os.Args[2] == "debug" {
		fmt.Println(`{"agent":{"bees-read-only":{"mode":"primary","permission":{"*":"deny","read":"allow","grep":"allow","glob":"allow"}}},"mcp":{}}`)
		return
	}
	// A read-only session through the shared restricted execution — a
	// grader, or the review pipeline's brief or one of its angles. It runs
	// as whichever agent the person's review configuration selects; the
	// fake answers in each backend's stream format.
	if isReviewSession() {
		prompt, _ := io.ReadAll(os.Stdin)
		answer := reviewAnswer(string(prompt))
		switch {
		case len(os.Args) > 1 && os.Args[1] == "--pure":
			for _, ev := range []string{
				`{"type":"text","sessionID":"open-review","part":{"type":"text","text":` + strconv.Quote(answer) + `}}`,
				`{"type":"step_finish","sessionID":"open-review","part":{"type":"step-finish","reason":"stop","cost":0}}`,
			} {
				fmt.Println(ev)
			}
		case slices.Contains(os.Args, "--no-tools"):
			for _, ev := range []string{
				`{"type":"session","id":"pi-review"}`,
				`{"type":"message_end","message":{"role":"assistant","provider":"anthropic","model":"claude-sonnet","content":[{"type":"text","text":` + strconv.Quote(answer) + `}],"stopReason":"stop","usage":{"cost":{"total":0}}}}`,
				`{"type":"turn_end"}`,
			} {
				fmt.Println(ev)
			}
		default:
			fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":%s,"session_id":"sid-review","num_turns":1,"total_cost_usd":0}`+"\n", strconv.Quote(answer))
		}
		return
	}
	runFakeRole(fail)
	cost := firstNonEmpty(os.Getenv("FAKE_COST"), "0.01")
	fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":"ok","session_id":"sid","num_turns":2,"total_cost_usd":%s}`+"\n", cost)
}

// reviewAnswer is what a read-only review session answers, from the prompt
// it is given: a rubric's score, an angle's findings (none), or the brief
// the pipeline grades the pull request against.
func reviewAnswer(text string) string {
	lines := strings.Split(strings.TrimSpace(text), "\n")
	switch {
	case strings.Contains(text, "## The rubric"):
		return fmt.Sprintf(`{"score": %s, "reasons": "the fake grader read the rubric"}`, firstNonEmpty(os.Getenv("FAKE_SCORE"), "0.9"))
	case !strings.Contains(lines[len(lines)-1], "from the ") || !strings.Contains(lines[len(lines)-1], " angle"):
		return `{"summary":"Fixes the answer.","size":"xs","acceptance_criteria":[],"touched_areas":[]}`
	}
	return `{"findings":[]}`
}

// runFakeRole does the role's scripted work through gh and git on the real
// PATH — the developer commits the fix and opens or updates a pull
// request, the reviewer approves or requests changes, the project manager
// and QA do their case's scripted actions under FAKE_PM and FAKE_QA, and
// every other role does nothing — writes the session's outcome and
// returns it.
func runFakeRole(fail func(error)) session.Outcome {
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
		if os.Getenv("FAKE_DEV_SEES_PR") == "1" && pr != 0 {
			n := strconv.Itoa(pr)
			seen := gh("", "pr", "view", n, "-R", repo, "--json", "title,body,headRefName,baseRefName,labels") +
				gh("", "pr", "diff", n, "-R", repo) +
				gh("", "api", fmt.Sprintf("repos/%s/issues/%s/comments", repo, n)) +
				gh("", "api", fmt.Sprintf("repos/%s/pulls/%s/reviews", repo, n))
			if err := os.WriteFile(filepath.Join(os.Getenv(session.EnvStateDir), "seen-pr.txt"), []byte(seen), 0o644); err != nil {
				fail(err)
			}
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
		if hang, _ := strconv.Atoi(os.Getenv("FAKE_REVIEW_HANG")); hang > 0 {
			time.Sleep(time.Duration(hang) * time.Second)
		}
		if os.Getenv("FAKE_REVIEW_CHANGES") == "1" {
			gh("answer.txt still says broken\n\n<!-- bees:reviewer -->", "pr", "review", strconv.Itoa(pr), "-R", repo, "--request-changes", "--body-file", "-")
			sendMailAbout(fail, config.RoleReviewer, config.RoleDeveloper, "Changes requested on #"+strconv.Itoa(pr),
				"answer.txt still says broken.", issue, pr)
			outcome = session.Outcome{Status: "changes-requested", Note: "one finding", Work: ghwork.New(issue, pr)}
			break
		}
		gh("looks right\n\n<!-- bees:reviewer -->", "pr", "review", strconv.Itoa(pr), "-R", repo, "--comment", "--body-file", "-")
		outcome = session.Outcome{Status: "approved", Note: "lgtm"}
	case config.RoleProjectManager:
		if os.Getenv("FAKE_PM") != "1" {
			break
		}
		gh("", "issue", "edit", "1", "-R", repo, "--body", "Make `answer.txt` say `fixed`, and nothing else.",
			"--add-label", "bees:ready", "--add-label", "bees:size/xs", "--remove-label", "bees:triage")
		gh("", "issue", "close", "3", "-R", repo, "--comment", "Duplicate of #1.\n\n<!-- bees:project_manager -->")
		sendMail(fail, config.RoleProjectManager, config.RoleDeveloper, "Re: what does fixed mean", "The word `fixed`, on one line.", 2)
	case config.RoleQA:
		if os.Getenv("FAKE_QA") != "1" {
			break
		}
		gh("", "issue", "create", "-R", repo, "--title", "greet.sh drops the comma", "--body", "It prints `Hello Ada`.",
			"--label", "bees", "--label", "bees:bug", "--label", "bees:triage")
		sendMail(fail, config.RoleQA, config.RoleProductManager, "QA report", "One bug filed.", 0)
	}
	if err := session.WriteOutcome(sessionDir, outcome); err != nil {
		fail(err)
	}
	return outcome
}

// fakeCodexAppServer is the codex fake started as `codex app-server`
// instead of `codex exec --json ...`: it speaks the JSON-RPC conversation
// over stdin and stdout — initialize, an optional config/read, thread/start
// or thread/resume, then turn/start — and does the same scripted work
// isReviewSession and runFakeRole do for `codex exec`, reported through
// item/completed and turn/completed notifications instead of exec's own
// stream. A thread/start whose sandbox is "read-only" is a review session,
// as the sandbox table gives a restricted turn; any other sandbox is an
// ordinary role session. It exits when the client closes stdin, as the
// real app server does.
func fakeCodexAppServer(fail func(error)) {
	type message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params struct {
			Sandbox string `json:"sandbox"`
			Input   string `json:"input"`
		} `json:"params"`
	}
	in := bufio.NewScanner(os.Stdin)
	in.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	out := json.NewEncoder(os.Stdout)
	send := func(v any) {
		if err := out.Encode(v); err != nil {
			fail(err)
		}
	}
	const threadID = "thread-fake"
	review := false
	for in.Scan() {
		var msg message
		if err := json.Unmarshal(in.Bytes(), &msg); err != nil {
			fail(fmt.Errorf("fake codex app-server: %w", err))
			continue
		}
		switch msg.Method {
		case "initialized":
			// A notification: no response.
		case "initialize", "config/read":
			result := map[string]any{}
			if msg.Method == "config/read" {
				result = map[string]any{"config": map[string]any{}, "origins": map[string]any{}}
			}
			send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": result})
		case "thread/start", "thread/resume":
			review = msg.Params.Sandbox == "read-only"
			send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{"thread": map[string]any{"id": threadID}}})
		case "turn/start":
			send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "result": map[string]any{}})
			var text string
			if review {
				text = reviewAnswer(msg.Params.Input)
			} else {
				text = runFakeRole(fail).Status
			}
			send(map[string]any{"jsonrpc": "2.0", "method": "item/completed",
				"params": map[string]any{"item": map[string]any{"type": "agent_message", "text": text}}})
			send(map[string]any{"jsonrpc": "2.0", "method": "turn/completed", "params": map[string]any{"turn": map[string]any{"status": "completed"}}})
		default:
			if len(msg.ID) > 0 {
				send(map[string]any{"jsonrpc": "2.0", "id": msg.ID, "error": map[string]any{"code": -32601, "message": "method not supported in this fake"}})
			}
		}
	}
}
