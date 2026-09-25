package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/fakegh"
	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/logging"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

// TestMain lets the test binary double as a fake `claude` — and a fake
// `codex`, `opencode` or `pi`, which it tells apart by its first arguments,
// codex's `exec`, opencode's `run` or pi's `-p --mode` — when FAKE_CLAUDE is
// set: the runner executes it, it inspects its role and environment,
// performs a scripted action and prints a stream-json result, or codex's,
// opencode's or pi's event stream when it is one of those.
//
// The flags that steer the fake (FAKE_CLAUDE, FAKE_DEV_HANG, FAKE_DEV_FAIL,
// FAKE_DEV_MAIL_TO, FAKE_ATTEMPT_FAIL, FAKE_ASSEMBLE_FAIL, FAKE_REVIEW_ALWAYS_CHANGES,
// FAKE_REVIEW_FAIL, FAKE_COST, FAKE_SIGNAL,
// FAKE_WAIT_FOR, FAKE_LIMIT, FAKE_ASSEMBLE_LIMIT, FAKE_LIMIT_WITH_OUTCOME, FAKE_RESULT_TEXT, FAKE_COPY_ISSUE_STATE,
// FAKE_TRIAGE, FAKE_FILE_ISSUE, FAKE_RESUME_FAIL, FAKE_REVIEW_NO_SUBMIT,
// FAKE_REVIEW_MISMATCH, FAKE_QA_NO_REPORT, FAKE_QA_OTHER_MAIL)
// reach it through the ordinary environment, so they must NOT start with
// BEES_: the runner strips inherited BEES_* variables from every session.
func TestMain(m *testing.M) {
	if os.Getenv("FAKE_CLAUDE") == "1" {
		fakeClaude()
		os.Exit(0)
	}
	// The fake agent is configured through FAKE_* variables of this process,
	// which a session inherits only when granted.
	session.HostEnv = append(session.HostEnv, "FAKE_*")
	// The runner drops inherited BEES_* variables, but the tests run this
	// binary directly too (and read the environment themselves), so clear the
	// ones a bees session would have exported: `go test` run from inside a
	// session must behave like one run from a plain shell.
	for _, k := range []string{session.EnvRole, session.EnvSessionDir, session.EnvStateDir, session.EnvRepo,
		session.EnvLabel, session.EnvIssue, session.EnvPR, session.EnvBranch,
		session.EnvConfig, session.EnvBin} {
		if err := os.Unsetenv(k); err != nil {
			fmt.Fprintln(os.Stderr, "unset:", err)
			os.Exit(2)
		}
	}
	os.Exit(m.Run())
}

const fakePR = 101

// fakeAssemble is the fake assembler session. Under FAKE_ASSEMBLE_FAIL it
// reports `failed`; otherwise it reads the attempts off its task, resets
// the issue's branch to the first candidate's remote branch, pushes it and
// reports `pr-opened`, recording the branch it took in the session
// directory as assembled.txt for tests to read.
func fakeAssemble(sessionDir, stateDir string, git func(args ...string), fail func(error)) session.Outcome {
	if os.Getenv("FAKE_ASSEMBLE_FAIL") == "1" {
		return session.Outcome{Status: OutcomeFailed, Note: "no attempt builds"}
	}
	prompt, err := os.ReadFile(filepath.Join(sessionDir, "prompt.md"))
	if err != nil {
		fail(err)
	}
	var pick string
	for _, line := range strings.Split(string(prompt), "\n") {
		if !strings.HasPrefix(line, "- `") || strings.Contains(line, "not a candidate") {
			continue
		}
		if branch, _, ok := strings.Cut(strings.TrimPrefix(line, "- `"), "`"); ok {
			pick = branch
			break
		}
	}
	if pick == "" {
		fail(fmt.Errorf("the assembler's task lists no candidate:\n%s", prompt))
	}
	git("fetch", "-q", "origin")
	git("reset", "-q", "--hard", "origin/"+pick)
	git("push", "-q")
	if err := os.WriteFile(filepath.Join(sessionDir, "assembled.txt"), []byte(pick), 0o644); err != nil {
		fail(err)
	}
	for _, marker := range []string{"fake-pr-created", "fake-pr-created-issue-" + os.Getenv(session.EnvIssue)} {
		if err := os.WriteFile(filepath.Join(stateDir, marker), nil, 0o644); err != nil {
			fail(err)
		}
	}
	return session.Outcome{Status: OutcomePROpened, Work: ghwork.New(0, fakePR)}
}

// fakeDiff is what the fake gh answers `pr diff` with: one file, so the
// review's diff source gathers something and a finding has a line to
// anchor to.
const fakeDiff = "diff --git a/widget.go b/widget.go\n--- a/widget.go\n+++ b/widget.go\n@@ -1,2 +1,3 @@\n package widgets\n+func Widget() {}\n"

// isReviewSession tells a brief or angle session of the review pipeline
// (internal/review's CLIAgent, run through shared restricted execution) from a factory
// session: claude's is asked for `--output-format json` where the runner
// asks for stream-json, and codex's runs in its read-only sandbox where the
// runner bypasses it.
// isReviewSession reports whether this process was started as a review
// pipeline session — the brief, an angle, a grader or a triage session —
// through the shared restricted execution. Claude is held to empty setting
// sources, codex to its read-only sandbox, opencode to its pure mode and
// pi to its read-only tool set; an ordinary session carries none of those.
func isReviewSession() bool {
	switch {
	case slices.Contains(os.Args, "--setting-sources"),
		slices.Contains(os.Args, "--no-tools"):
		return true
	case len(os.Args) > 1 && os.Args[1] == "exec" && slices.Contains(os.Args, "read-only"),
		len(os.Args) > 1 && os.Args[1] == "--pure":
		return true
	}
	return false
}

// fakeReviewSession is the fake distiller or angle session. It reads its
// prompt from stdin, tells the two apart by the prompt's last line (the
// distiller's asks for the brief, an angle's names the angle), records the
// session in the file FAKE_REVIEW_LOG names when it is set — one JSON line
// per session: the kind, the command line and the directory it ran in —
// and answers as the CLI it was started as: claude's result object, or
// codex's event stream.
//
// The brief sizes the change FAKE_REVIEW_SIZE, "s" when unset. Each angle
// reports one finding, titled after the angle so the judge keeps them all
// apart, unless FAKE_REVIEW_EMPTY is set; FAKE_ANGLE_FAIL names an angle
// (its prompt title, "documentation accuracy"), or "all", whose session
// dies instead.
func fakeReviewSession() {
	prompt, _ := io.ReadAll(os.Stdin)
	lines := strings.Split(strings.TrimSpace(string(prompt)), "\n")
	last := lines[len(lines)-1]
	kind := "brief"
	if _, after, ok := strings.Cut(last, "from the "); ok {
		kind, _, _ = strings.Cut(after, " angle")
	}
	if p := os.Getenv("FAKE_REVIEW_LOG"); p != "" {
		dir, _ := os.Getwd()
		var files []string
		if entries, err := os.ReadDir(dir); err == nil {
			for _, e := range entries {
				files = append(files, e.Name())
			}
		}
		rec, _ := json.Marshal(map[string]any{"kind": kind, "args": os.Args[1:], "dir": dir, "files": files, "role": os.Getenv(session.EnvRole), "prompt": string(prompt)})
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			fmt.Fprintln(os.Stderr, "fake review session:", err)
			os.Exit(2)
		}
		_, _ = f.Write(append(rec, '\n'))
		_ = f.Close()
	}
	if fail := os.Getenv("FAKE_ANGLE_FAIL"); kind != "brief" && (fail == kind || fail == "all") {
		fmt.Fprintln(os.Stderr, "fake review session: the model is overloaded")
		os.Exit(1)
	}
	var answer string
	if kind == "brief" {
		size := os.Getenv("FAKE_REVIEW_SIZE")
		if size == "" {
			size = "s"
		}
		answer = fmt.Sprintf(`{"summary":"Adds the widget.","size":%q,"acceptance_criteria":[{"text":"a widget exists","source":"#1"}],"touched_areas":[{"name":"widgets","paths":["widget.go"],"summary":"adds Widget"}]}`, size)
	} else if os.Getenv("FAKE_REVIEW_EMPTY") == "1" {
		answer = `{"findings":[]}`
	} else {
		// Anchored to a line of its own per angle (the length of the
		// angle's title), so the judge does not fold two angles' findings
		// into one.
		answer = fmt.Sprintf(`{"findings":[{"category":"correctness","severity":"medium","file":"widget.go","lines":[%d,%d],"side":"new","title":"%s: Widget does nothing","body":"Widget has an empty body (from the %s angle).","evidence":"func Widget() {}"}]}`, len(kind), len(kind), kind, kind)
	}
	// The session answers in its own backend's stream format: the fake is
	// started as claude, codex, opencode or pi, whichever the reviewer's
	// configuration selects, and the shared execution reads each its way.
	switch {
	case len(os.Args) > 1 && os.Args[1] == "exec" && slices.Contains(os.Args, "read-only"):
		for _, ev := range []string{
			`{"type":"thread.started","thread_id":"thread-` + kind + `"}`,
			`{"type":"item.completed","item":{"type":"agent_message","text":` + strconv.Quote(answer) + `}}`,
			`{"type":"turn.completed"}`,
		} {
			fmt.Println(ev)
		}
	case len(os.Args) > 1 && os.Args[1] == "--pure":
		for _, ev := range []string{
			`{"type":"text","sessionID":"` + kind + `","part":{"type":"text","text":` + strconv.Quote(answer) + `}}`,
			`{"type":"step_finish","sessionID":"` + kind + `","part":{"type":"step-finish","reason":"stop","cost":0.25}}`,
		} {
			fmt.Println(ev)
		}
	case slices.Contains(os.Args, "--no-tools"):
		for _, ev := range []string{
			`{"type":"session","id":"` + kind + `"}`,
			`{"type":"message_end","message":{"role":"assistant","provider":"anthropic","model":"claude-sonnet","content":[{"type":"text","text":` + strconv.Quote(answer) + `}],"stopReason":"stop","usage":{"cost":{"total":0.75}}}}`,
			`{"type":"turn_end"}`,
		} {
			fmt.Println(ev)
		}
	default:
		fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":%s,"session_id":"sid-review-%s","num_turns":1,"total_cost_usd":0.25}`+"\n", strconv.Quote(answer), kind)
	}
}

// findingsOf is the `## Findings` section of a judge session's task, which
// is what the fake posts as its review's body.
func findingsOf(prompt string) string {
	_, after, ok := strings.Cut(prompt, "## Findings")
	if !ok {
		return "(no findings section)"
	}
	before, _, _ := strings.Cut(after, "## Instructions")
	return strings.TrimSpace(before)
}

func fakeClaude() {
	if len(os.Args) > 1 && os.Args[1] == "mcp" {
		fmt.Println("[]")
		return
	}
	// The configuration inventory a restricted opencode run probes with
	// before the model: what a CLI honoring the inline restrictions
	// resolves, with no inherited server and the read-only agent in force.
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
	// A brief or angle session of the review pipeline has no role, no
	// session directory and no state directory: it is answered before any
	// of those is looked at.
	if isReviewSession() {
		fakeReviewSession()
		return
	}
	role := os.Getenv(session.EnvRole)
	sessionDir := os.Getenv(session.EnvSessionDir)
	stateDir := os.Getenv(session.EnvStateDir)
	box := mail.Open(filepath.Join(stateDir, "mail"))
	fail := func(err error) {
		fmt.Fprintln(os.Stderr, "fake claude:", err)
		os.Exit(2)
	}
	// The runner starts codex as `codex exec --json ...` and opencode as
	// `opencode run --format json ...`; claude never gets either. A codex or
	// opencode session prints its own events instead of claude's
	// stream-json, and the runner reads each stream its own way.
	codex := len(os.Args) > 1 && os.Args[1] == "exec"
	opencode := len(os.Args) > 1 && os.Args[1] == "run"
	pi := len(os.Args) > 2 && os.Args[1] == "-p" && os.Args[2] == "--mode"
	if codex || opencode || pi {
		// The prompt is on stdin for codex, opencode and pi; claude reads
		// it there too, but only those close with an error when it is left
		// unread.
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	if pi {
		// pi-mcp-adapter reads only the --mcp-config file when
		// PI_MCP_CONFIG_MODE says so; record it so tests can see it set.
		if err := os.WriteFile(filepath.Join(sessionDir, "pi-mcp-mode.txt"), []byte(os.Getenv(session.EnvPiMCPConfigMode)), 0o644); err != nil {
			fail(err)
		}
	}
	if opencode {
		// opencode is configured through the file OPENCODE_CONFIG names,
		// which it reads for its MCP servers and instructions; record the
		// path so tests can see where the runner put it.
		if err := os.WriteFile(filepath.Join(sessionDir, "opencode-config.txt"), []byte(os.Getenv(session.EnvOpenCodeConfig)), 0o644); err != nil {
			fail(err)
		}
	}
	// Record the command line so tests can assert on the flags the runner
	// built, the way internal/session's fake does.
	if err := os.WriteFile(filepath.Join(sessionDir, "args.txt"), []byte(strings.Join(os.Args, "\n")), 0o644); err != nil {
		fail(err)
	}
	git := func(args ...string) {
		if _, err := workspace.Git(context.Background(), ".", args...); err != nil {
			fail(err)
		}
	}
	// Record the order sessions ran in, so tests can assert the sequence
	// across roles (a session directory name carries a one-second timestamp,
	// so sorting the directories is not chronological).
	if f, err := os.OpenFile(filepath.Join(stateDir, "fake-order"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		_, _ = fmt.Fprintln(f, filepath.Base(sessionDir))
		_ = f.Close()
	}
	// The session id claude reports is derived from the --name the runner
	// passed ("bees-developer-issue-1-r1" -> "sid-developer-issue-1-r1"), so
	// a test can predict the id a later round must resume with.
	// An opencode session's id is derived from its --title the same way,
	// and a pi session's from its --name like claude's.
	sessionID := "fake"
	for _, flag := range []string{"--name", "--title"} {
		if i := slices.Index(os.Args, flag); i >= 0 && i+1 < len(os.Args) {
			sessionID = "sid-" + strings.TrimPrefix(os.Args[i+1], "bees-")
		}
	}
	// FAKE_RESUME_FAIL makes a resumed launch die the way real claude does
	// with a session id it no longer has: before any result event, with a
	// non-zero exit. A launch with no --resume runs normally, so the retry
	// the runner makes without the flag is the one that does the work.
	if os.Getenv("FAKE_RESUME_FAIL") == "1" && slices.Contains(os.Args, "--resume") {
		fmt.Fprintln(os.Stderr, "No conversation found with session ID:", os.Args[slices.Index(os.Args, "--resume")+1])
		os.Exit(1)
	}
	// FAKE_WAIT_FOR holds the session "running" until the named file exists:
	// how a test acts on a session that is observably in flight — cancelling
	// the loop, hard-stopping the factory — with no timing window, because
	// the session cannot finish before the test creates the file. The wait is
	// capped so a test that never creates it still ends.
	if p := os.Getenv("FAKE_WAIT_FOR"); p != "" {
		for i := 0; i < 3000; i++ {
			if _, err := os.Stat(p); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	// FAKE_LIMIT makes a session hit the account-wide claude session limit,
	// whatever its role: it emits a blocking rate_limit_event. The value is
	// the resetsAt unix timestamp, or "none" for an event that carried
	// none. The session then dies without reporting an outcome, the way one
	// that could not start does — unless FAKE_LIMIT_WITH_OUTCOME is set, in
	// which case the role does its work and reports it, which is a session
	// that finished just as the account ran out of capacity.
	//
	// FAKE_ASSEMBLE_LIMIT is the same, but only for the best-of-N assembler
	// session (told apart by "-assemble" in its name): every attempt runs
	// and pushes normally, and only the session that would compare them
	// hits the limit — the one case bestofn.go treats differently from an
	// ordinary developer session hitting it.
	if v := os.Getenv("FAKE_LIMIT"); v != "" || (strings.Contains(sessionID, "-assemble") && os.Getenv("FAKE_ASSEMBLE_LIMIT") != "") {
		if v == "" {
			v = os.Getenv("FAKE_ASSEMBLE_LIMIT")
		}
		resets := ""
		if v != "none" {
			resets = `,"resetsAt":` + v
		}
		fmt.Printf(`{"type":"rate_limit_event","rate_limit_info":{"status":"blocked","rateLimitType":"five_hour","overageStatus":"allowed"%s}}`+"\n", resets)
		if os.Getenv("FAKE_LIMIT_WITH_OUTCOME") == "" {
			fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":"You've hit your session limit","session_id":%q,"num_turns":1,"total_cost_usd":0.01}`+"\n", sessionID)
			return
		}
	}
	// FAKE_SIGNAL kills this session with the named signal — whatever its
	// role — after writing a few assistant messages, the way a session the
	// OS or a closing terminal kills dies: no result event, so no turn
	// count and no cost from claude, and a wait status rather than an exit
	// code. The value is the signal number.
	if v := os.Getenv("FAKE_SIGNAL"); v != "" {
		sig, err := strconv.Atoi(v)
		if err != nil {
			fail(err)
		}
		for i := 0; i < 3; i++ {
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"working"}]}}`)
		}
		if err := syscall.Kill(os.Getpid(), syscall.Signal(sig)); err != nil {
			fail(err)
		}
		select {} // the signal arrives; never reached
	}
	// FAKE_COPY_ISSUE_STATE copies the issue's bookkeeping as it stands while
	// this session runs to <state_dir>/running-<session dir>.json. It is the
	// only way to see what the scheduler recorded *during* a session, which
	// is exactly what a scheduler killed mid-session leaves behind.
	if os.Getenv("FAKE_COPY_ISSUE_STATE") == "1" {
		if n := os.Getenv(session.EnvIssue); n != "" {
			if b, err := os.ReadFile(state.New(stateDir).WorkPath(ghwork.IssueKey(envIntFixture(n)))); err == nil {
				if err := os.WriteFile(filepath.Join(stateDir, "running-"+filepath.Base(sessionDir)+".json"), b, 0o644); err != nil {
					fail(err)
				}
			}
		}
	}
	counter := func(name string) int {
		p := filepath.Join(stateDir, "fake-"+name)
		n := 0
		if b, err := os.ReadFile(p); err == nil {
			n, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		n++
		_ = os.WriteFile(p, []byte(strconv.Itoa(n)), 0o644)
		return n
	}
	var outcome session.Outcome
	switch role {
	case config.RoleDeveloper:
		// The assembler of a best-of-N fan-out (bestofn.go), told apart by
		// its session name: it takes the first candidate branch its task
		// lists whole, the way the prompt's "one attempt as it stands"
		// path does, and opens the pull request from the issue's branch.
		if strings.Contains(sessionID, "-assemble") {
			outcome = fakeAssemble(sessionDir, stateDir, git, fail)
			break
		}
		n := counter("dev")
		// Infrastructure failures: FAKE_DEV_HANG=N hangs the first N
		// attempts until the role timeout kills them; FAKE_DEV_FAIL reports
		// `failed` outright.
		if hang, _ := strconv.Atoi(os.Getenv("FAKE_DEV_HANG")); n <= hang {
			time.Sleep(time.Minute)
		}
		if os.Getenv("FAKE_DEV_FAIL") == "1" {
			outcome = session.Outcome{Status: OutcomeFailed, Note: "cannot build"}
			break
		}
		// FAKE_ATTEMPT_FAIL makes best-of-N attempt <i> ("all": every
		// attempt) report `failed` without committing or pushing anything:
		// an attempt with nothing on its branch for the assembler to read.
		if v := os.Getenv("FAKE_ATTEMPT_FAIL"); v != "" {
			_, i, isAttempt := strings.Cut(sessionID, "-attempt-")
			if isAttempt && (v == "all" || v == i) {
				outcome = session.Outcome{Status: OutcomeFailed, Note: "attempt " + i + " could not build"}
				break
			}
		}
		// FAKE_DEV_MAIL_TO makes the session write to another role before it
		// finishes, the way a real one does with `bees mail send`: a
		// different process appending to the same mailbox on disk.
		if to := os.Getenv("FAKE_DEV_MAIL_TO"); to != "" {
			if _, err := box.Send(mail.Message{From: role, To: to, Subject: "a question about the queue", Body: "please look at this"}); err != nil {
				fail(err)
			}
		}
		if err := os.WriteFile(fmt.Sprintf("work-%d.txt", n), []byte("done"), 0o644); err != nil {
			fail(err)
		}
		git("add", ".")
		git("-c", "user.email=bee@example.com", "-c", "user.name=bee", "commit", "-q", "-m", fmt.Sprintf("work %d", n))
		git("push", "-q")
		if os.Getenv(session.EnvPR) == "" {
			// "Open" the PR: the fake gh treats the markers as the PR existing
			// (fakePR, and any hidden PR on this issue's branch).
			for _, marker := range []string{"fake-pr-created", "fake-pr-created-issue-" + os.Getenv(session.EnvIssue)} {
				if err := os.WriteFile(filepath.Join(stateDir, marker), nil, 0o644); err != nil {
					fail(err)
				}
			}
			outcome = session.Outcome{Status: OutcomePROpened, Work: ghwork.New(0, fakePR)}
		} else {
			outcome = session.Outcome{Status: OutcomePRUpdated, Work: ghwork.New(0, fakePR)}
		}
	case config.RoleReviewer:
		pr, _ := strconv.Atoi(os.Getenv(session.EnvPR))
		issue, _ := strconv.Atoi(os.Getenv(session.EnvIssue))
		if os.Getenv("BEES_REVIEW_MODE") == "checks" {
			counter("checks")
			prompt, _ := os.ReadFile(filepath.Join(sessionDir, "prompt.md"))
			if _, err := box.Send(mail.Message{From: role, To: config.RoleDeveloper, Subject: "Check failed: go / test", Body: "main error: TestX fails\n\n" + string(prompt), Work: ghwork.New(issue, pr)}); err != nil {
				fail(err)
			}
			outcome = session.Outcome{Status: OutcomeChangesRequested}
			break
		}
		// FAKE_REVIEW_FAIL reports `failed` outright, the way a reviewer
		// that could not review does.
		if os.Getenv("FAKE_REVIEW_FAIL") == "1" {
			counter("review")
			outcome = session.Outcome{Status: OutcomeFailed, Note: "cannot read the diff"}
			break
		}
		// The judge session posts the findings its task carries as one
		// review, the way the prompt tells a real reviewer to: the body is
		// the verdict line and the task's `## Findings` section, submitted
		// through github.Client.SubmitReview. The gh call is recorded in
		// the session directory as review.json ({"args", "stdin", "issue"})
		// for tests to read, since the scheduler's fake gh lives in another
		// process, and the state GitHub would hold is recorded for the
		// harness's fake, which the scheduler reads the review back from.
		// FAKE_REVIEW_NO_SUBMIT reports the verdict without submitting
		// anything, the way a session that hallucinated its status does.
		prompt, err := os.ReadFile(filepath.Join(sessionDir, "prompt.md"))
		if err != nil {
			fail(err)
		}
		submit := func(event string) {
			if os.Getenv("FAKE_REVIEW_NO_SUBMIT") == "1" {
				return
			}
			body := event + ": the findings of the review\n\n" + findingsOf(string(prompt)) + "\n\n<!-- bees:reviewer -->"
			c := github.New(os.Getenv(session.EnvRepo))
			c.ExecStdin = func(_ context.Context, stdin string, args ...string) ([]byte, error) {
				rec, err := json.Marshal(map[string]any{"args": args, "stdin": stdin, "issue": os.Getenv(session.EnvIssue)})
				if err != nil {
					return nil, err
				}
				return nil, os.WriteFile(filepath.Join(sessionDir, "review.json"), rec, 0o644)
			}
			if err := c.SubmitReview(context.Background(), pr, event, body); err != nil {
				fail(err)
			}
			if err := requestGHEdit(stateDir, ghEdit{Number: pr, Review: reviewStates[event]}); err != nil {
				fail(err)
			}
		}
		// No issue: a review a person asked for with bees:review-requested.
		// There is no developer to mail, so the review is the verdict: the
		// fake reads off its task whether the factory is the pull request's
		// author (the task states the comparison; the fake never re-derives
		// it) and submits approve, or comment when the factory is the
		// author, or request-changes under FAKE_REVIEW_ALWAYS_CHANGES.
		if issue == 0 {
			counter("review")
			event, status := "approve", OutcomeApproved
			switch {
			case os.Getenv("FAKE_REVIEW_ALWAYS_CHANGES") == "1":
				event, status = "request-changes", OutcomeChangesRequested
			case strings.Contains(string(prompt), "That is this pull request's author"):
				event = "comment"
			}
			// FAKE_REVIEW_MISMATCH submits the other verdict and reports this
			// one: a session whose status does not match what it did.
			if os.Getenv("FAKE_REVIEW_MISMATCH") == "1" {
				if event == "request-changes" {
					event = "approve"
				} else {
					event = "request-changes"
				}
			}
			submit(event)
			outcome = session.Outcome{Status: status, Note: "one review submitted"}
			break
		}
		// A developer's pull request: the findings go on it as a comment
		// review, and the verdict to the developer by mail and to the
		// orchestrator as the outcome.
		submit("comment")
		// FAKE_REVIEW_ALWAYS_CHANGES never approves, which is the only way
		// to reach the "not approved after N review rounds" escalation.
		if os.Getenv("FAKE_REVIEW_ALWAYS_CHANGES") == "1" {
			round := counter("review")
			if _, err := box.Send(mail.Message{From: role, To: config.RoleDeveloper, Subject: fmt.Sprintf("Review round %d", round), Body: "still not right", Work: ghwork.New(issue, pr)}); err != nil {
				fail(err)
			}
			outcome = session.Outcome{Status: OutcomeChangesRequested}
			break
		}
		if counter("review") == 1 {
			if _, err := box.Send(mail.Message{From: role, To: config.RoleDeveloper, Subject: "Review round 1", Body: "please add tests", Work: ghwork.New(issue, pr)}); err != nil {
				fail(err)
			}
			outcome = session.Outcome{Status: OutcomeChangesRequested}
		} else {
			outcome = session.Outcome{Status: OutcomeApproved, Note: "lgtm"}
		}
	default:
		counter(role)
		// FAKE_TRIAGE=<n> is the project manager moving a work item out of
		// triage with issue_set_state; FAKE_FILE_ISSUE=<n> is a manager
		// filing one with issue_create. Each is two writes, exactly as it is
		// in a real session: the change on GitHub, and the note the MCP
		// server leaves behind so the scheduler can refresh its cache when
		// the session ends (written with the function the server calls). The
		// change travels through the state directory because this process
		// cannot reach the harness's in-process fake gh.
		if v := os.Getenv("FAKE_TRIAGE"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				fail(err)
			}
			if err := requestGHEdit(stateDir, ghEdit{Number: n, Add: []string{"bees:ready"}, Remove: []string{"bees:triage"}}); err != nil {
				fail(err)
			}
			if err := session.RecordTouched(sessionDir, n); err != nil {
				fail(err)
			}
		}
		if v := os.Getenv("FAKE_FILE_ISSUE"); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil {
				fail(err)
			}
			if err := requestGHEdit(stateDir, ghEdit{Number: n, Create: true, Title: "Filed by the " + role,
				Add: []string{"bees", "bees:triage"}}); err != nil {
				fail(err)
			}
			if err := session.RecordTouched(sessionDir, n); err != nil {
				fail(err)
			}
		}
		// QA's prompt requires one report to the product manager every
		// session, and the scheduler checks for it when the session ends: a
		// QA session that reports `done` without sending it is a claim
		// nothing backs, which is what FAKE_QA_NO_REPORT produces.
		if role == config.RoleQA && os.Getenv("FAKE_QA_NO_REPORT") != "1" {
			if _, err := box.Send(mail.Message{From: role, To: config.RoleProductManager, Subject: "QA report", Body: "tested the merged pull requests; nothing broken"}); err != nil {
				fail(err)
			}
		}
		// FAKE_RELEASE is what a release manager session does with the
		// milestone its task names: "ship:<number>" closes it, the way
		// release_ship ends; "escalate:<issue>:<milestone title>" files the
		// needs-human issue for a refused tag in that milestone. Unset, the
		// session does nothing and still reports done.
		if v := os.Getenv("FAKE_RELEASE"); role == config.RoleReleaseManager && v != "" {
			parts := strings.Split(v, ":")
			n, err := strconv.Atoi(parts[1])
			if err != nil {
				fail(err)
			}
			edit := ghEdit{CloseMilestone: n}
			if parts[0] == "escalate" {
				edit = ghEdit{Number: n, Create: true, Title: "Release " + parts[2] + " needs a person", Milestone: parts[2],
					Add: []string{"bees", "bees:triage", "bees:needs-human"}}
			}
			if err := requestGHEdit(stateDir, edit); err != nil {
				fail(err)
			}
			if parts[0] == "escalate" {
				if err := session.RecordTouched(sessionDir, n); err != nil {
					fail(err)
				}
			}
		}
		// FAKE_QA_OTHER_MAIL writes to the product manager as another role
		// while QA runs, the way a project manager session running alongside
		// it does: mail QA's report cannot be confused with.
		if role == config.RoleQA && os.Getenv("FAKE_QA_OTHER_MAIL") == "1" {
			if _, err := box.Send(mail.Message{From: config.RoleProjectManager, To: config.RoleProductManager, Subject: "A question about #4", Body: "which milestone?"}); err != nil {
				fail(err)
			}
		}
		outcome = session.Outcome{Status: OutcomeDone, Note: "ok"}
	}
	if err := session.WriteOutcome(sessionDir, outcome); err != nil {
		fail(err)
	}
	// FAKE_COST makes a session's cost controllable, which is what the cost
	// budget tests spend against.
	cost := 0.01
	if v := os.Getenv("FAKE_COST"); v != "" {
		c, err := strconv.ParseFloat(v, 64)
		if err != nil {
			fail(err)
		}
		cost = c
	}
	// FAKE_RESULT_TEXT is what the session says it did. A bee whose work is
	// the account's own limit writes the words "session limit" here.
	text := "ok"
	if v := os.Getenv("FAKE_RESULT_TEXT"); v != "" {
		text = v
	}
	if codex {
		// Two completed items are the two turns claude's result reports,
		// so a test's turn count holds whichever agent ran; there is no
		// cost to report.
		fmt.Println(`{"type":"thread.started","thread_id":"fake-thread"}`)
		fmt.Println(`{"type":"item.completed","item":{"id":"item_0","type":"mcp_tool_call","server":"bees","tool":"done","status":"completed"}}`)
		fmt.Printf(`{"type":"item.completed","item":{"id":"item_1","type":"agent_message","text":%q}}`+"\n", text)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":0,"output_tokens":2}}`)
		return
	}
	if pi {
		// Two ended turns are the two turns, the first a response that
		// called a tool and the second one that stopped; each response
		// carries half the cost, which pi reports per response.
		fmt.Printf(`{"type":"session","version":3,"id":%q,"cwd":"."}`+"\n", sessionID)
		fmt.Println(`{"type":"agent_start"}`)
		fmt.Printf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"toolCall","id":"c1","name":"bees_done","arguments":{}}],"stopReason":"toolUse","usage":{"cost":{"total":%v}}}}`+"\n", cost/2)
		fmt.Println(`{"type":"message_end","message":{"role":"toolResult","toolCallId":"c1","toolName":"bees_done","content":[{"type":"text","text":"ok"}],"isError":false}}`)
		fmt.Println(`{"type":"turn_end","toolResults":[]}`)
		fmt.Printf(`{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":%q}],"stopReason":"stop","usage":{"cost":{"total":%v}}}}`+"\n", text, cost/2)
		fmt.Println(`{"type":"turn_end","toolResults":[]}`)
		fmt.Println(`{"type":"agent_end","messages":[]}`)
		return
	}
	if opencode {
		// Two finished steps are the two turns, the first ended by a tool
		// call and the second by the model stopping; each carries half
		// the cost, which opencode reports per step.
		fmt.Printf(`{"type":"step_start","timestamp":1,"sessionID":%q,"part":{"type":"step-start"}}`+"\n", sessionID)
		fmt.Printf(`{"type":"tool_use","timestamp":2,"sessionID":%q,"part":{"type":"tool","tool":"bees_done","state":{"status":"completed"}}}`+"\n", sessionID)
		fmt.Printf(`{"type":"step_finish","timestamp":3,"sessionID":%q,"part":{"type":"step-finish","reason":"tool-calls","cost":%v,"tokens":{"input":10,"output":2}}}`+"\n", sessionID, cost/2)
		fmt.Printf(`{"type":"text","timestamp":4,"sessionID":%q,"part":{"type":"text","text":%q}}`+"\n", sessionID, text)
		fmt.Printf(`{"type":"step_finish","timestamp":5,"sessionID":%q,"part":{"type":"step-finish","reason":"stop","cost":%v,"tokens":{"input":10,"output":2}}}`+"\n", sessionID, cost/2)
		return
	}
	fmt.Printf(`{"type":"result","subtype":"success","is_error":false,"result":%q,"session_id":%q,"num_turns":2,"total_cost_usd":%v}`+"\n", text, sessionID, cost)
}

// fakeGH is the in-memory GitHub backing the gh wrapper (internal/fakegh)
// plus what only these tests use: pull requests that stay hidden until a
// developer session opened them.
type fakeGH struct {
	*fakegh.GitHub
	// prMarker is the file a developer session leaves when it opened
	// fakePR, and the prefix of the one it leaves for a hidden PR.
	prMarker string
	// hidden lists PRs that do not exist until a developer session opened
	// one on their head branch (bees/issue-N), like fakePR.
	hidden map[int]bool
}

// visible hides fakePR until a developer session opened it, and a hidden PR
// until one opened it on its head branch. Called with the lock held.
func (f *fakeGH) visible(p *github.PR) bool {
	if p.Number == fakePR {
		_, err := os.Stat(f.prMarker)
		return err == nil
	}
	if f.hidden[p.Number] {
		issue := strings.TrimPrefix(p.HeadRefName, "bees/issue-")
		_, err := os.Stat(f.prMarker + "-issue-" + issue)
		return err == nil
	}
	return true
}

// implicitParent makes issue 1 a sub-issue of feature 5 while that feature
// exists, which is what most tests want. Called with the lock held.
func (f *fakeGH) implicitParent(n int) (int, string) {
	if n == 1 && f.Issues[5] != nil {
		return 5, "Exports"
	}
	return 0, ""
}

type (
	ghEdit         = fakegh.Edit
	checksResponse = fakegh.ChecksResponse
)

// reviewStates maps a gh review event to the state GitHub records for it.
var reviewStates = fakegh.ReviewStates

// requestGHEdit records one edit for the fake GitHub to apply: the fake
// claude runs in its own process, exactly as `bees mcp serve` does, so a
// session-side `gh` write cannot reach the harness's in-memory fake and
// travels through the state directory instead.
func requestGHEdit(stateDir string, e ghEdit) error {
	return fakegh.RequestEdit(filepath.Join(stateDir, "gh-edits"), e)
}

// fakeClock is a settable clock for tests that drive the scheduler loop.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type harness struct {
	t     *testing.T
	cfg   *config.Config
	gh    *fakeGH
	store *state.Store
	box   *mail.Box
	sched *Scheduler
	clone string
	logs  *syncBuffer
	clock *fakeClock // nil unless the harness was built with a fixed clock
}

// syncBuffer collects log output; workers log concurrently.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func newHarness(t *testing.T, toml string) *harness {
	t.Helper()
	return newHarnessAt(t, toml, time.Time{})
}

// newHarnessAt builds a harness whose scheduler reads the clock from
// harness.clock, starting at now. A zero now uses the real clock. Each opt
// is given the Deps the scheduler is built from, for a test that replaces
// one of them.
func newHarnessAt(t *testing.T, toml string, now time.Time, opts ...func(*Deps)) *harness {
	t.Helper()
	_, clone := testutil.SetupRepos(t)
	cfgPath := filepath.Join(clone, "bees.toml")
	if err := os.WriteFile(cfgPath, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	// Like `bees init`: keep the config and state dir out of git.
	if err := os.WriteFile(filepath.Join(clone, ".git", "info", "exclude"), []byte("/bees.toml\n/.bees/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-review checks read runs before every first review, so without
	// this every test would sit through the 1m default wait. Tests that care
	// about the timings set their own and keep them.
	if rs := cfg.Roles[config.RoleReviewer]; rs.ChecksWait.Duration == 0 || rs.ChecksPollInterval.Duration == 0 {
		if rs.ChecksWait.Duration == 0 {
			rs.ChecksWait = config.Duration{Duration: time.Millisecond}
		}
		if rs.ChecksPollInterval.Duration == 0 {
			rs.ChecksPollInterval = config.Duration{Duration: 10 * time.Millisecond}
		}
		cfg.Roles[config.RoleReviewer] = rs
	}
	// default_branch is derived from the (local) origin remote's HEAD.
	if err := cfg.Resolve(context.Background()); err != nil {
		t.Fatal(err)
	}
	if cfg.Project.DefaultBranch != "main" {
		t.Fatalf("default branch not derived: %q", cfg.Project.DefaultBranch)
	}
	store := state.New(cfg.StateDir())
	if err := store.Init(); err != nil {
		t.Fatal(err)
	}
	gh := &fakeGH{
		GitHub:   fakegh.New(cfg.Project.Repo),
		prMarker: filepath.Join(store.Dir, "fake-pr-created"),
		hidden:   map[int]bool{},
	}
	gh.Login = "kyle"
	gh.Diff = fakeDiff
	gh.EditsDir = filepath.Join(store.Dir, "gh-edits")
	gh.DefaultSubIssues = github.SubIssueSummary{Total: 3, Completed: 1}
	gh.Visible = gh.visible
	gh.ImplicitParent = gh.implicitParent
	// Like a repository `bees init` has just set up: every label exists.
	for _, l := range cfg.Labels().All() {
		gh.Labels = append(gh.Labels, l.Name)
	}
	client := github.New(cfg.Project.Repo)
	client.Exec = gh.Exec

	t.Setenv("FAKE_CLAUDE", "1")
	// Log through the real logging package so tests see the summary lines a
	// terminal would; dump it when the test fails.
	logs := &syncBuffer{}
	t.Cleanup(func() {
		if t.Failed() {
			t.Log("scheduler log:\n" + logs.String())
		}
	})
	logger := logging.New(logging.Options{Format: logging.FormatText, Console: logs})
	runner := &session.Runner{
		ClaudeBin:   os.Args[0],
		CodexBin:    os.Args[0],
		OpenCodeBin: os.Args[0],
		PiBin:       os.Args[0],
		SessionsDir: store.SessionsDir(),
		StateDir:    store.Dir,
		Repo:        cfg.Project.Repo,
		Label:       cfg.Filter.Label,
		Logger:      logger.Logger,
	}
	ws := workspace.NewManager(clone, filepath.Join(t.TempDir(), "ws"))
	box := mail.Open(store.MailDir())
	deps := Deps{Config: cfg, GitHub: client, Mail: box, Runner: runner, Workspaces: ws, Store: store, Logger: runner.Logger}
	var clock *fakeClock
	if !now.IsZero() {
		clock = &fakeClock{t: now}
		deps.Now = clock.now
	}
	for _, opt := range opts {
		opt(&deps)
	}
	sched, err := New(deps)
	if err != nil {
		t.Fatal(err)
	}
	sched.Once = true
	return &harness{t: t, cfg: cfg, gh: gh, store: store, box: box, sched: sched, clone: clone, logs: logs, clock: clock}
}

// stateOfIssue is the workflow state label the fake GitHub now carries for an
// issue, which is what a restarted scheduler would read.
func (h *harness) stateOfIssue(n int) string {
	h.gh.Lock()
	defer h.gh.Unlock()
	return h.sched.stateOf(h.gh.Issues[n].Labels)
}

func (h *harness) sessions(role string) []string {
	entries, _ := os.ReadDir(h.store.SessionsDir())
	var out []string
	for _, e := range entries {
		if strings.Contains(e.Name(), "-"+role+"-") {
			out = append(out, filepath.Join(h.store.SessionsDir(), e.Name()))
		}
	}
	sort.Strings(out)
	return out
}

// sessionOrder lists the session directories in the order the sessions ran.
func (h *harness) sessionOrder() []string {
	b, err := os.ReadFile(filepath.Join(h.store.Dir, "fake-order"))
	if err != nil {
		return nil
	}
	return strings.Fields(strings.TrimSpace(string(b)))
}

// sessionNames is sessionOrder with the "<date>-<time>-" prefix and the random
// MkdirTemp suffix stripped, so "reviewer-pr-101-r2" is the whole name and not
// a prefix of "reviewer-pr-101-r2-checkfix1".
func (h *harness) sessionNames() []string {
	dirs := h.sessionOrder()
	names := make([]string, len(dirs))
	for i, d := range dirs {
		parts := strings.Split(d, "-")
		if len(parts) > 3 {
			parts = parts[2 : len(parts)-1]
		}
		names[i] = strings.Join(parts, "-")
	}
	return names
}

// wantOrder asserts the sessions ran in this order, by exact session name.
func (h *harness) wantOrder(want ...string) {
	h.t.Helper()
	got := h.sessionNames()
	if len(got) != len(want) {
		h.t.Fatalf("session order: %v\nwant     %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			h.t.Fatalf("session %d is %q, want %q\nfull order: %v", i, got[i], want[i], got)
		}
	}
}

// sessionFlag reads the value the runner passed for a flag in the newest
// session of a role, from the args.txt the fake claude wrote.
func (h *harness) sessionFlag(t *testing.T, role, flag string) string {
	t.Helper()
	dirs := h.sessions(role)
	if len(dirs) == 0 {
		t.Fatalf("no %s session ran", role)
	}
	b, err := os.ReadFile(filepath.Join(dirs[len(dirs)-1], "args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Split(string(b), "\n")
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("%s session has no %s in %v", role, flag, args)
	return ""
}

const baseTOML = `
version = 1
[project]
repo = "acme/widgets"
[scheduler]
poll_interval = "1s"
max_developers = 2
max_review_rounds = 3
`

func TestFullDeveloperReviewLoop(t *testing.T) {
	h := newHarness(t, baseTOML)
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.Issues[2] = &github.Issue{Number: 2, Title: "Human filed this", Body: "hi", State: "OPEN", Labels: []github.Label{{Name: "bees"}}, CreatedAt: time.Now()}
	h.gh.Issues[3] = &github.Issue{Number: 3, Title: "Spec me", Body: "vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// Issue 1 walked the whole loop: in-progress -> review -> in-progress -> review -> approved.
	want := []string{"bees:in-progress", "bees:review", "bees:in-progress", "bees:review", "bees:approved"}
	if got := h.gh.History[1]; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("issue 1 label history: %v", got)
	}
	if !github.HasLabel(h.gh.PRs[fakePR].Labels, "bees:approved") {
		t.Fatalf("PR labels: %v", h.gh.PRs[fakePR].Labels)
	}
	if len(h.gh.Merged) != 0 {
		t.Fatal("auto_merge is off; nothing should be merged")
	}
	if len(h.gh.Comments[1]) != 0 {
		t.Fatalf("no escalation expected: %v", h.gh.Comments[1])
	}
	// Issue 2 had no state label and neither bees:feature nor bees:feedback:
	// it is an idea a person handed the factory, so it goes to the product
	// manager as feedback.
	if got := h.gh.History[2]; strings.Join(got, ",") != "bees:feedback" {
		t.Fatalf("issue 2 label history: %v", got)
	}
	// Sessions: 2 developer, 2 reviewer, and each singleton once.
	for role, n := range map[string]int{config.RoleDeveloper: 2, config.RoleReviewer: 2, config.RoleProjectManager: 1, config.RoleProductManager: 1, config.RoleQA: 1} {
		if got := len(h.sessions(role)); got != n {
			t.Errorf("%s sessions: got %d want %d", role, got, n)
		}
	}
	// Report every role's count above, then stop: the assertions below index
	// these session slices, so a missing session would panic instead of failing.
	if t.Failed() {
		t.FailNow()
	}
	// The second developer session received the reviewer's mail.
	dev := h.sessions(config.RoleDeveloper)
	prompt, _ := os.ReadFile(filepath.Join(dev[1], "prompt.md"))
	if !strings.Contains(string(prompt), "please add tests") || !strings.Contains(string(prompt), "review round 2 of 3") {
		t.Fatalf("second developer prompt:\n%s", prompt)
	}
	// The console got one summary line per session, in the order they ended.
	wantSummaries := []string{
		"✓ developer issue #1 → PR #101 opened",
		"✗ reviewer PR #101 changes requested",
		"✓ developer issue #1 → PR #101 updated",
		"✓ reviewer PR #101 approved",
	}
	rest := h.logs.String()
	for _, line := range wantSummaries {
		_, after, found := strings.Cut(rest, line)
		if !found {
			t.Fatalf("missing summary %q (or out of order) in:\n%s", line, h.logs.String())
		}
		rest = after
	}

	// The project manager saw issue 3 in its triage list, and issue 2 — the
	// idea reconcile relabelled — reached the product manager instead.
	pjm := h.sessions(config.RoleProjectManager)
	prompt, _ = os.ReadFile(filepath.Join(pjm[0], "prompt.md"))
	if !strings.Contains(string(prompt), "#3: Spec me") {
		t.Fatalf("project manager prompt:\n%s", prompt)
	}
	pdm := h.sessions(config.RoleProductManager)
	prompt, _ = os.ReadFile(filepath.Join(pdm[0], "prompt.md"))
	if !strings.Contains(string(prompt), "#2: Human filed this") {
		t.Fatalf("product manager prompt:\n%s", prompt)
	}
	// Mail was delivered (marked read) and bookkeeping recorded round 2. The
	// QA session's report to the product manager is the exception: QA ran
	// after it, and the next product manager session is what reads it.
	unread, _ := h.box.List(mail.Filter{UnreadOnly: true})
	if len(unread) != 1 || unread[0].From != config.RoleQA || unread[0].To != config.RoleProductManager {
		t.Fatalf("unread mail left: %+v", unread)
	}
	if is, _ := h.store.Issue(1); is.Round != 2 || ghwork.PR(is.Work) != fakePR {
		t.Fatalf("bookkeeping: %+v", is)
	}
	// Both commits reached origin on the developer branch and worktrees are gone.
	log, err := workspace.Git(ctx, h.clone, "log", "--oneline", "origin/bees/issue-1")
	if err != nil || strings.Count(log, "\n") != 2 {
		t.Fatalf("origin branch log: %q %v", log, err)
	}
	if out, _ := workspace.Git(ctx, h.clone, "worktree", "list"); strings.Count(out, "\n") != 0 {
		t.Fatalf("worktrees left behind:\n%s", out)
	}
	// Status file reflects an idle factory.
	st, _ := h.store.LoadStatus()
	if len(st.Workers) != 0 || st.Singletons[config.RoleQA] != "idle" {
		t.Fatalf("status: %+v", st)
	}
	for _, r := range []string{config.RoleProductManager, config.RoleProjectManager, config.RoleQA} {
		if rs, _ := h.store.Role(r); rs.LastRun.IsZero() {
			t.Errorf("%s last run not recorded", r)
		}
	}
	// Every session is in the ledger, with what it cost and what it did,
	// and so is the first review round's pipeline: the brief and the two
	// angles of an `s` change, entered once under the round's name with what
	// the fake CLI reported they cost ($0.25 each). The second round verifies
	// and runs no pipeline.
	ledger, err := h.store.ReadLedger(time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(ledger) != 8 {
		t.Fatalf("ledger has %d entries, want one per session (7) and one review (1):\n%+v", len(ledger), ledger)
	}
	byRole := map[string][]state.LedgerEntry{}
	reviews := 0
	for _, e := range ledger {
		if e.Outcome == "reviewed" {
			reviews++
			if e.Role != config.RoleReviewer || ghwork.Issue(e.Work) != 1 || ghwork.PR(e.Work) != fakePR || e.Turns != 2 || e.CostUSD != 0.75 || !strings.HasPrefix(e.Session, "reviewer-pr-101-r") {
				t.Errorf("review ledger entry not filled in: %+v", e)
			}
			continue
		}
		if e.Turns != 2 || e.CostUSD != 0.01 || e.Session == "" || e.Time.IsZero() {
			t.Errorf("ledger entry not filled in: %+v", e)
		}
		// DurationMS is whole milliseconds, so a session that spawned and exited
		// in under 1ms records 0. That is a legitimate duration, not a missing
		// field: never assert it is positive.
		if e.DurationMS < 0 {
			t.Errorf("ledger entry has a negative duration: %+v", e)
		}
		byRole[e.Role] = append(byRole[e.Role], e)
	}
	for role, n := range map[string]int{config.RoleDeveloper: 2, config.RoleReviewer: 2, config.RoleProjectManager: 1, config.RoleProductManager: 1, config.RoleQA: 1} {
		if got := len(byRole[role]); got != n {
			t.Errorf("%s ledger entries: got %d want %d", role, got, n)
		}
	}
	// Report every role's count above, then stop: the assertions below index
	// these ledger buckets, so a missing entry would panic instead of failing.
	if t.Failed() {
		t.FailNow()
	}
	for i, want := range []string{OutcomePROpened, OutcomePRUpdated} {
		got := byRole[config.RoleDeveloper][i]
		if got.Outcome != want || ghwork.Issue(got.Work) != 1 || ghwork.PR(got.Work) != fakePR {
			t.Errorf("developer ledger entry %d: %+v", i, got)
		}
	}
	for i, want := range []string{OutcomeChangesRequested, OutcomeApproved} {
		got := byRole[config.RoleReviewer][i]
		if got.Outcome != want || ghwork.PR(got.Work) != fakePR {
			t.Errorf("reviewer ledger entry %d: %+v", i, got)
		}
	}
	if e := byRole[config.RoleQA][0]; ghwork.Issue(e.Work) != 0 || ghwork.PR(e.Work) != 0 {
		t.Errorf("qa ledger entry should have no issue or PR: %+v", e)
	}
}

func TestQuestionBlocksAndAnswerUnblocks(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n")
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:blocked"}}}
	// The developer asked earlier; now the project manager's answer is waiting.
	if _, err := h.box.Send(mail.Message{From: config.RoleProjectManager, To: config.RoleDeveloper, Subject: "Re: Vague", Body: "do X", Work: ghwork.New(1, 0)}); err != nil {
		t.Fatal(err)
	}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true} // reviewer disabled: PR auto-approved
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// The size backstop runs after the unblock, so the issue is sized in the
	// same pass that made it ready.
	want := "bees:ready,bees:size/m,bees:in-progress,bees:approved"
	if got := strings.Join(h.gh.History[1], ","); got != want {
		t.Fatalf("history: %s want %s", got, want)
	}
	dev := h.sessions(config.RoleDeveloper)
	prompt, _ := os.ReadFile(filepath.Join(dev[0], "prompt.md"))
	if !strings.Contains(string(prompt), "do X") {
		t.Fatalf("answer not delivered:\n%s", prompt)
	}
}

func TestEscalationWhenNoPR(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "x", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}
	// No PR 101 registered: the developer claims pr-opened but nothing exists.
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.History[1], ","); got != "bees:in-progress,bees:needs-human" {
		t.Fatalf("history: %s", got)
	}
	if len(h.gh.Comments[1]) != 1 || !strings.Contains(h.gh.Comments[1][0], "needs a human") {
		t.Fatalf("comments: %v", h.gh.Comments[1])
	}
}

func TestHumanFeedbackReopensApprovedPR(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	created := time.Now().Add(-time.Hour)
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Done already", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:approved"}, {Name: "bees:size/s"}}, CreatedAt: created}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:approved"}}, CreatedAt: created, UpdatedAt: time.Now(),
		Body: "Closes #1"}
	// The PR "exists" from the start for this test.
	if err := os.WriteFile(h.gh.prMarker, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	// Seed the branch on origin so the worker can reuse it.
	if err := os.WriteFile(filepath.Join(h.clone, "seed.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"checkout", "-q", "-b", "bees/issue-1"}, {"add", "."}, {"commit", "-q", "-m", "seed"}, {"push", "-q", "-u", "origin", "bees/issue-1"}, {"checkout", "-q", "main"}} {
		if _, err := workspace.Git(context.Background(), h.clone, args...); err != nil {
			t.Fatal(err)
		}
	}
	now := time.Now().UTC().Format(time.RFC3339)
	h.gh.Activity["repos/acme/widgets/pulls/101/comments"] = fmt.Sprintf(`[
		{"id": 555, "user": {"login": "kyle"}, "body": "please rename this", "path": "seed.txt", "line": 1, "html_url": "https://x/555", "created_at": %q},
		{"id": 556, "user": {"login": "kyle"}, "body": "will do\n\n<!-- bees:developer -->", "path": "seed.txt", "line": 1, "html_url": "https://x/556", "created_at": %q},
		{"id": 557, "user": {"login": "kyle"}, "body": "Replying to the bot:\n> <!-- bees:developer -->\n\nActually, hold off on merging.", "path": "seed.txt", "line": 1, "html_url": "https://x/557", "created_at": %q}
	]`, now, now, now)
	h.gh.Activity["repos/acme/widgets/pulls/101/reviews"] = fmt.Sprintf(`[
		{"id": 777, "user": {"login": "kyle"}, "body": "", "state": "APPROVED", "html_url": "https://x/777", "submitted_at": %q}
	]`, now)

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// approved -> ready (human feedback) -> in-progress -> review -> ... -> approved
	hist := strings.Join(h.gh.History[1], ",")
	if !strings.HasPrefix(hist, "bees:ready,bees:in-progress,bees:review") || !strings.HasSuffix(hist, "bees:approved") {
		t.Fatalf("history: %s", hist)
	}
	dev := h.sessions(config.RoleDeveloper)
	if len(dev) == 0 {
		t.Fatal("no developer session ran")
	}
	prompt, _ := os.ReadFile(filepath.Join(dev[0], "prompt.md"))
	for _, want := range []string{"Feedback on PR #101 from kyle", "please rename this", "pulls/101/comments/555/replies", "seed.txt:1", "Actually, hold off on merging."} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("developer prompt missing %q", want)
		}
	}
	for _, unwanted := range []string{"will do", "APPROVED"} {
		if strings.Contains(string(prompt), unwanted) {
			t.Errorf("developer prompt should not contain %q (bee comment / empty approval)", unwanted)
		}
	}
	if bk, _ := h.store.Issue(1); bk.HumanSeenAt.IsZero() {
		t.Fatal("HumanSeenAt not recorded")
	}
}

func TestLabelBackstop(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[filter]\nassignee = \"kyle\"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n")
	// A triage issue so the project manager runs; and an issue "created by a
	// bee" with a kind label but no base label and no assignee.
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "triage me", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}}, Assignees: []github.Author{{Login: "kyle"}}, CreatedAt: time.Now().Add(-time.Hour)}
	h.gh.Issues[7] = &github.Issue{Number: 7, Title: "bug from a bee", State: "OPEN", Labels: []github.Label{{Name: "bees:bug"}, {Name: "bees:triage"}}, CreatedAt: time.Now()}
	h.gh.Issues[8] = &github.Issue{Number: 8, Title: "unrelated", State: "OPEN", Labels: nil, CreatedAt: time.Now()}
	// A pull request opened outside a developer worker, with a factory label
	// but neither the base label nor the assignee: unassigned it would be
	// invisible to a factory filtering on one.
	h.gh.PRs[9] = &github.PR{Number: 9, Title: "pr from a bee", State: "OPEN", HeadRefName: "bees/issue-7", BaseRefName: "main",
		Labels: []github.Label{{Name: "bees:review"}}, CreatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.History[7], ","); got != "bees,assignee:kyle" {
		t.Fatalf("issue 7 history: %s", got)
	}
	if len(h.gh.History[8]) != 0 {
		t.Fatalf("unrelated issue touched: %v", h.gh.History[8])
	}
	if got := strings.Join(h.gh.History[9], ","); got != "bees,assignee:kyle" {
		t.Fatalf("PR 9 history: %s", got)
	}
	// The project manager is told the size it must not exceed when it sizes
	// a work item, so it splits anything bigger instead.
	dirs := h.sessions(config.RoleProjectManager)
	if len(dirs) == 0 {
		t.Fatal("no project manager session")
	}
	prompt, _ := os.ReadFile(filepath.Join(dirs[0], "system-prompt.md"))
	if !strings.Contains(string(prompt), "anything larger than `l` is not dispatched") {
		t.Fatalf("project manager system prompt does not carry max_size:\n%s", prompt)
	}
}

func TestAutoMergeAfterChecks(t *testing.T) {
	h := newHarness(t, baseTOML+`
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
[roles.reviewer]
auto_merge = true
merge_method = "rebase"
checks_wait = "1ms"
checks_poll_interval = "10ms"
checks_timeout = "5s"
max_check_fix_rounds = 2
# This test pins the post-approval gate; prereview_test.go owns the other one.
pre_review_checks = false
`)
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Ship it", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	pending := `[{"name":"go / test","bucket":"pending","state":"PENDING","link":"https://ci.example.com/run/1","workflow":"CI"}]`
	failing := `[{"name":"go / test","bucket":"fail","state":"FAILURE","link":"https://ci.example.com/run/1","description":"1 test failed","workflow":"CI"},{"name":"lint","bucket":"pass","state":"SUCCESS"}]`
	passing := `[{"name":"go / test","bucket":"pass","state":"SUCCESS"},{"name":"lint","bucket":"pass","state":"SUCCESS"}]`
	h.gh.Checks = []checksResponse{
		{JSON: pending, Err: fmt.Errorf("exit status 8")}, // still running: gh exits 8
		{JSON: failing, Err: fmt.Errorf("exit status 1")}, // failed: gh exits 1
		{JSON: pending, Err: fmt.Errorf("exit status 8")}, // after the fix push
		{JSON: passing, Err: nil},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.gh.Merged) != 1 || h.gh.Merged[0] != fakePR {
		t.Fatalf("merged: %v", h.gh.Merged)
	}
	if got := strings.Join(h.gh.MergeArgs[0], " "); !strings.Contains(got, "--rebase") || !strings.Contains(got, "--delete-branch") {
		t.Fatalf("merge args: %s", got)
	}
	want := "bees:in-progress,bees:review,bees:in-progress,bees:review,bees:approved,bees:in-progress"
	if got := strings.Join(h.gh.History[1], ","); got != want {
		t.Fatalf("history: %s\nwant    %s", got, want)
	}
	if len(h.gh.Comments[1]) != 0 {
		t.Fatalf("unexpected escalation: %v", h.gh.Comments[1])
	}
	// developer: initial, review fix, checks fix = 3; reviewer: 2 reviews + 1 checks diagnosis
	if n := len(h.sessions(config.RoleDeveloper)); n != 3 {
		t.Fatalf("developer sessions: %d", n)
	}
	rev := h.sessions(config.RoleReviewer)
	if len(rev) != 3 {
		t.Fatalf("reviewer sessions: %d", len(rev))
	}
	var checksPrompt string
	for _, dir := range rev {
		if strings.Contains(dir, "checks") {
			b, _ := os.ReadFile(filepath.Join(dir, "prompt.md"))
			checksPrompt = string(b)
		}
	}
	for _, want := range []string{"checks failed on pull request #101", "**go / test** (CI) — fail: 1 test failed", "https://ci.example.com/run/1", "do not assume GitHub"} {
		if !strings.Contains(checksPrompt, want) {
			t.Errorf("checks prompt missing %q", want)
		}
	}
	if strings.Contains(checksPrompt, "lint") {
		t.Error("passing checks should not be listed as failing")
	}
	dev := h.sessions(config.RoleDeveloper)
	fix, _ := os.ReadFile(filepath.Join(dev[2], "prompt.md"))
	if !strings.Contains(string(fix), "main error: TestX fails") {
		t.Fatalf("developer fix prompt missing the diagnosis:\n%s", fix)
	}
	if bk, _ := h.store.Issue(1); bk.CheckFixRounds != 1 {
		t.Fatalf("bookkeeping: %+v", bk)
	}
}

func TestChecksTimeoutEscalates(t *testing.T) {
	h := newHarness(t, baseTOML+`
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
[roles.reviewer]
auto_merge = true
checks_wait = "1ms"
checks_poll_interval = "10ms"
checks_timeout = "1ms"
pre_review_checks = false
`)
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Slow CI", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	h.gh.Checks = []checksResponse{{JSON: `[{"name":"slow","bucket":"pending","state":"PENDING"}]`, Err: fmt.Errorf("exit status 8")}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.gh.Merged) != 0 {
		t.Fatal("must not merge with pending checks")
	}
	if got := h.gh.History[1]; got[len(got)-1] != "bees:needs-human" {
		t.Fatalf("history: %v", got)
	}
	if len(h.gh.Comments[1]) != 1 || !strings.Contains(h.gh.Comments[1][0], "still pending") {
		t.Fatalf("comments: %v", h.gh.Comments[1])
	}
}

func TestFeedbackGoesToProductManager(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	now := time.Now()
	h.gh.Issues[3] = &github.Issue{Number: 3, Title: "Dark mode please", Body: "idea", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feedback"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	h.gh.Issues[4] = &github.Issue{Number: 4, Title: "Already answered", Body: "old idea", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feedback"}}, CreatedAt: now.Add(-2 * time.Hour), UpdatedAt: now,
		Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "filed #10 for this\n\n<!-- bees:product_manager -->", CreatedAt: now.Add(-time.Hour)}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	// Feedback issues never enter the workflow state machine.
	if len(h.gh.History[3]) != 0 || len(h.gh.History[4]) != 0 {
		t.Fatalf("feedback issues were relabelled: %v %v", h.gh.History[3], h.gh.History[4])
	}
	pm := h.sessions(config.RoleProductManager)
	if len(pm) != 1 {
		t.Fatalf("product manager sessions: %d", len(pm))
	}
	prompt, _ := os.ReadFile(filepath.Join(pm[0], "prompt.md"))
	if !strings.Contains(string(prompt), "#3: Dark mode please") {
		t.Fatalf("fresh feedback missing from prompt:\n%s", prompt)
	}
	if strings.Contains(string(prompt), "#4: Already answered") {
		t.Fatal("feedback already answered by the product manager should not be re-presented")
	}
	// After the run, nothing is fresh until a human comments again.
	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if h.sched.productManagerHasWork(ctx, snap) {
		t.Fatal("product manager should be idle: no fresh feedback, interval not elapsed")
	}
	h.gh.Issues[3].Comments = []github.Comment{
		{Author: github.Author{Login: "kyle"}, Body: "created #11\n\n<!-- bees:product_manager -->", CreatedAt: now.Add(time.Second)},
		{Author: github.Author{Login: "kyle"}, Body: "also on mobile please", CreatedAt: now.Add(2 * time.Second)},
	}
	h.gh.Issues[3].UpdatedAt = now.Add(2 * time.Second)
	snap, _ = h.sched.poll(ctx)
	if !h.sched.productManagerHasWork(ctx, snap) {
		t.Fatal("a new human comment on feedback should wake the product manager")
	}
}

func TestFeatureIssuesBelongToProductManager(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	now := time.Now()
	// A feature filed by a person, never touched by the PM: fresh.
	h.gh.Issues[5] = &github.Issue{Number: 5, Title: "Exports", Body: "csv please", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	// A feature where the PM asked a question and the person just answered.
	h.gh.Issues[6] = &github.Issue{Number: 6, Title: "Search", Body: "find things", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}, {Name: "bees:question"}}, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now,
		Comments: []github.Comment{
			{Author: github.Author{Login: "kyle"}, Body: "Fuzzy or exact?\n\n<!-- bees:product_manager -->", CreatedAt: now.Add(-2 * time.Hour)},
			{Author: github.Author{Login: "kyle"}, Body: "fuzzy", CreatedAt: now.Add(-time.Minute)},
		}}
	// A feature already broken down: the PM commented last.
	h.gh.Issues[7] = &github.Issue{Number: 7, Title: "Done planning", Body: "x", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}}, CreatedAt: now.Add(-3 * time.Hour), UpdatedAt: now,
		Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "work items: #8 #9\n\n<!-- bees:product_manager -->", CreatedAt: now.Add(-time.Hour)}}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{5, 6, 7} {
		for _, l := range h.gh.History[n] {
			if strings.HasPrefix(l, "bees:") && l != "bees:question" {
				t.Fatalf("feature issue #%d was put in the state machine: %v", n, h.gh.History[n])
			}
		}
	}
	if github.HasLabel(h.gh.Issues[6].Labels, "bees:question") {
		t.Fatal("answered question should have lost the question label")
	}
	pm := h.sessions(config.RoleProductManager)
	if len(pm) != 1 {
		t.Fatalf("product manager sessions: %d", len(pm))
	}
	prompt, _ := os.ReadFile(filepath.Join(pm[0], "prompt.md"))
	for _, want := range []string{"#5: Exports", "#6: Search", "fuzzy", "| 7 | - | 1/3 done | - | - | Done planning |"} {
		if !strings.Contains(string(prompt), want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if strings.Contains(string(prompt), "#7: Done planning") {
		t.Error("already broken-down feature should only appear in the summary table")
	}
	if strings.Contains(string(prompt), "| 5 | - |") && strings.Contains(string(prompt), "## Open work items (3)") {
		t.Error("feature issues must not be listed as work items")
	}
}

// A session killed by its timeout is an infrastructure failure: the worker
// runs it again instead of escalating.
func TestInfrastructureFailureIsRetried(t *testing.T) {
	t.Setenv("FAKE_DEV_HANG", "1")
	h := newHarness(t, baseTOML+`
retries = 1
retry_delay = "0s"
[roles.developer]
timeout = "5s"
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
`)
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	h.sched.OnlyRoles = map[string]bool{config.RoleDeveloper: true} // reviewer disabled: PR auto-approved

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.History[1], ","); got != "bees:in-progress,bees:approved" {
		t.Fatalf("history: %s", got)
	}
	if len(h.gh.Comments[1]) != 0 {
		t.Fatalf("no escalation expected: %v", h.gh.Comments[1])
	}
	dev := h.sessions(config.RoleDeveloper)
	if len(dev) != 2 {
		t.Fatalf("developer sessions: %v", dev)
	}
	// The retry keeps its own transcript directory.
	if !strings.Contains(filepath.Base(dev[1]), "-retry1-") {
		t.Errorf("retry session directory not named as a retry: %s", dev[1])
	}
	first, _ := os.ReadFile(filepath.Join(dev[0], "prompt.md"))
	if strings.Contains(string(first), "previous attempt was interrupted") {
		t.Errorf("first attempt got the retry preamble:\n%s", first)
	}
	retry, _ := os.ReadFile(filepath.Join(dev[1], "prompt.md"))
	if !strings.Contains(string(retry), "previous attempt was interrupted") {
		t.Errorf("retry is missing the interrupted-work preamble:\n%s", retry)
	}
}

// A session that ran and reported `failed` made a decision: it escalates at
// once, however many retries are configured.
func TestReportedFailureEscalatesWithoutRetry(t *testing.T) {
	t.Setenv("FAKE_DEV_FAIL", "1")
	h := newHarness(t, baseTOML+`
retries = 2
retry_delay = "0s"
[roles.product_manager]
enabled = false
[roles.qa]
enabled = false
[roles.project_manager]
enabled = false
`)
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Build the thing", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(h.gh.History[1], ","); got != "bees:in-progress,bees:needs-human" {
		t.Fatalf("history: %s", got)
	}
	if got := len(h.sessions(config.RoleDeveloper)); got != 1 {
		t.Fatalf("developer sessions: got %d want 1", got)
	}
	if len(h.gh.Comments[1]) != 1 || !strings.Contains(h.gh.Comments[1][0], "ended with `failed`") {
		t.Fatalf("comments: %v", h.gh.Comments[1])
	}
}

// workHoursTOML is baseTOML with a mon-fri 09:00-18:00 window in UTC and
// every role disabled, so only the polling loop itself is exercised.
const workHoursTOML = baseTOML + `
off_hours_poll_interval = "1h"
work_hours = "09:00-18:00"
work_days = ["mon", "tue", "wed", "thu", "fri"]
timezone = "UTC"
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

func TestOffHoursPollingIsThrottled(t *testing.T) {
	// 2026-08-29 12:00 UTC is a Saturday: outside the window.
	h := newHarnessAt(t, workHoursTOML, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Later", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}}
	ctx := context.Background()

	full, err := h.sched.tick(ctx)
	if err != nil || !full {
		t.Fatalf("first tick: full=%v err=%v", full, err)
	}
	if h.gh.CallCount("issue list") != 1 || h.gh.CallCount("pr list") != 1 {
		t.Fatalf("first tick should poll once: %v", h.gh.Calls)
	}
	// The loop keeps ticking every poll_interval, but off hours the next
	// GitHub poll is an hour out, so the second pass is local.
	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	full, err = h.sched.tick(ctx)
	if err != nil || full {
		t.Fatalf("second tick: full=%v err=%v", full, err)
	}
	if h.gh.CallCount("issue list") != 1 || h.gh.CallCount("pr list") != 1 {
		t.Fatalf("a local pass must not poll GitHub: %v", h.gh.Calls)
	}

	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.InWorkHours == nil || *st.InWorkHours {
		t.Fatalf("status should report off hours: %+v", st.InWorkHours)
	}
	if want := time.Date(2026, 8, 29, 13, 0, 0, 0, time.UTC); !st.NextPoll.Equal(want) {
		t.Fatalf("next poll %s, want %s", st.NextPoll, want)
	}

	// Once the off-hours interval has elapsed, polling resumes.
	h.clock.advance(time.Hour)
	if full, err := h.sched.tick(ctx); err != nil || !full {
		t.Fatalf("tick after the off-hours interval: full=%v err=%v", full, err)
	}
	if h.gh.CallCount("issue list") != 2 {
		t.Fatalf("expected a second poll: %v", h.gh.Calls)
	}
}

func TestInWorkHoursPollsEveryTick(t *testing.T) {
	// 2026-08-31 12:00 UTC is a Monday inside the window.
	h := newHarnessAt(t, workHoursTOML, time.Date(2026, 8, 31, 12, 0, 0, 0, time.UTC))
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if full, err := h.sched.tick(ctx); err != nil || !full {
			t.Fatalf("tick %d: full=%v err=%v", i, full, err)
		}
		h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	}
	if h.gh.CallCount("issue list") != 3 || h.gh.CallCount("pr list") != 3 {
		t.Fatalf("every tick should poll in work hours: %v", h.gh.Calls)
	}
	h.sched.writeStatus()
	if st, _ := h.store.LoadStatus(); st.InWorkHours == nil || !*st.InWorkHours {
		t.Fatalf("status should report work hours: %+v", st.InWorkHours)
	}
}

func TestLocalPassUnblocksIssueOffHours(t *testing.T) {
	h := newHarnessAt(t, workHoursTOML, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:blocked"}, {Name: "bees:size/s"}}}
	ctx := context.Background()
	if _, err := h.sched.tick(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.gh.History[1]) != 0 {
		t.Fatalf("nothing has answered the question yet: %v", h.gh.History[1])
	}
	// The project manager answers by mail; the next local pass picks it up
	// without polling GitHub.
	if _, err := h.box.Send(mail.Message{From: config.RoleProjectManager, To: config.RoleDeveloper, Subject: "Re: Vague", Body: "do X", Work: ghwork.New(1, 0)}); err != nil {
		t.Fatal(err)
	}
	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	if full, err := h.sched.tick(ctx); err != nil || full {
		t.Fatalf("second tick: full=%v err=%v", full, err)
	}
	if got := strings.Join(h.gh.History[1], ","); got != "bees:ready" {
		t.Fatalf("history: %v", h.gh.History[1])
	}
	if h.gh.CallCount("issue list") != 1 || h.gh.CallCount("pr list") != 1 {
		t.Fatalf("the local pass polled GitHub: %v", h.gh.Calls)
	}
	// The new state label reached the cached poll, so the local passes until
	// the next one classify the issue as ready and do not move it again.
	cached := h.sched.classify(context.Background(), h.sched.lastIssues, h.sched.lastPRs)
	if len(cached.byState["blocked"]) != 0 || len(cached.byState["ready"]) != 1 {
		t.Fatalf("cached poll still says blocked: %v", h.sched.lastIssues)
	}
	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	if full, err := h.sched.tick(ctx); err != nil || full {
		t.Fatalf("third tick: full=%v err=%v", full, err)
	}
	if got := strings.Join(h.gh.History[1], ","); got != "bees:ready" {
		t.Fatalf("a local pass moved the label again: %v", h.gh.History[1])
	}
}

// workHoursDevTOML is workHoursTOML with the developer and reviewer enabled,
// so a whole develop -> review -> approve cycle runs off hours.
const workHoursDevTOML = baseTOML + `
off_hours_poll_interval = "1h"
work_hours = "09:00-18:00"
work_days = ["mon", "tue", "wed", "thu", "fri"]
timezone = "UTC"
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

func TestLocalPassDoesNotRedispatchFinishedIssues(t *testing.T) {
	// Saturday: off hours, so the tick after the first one is local and its
	// snapshot still carries the issue's pre-work labels.
	h := newHarnessAt(t, workHoursDevTOML, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: time.Now()}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if full, err := h.sched.tick(ctx); err != nil || !full {
		t.Fatalf("first tick: full=%v err=%v", full, err)
	}
	h.sched.wg.Wait()
	want := "bees:in-progress,bees:review,bees:in-progress,bees:review,bees:approved"
	before := strings.Join(h.gh.History[1], ",")
	if before != want {
		t.Fatalf("first pass history: %s, want %s", before, want)
	}

	lists, views := h.gh.CallCount("issue list")+h.gh.CallCount("pr list"), h.gh.CallCount("issue view")

	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	if full, err := h.sched.tick(ctx); err != nil || full {
		t.Fatalf("second tick: full=%v err=%v", full, err)
	}
	h.sched.wg.Wait()
	if got := strings.Join(h.gh.History[1], ","); got != before {
		t.Fatalf("local pass restarted a finished issue: %s", got)
	}
	if n := len(h.sessions(config.RoleDeveloper)); n != 2 {
		t.Fatalf("developer sessions: %d, want 2", n)
	}
	// The candidate cost one live issue view, not a poll.
	if n := h.gh.CallCount("issue list") + h.gh.CallCount("pr list"); n != lists {
		t.Fatalf("a local pass must not poll GitHub: %v", h.gh.Calls)
	}
	if n := h.gh.CallCount("issue view"); n != views+1 {
		t.Fatalf("live checks: %d, want 1", n-views)
	}
	// The refreshed issue replaces the stale cached one, so the next local
	// pass classifies it as approved and does not check it again.
	views = h.gh.CallCount("issue view")
	h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
	if full, err := h.sched.tick(ctx); err != nil || full {
		t.Fatalf("third tick: full=%v err=%v", full, err)
	}
	h.sched.wg.Wait()
	if h.gh.CallCount("issue view") != views {
		t.Fatalf("an issue the cache already knows is approved was checked again: %v", h.gh.Calls)
	}
	if got := strings.Join(h.gh.History[1], ","); got != before {
		t.Fatalf("local pass restarted a finished issue: %s", got)
	}
}

// An issue a human filed without a state label is labelled bees:feedback
// once: the cached poll a local pass classifies from is updated with the new
// label, so the passes in between two GitHub polls do not repeat the edit.
func TestLocalPassDoesNotRepeatTheFeedbackLabel(t *testing.T) {
	// Saturday: off hours, so only the first tick polls GitHub. The product
	// manager stays enabled: this test is about the feedback route
	// specifically, not about staying disabled like workHoursTOML's roles.
	h := newHarnessAt(t, baseTOML+`
off_hours_poll_interval = "1h"
work_hours = "09:00-18:00"
work_days = ["mon", "tue", "wed", "thu", "fri"]
timezone = "UTC"
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`, time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC))
	// tick dispatches singletons in goroutines and does not wait for them,
	// so an enabled product manager would still be writing into the test's
	// temp directory when it is removed. OnlyRoles scopes dispatch without
	// touching the routing, which reads the configured factory.
	h.sched.OnlyRoles = map[string]bool{}
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", State: "OPEN", Labels: []github.Label{{Name: "bees"}}}
	ctx := context.Background()

	if full, err := h.sched.tick(ctx); err != nil || !full {
		t.Fatalf("first tick: full=%v err=%v", full, err)
	}
	lists := h.gh.CallCount("issue list")
	for i := 0; i < 2; i++ {
		h.clock.advance(h.cfg.Scheduler.PollInterval.Duration)
		if full, err := h.sched.tick(ctx); err != nil || full {
			t.Fatalf("local tick %d: full=%v err=%v", i, full, err)
		}
	}
	if got := strings.Join(h.gh.History[1], ","); got != "bees:feedback" {
		t.Fatalf("label history: %q, want bees:feedback once", got)
	}
	if n := h.gh.CallCount("issue list"); n != lists {
		t.Fatalf("a local pass polled GitHub: %v", h.gh.Calls)
	}
}

// noRolesTOML disables every role, so a pass does nothing but reconcile.
const noRolesTOML = baseTOML + `
[roles.developer]
enabled = false
[roles.reviewer]
enabled = false
[roles.product_manager]
enabled = false
[roles.project_manager]
enabled = false
[roles.qa]
enabled = false
`

func TestReadyIssueWithoutASizeGetsTheDefault(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	// 1 was fast-tracked to ready by a human and has no size; 2 was sized
	// by the project manager.
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Fast-tracked", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}}, CreatedAt: time.Now()}
	h.gh.Issues[2] = &github.Issue{Number: 2, Title: "Sized", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/xs"}}, CreatedAt: time.Now()}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.History[1], ","); got != "bees:size/m" {
		t.Fatalf("issue 1 label history: %q, want bees:size/m", got)
	}
	if got := h.gh.History[2]; len(got) != 0 {
		t.Fatalf("issue 2 was already sized and must be left alone: %v", got)
	}
	if !github.HasLabel(h.gh.Issues[1].Labels, "bees:ready") {
		t.Fatalf("issue 1 lost its state label: %v", h.gh.Issues[1].Labels)
	}
	if !strings.Contains(h.logs.String(), "ready issue without a size gets the default") {
		t.Fatalf("no log line about the default size:\n%s", h.logs.String())
	}
	// reconcile sized the issue and recounts before the status is written,
	// so the first pass already reports it as "m" rather than unsized.
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.ReadySizes["m"] != 1 || st.ReadySizes["xs"] != 1 || st.ReadySizes[""] != 0 {
		t.Fatalf("ready sizes after the first pass: %v", st.ReadySizes)
	}

	// Second pass: the label is there, so nothing is added again and the
	// breakdown reports it.
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.History[1], ","); got != "bees:size/m" {
		t.Fatalf("size added twice: %q", got)
	}
	st, err = h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if st.ReadySizes["m"] != 1 || st.ReadySizes["xs"] != 1 || st.ReadySizes[""] != 0 {
		t.Fatalf("ready sizes after the second pass: %v", st.ReadySizes)
	}
}

// An issue reconcile unblocks joins the ready queue after the blocked loop
// has run, so the size backstop has to come last to size it in the same pass.
func TestUnblockedIssueIsSizedInTheSamePass(t *testing.T) {
	h := newHarness(t, noRolesTOML)
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:blocked"}}}
	// The answer the developer asked for is waiting in the mailbox.
	if _, err := h.box.Send(mail.Message{From: config.RoleProjectManager, To: config.RoleDeveloper, Subject: "Re: Vague", Body: "do X", Work: ghwork.New(1, 0)}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(h.gh.History[1], ","); got != "bees:ready,bees:size/m" {
		t.Fatalf("label history: %q, want bees:ready,bees:size/m", got)
	}
	// Both edits reached the cached poll, so the local passes until the next
	// poll classify the issue as a sized ready one and add neither again.
	cached := h.sched.classify(context.Background(), h.sched.lastIssues, h.sched.lastPRs).byState["ready"]
	if len(cached) != 1 || !github.HasLabel(cached[0].Labels, "bees:ready") || !github.HasLabel(cached[0].Labels, "bees:size/m") {
		t.Fatalf("cached issue: %v", h.sched.lastIssues)
	}
	h.sched.localPass(ctx)
	if got := strings.Join(h.gh.History[1], ","); got != "bees:ready,bees:size/m" {
		t.Fatalf("a local pass repeated the edits: %q", got)
	}
}

func TestSizeSurvivesTheStateMachine(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.product_manager]\nenabled = false\n[roles.qa]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Build the thing", Body: "please", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/xs"}}, CreatedAt: time.Now()}
	h.gh.PRs[fakePR] = &github.PR{Number: fakePR, Title: "Build the thing", State: "OPEN", HeadRefName: "bees/issue-1", BaseRefName: "main", Labels: []github.Label{{Name: "bees"}}}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}

	// ready -> in-progress -> review -> ... -> approved, and the size label
	// is still there: state transitions only touch the state labels.
	if got := strings.Join(h.gh.History[1], ","); got != "bees:in-progress,bees:review,bees:in-progress,bees:review,bees:approved" {
		t.Fatalf("label history: %s", got)
	}
	if !github.HasLabel(h.gh.Issues[1].Labels, "bees:size/xs") {
		t.Fatalf("size label lost: %v", h.gh.Issues[1].Labels)
	}
	// The reviewer's judge session is told the brief's size, which the
	// review pipeline sized itself, not the label's.
	dirs := h.sessions(config.RoleReviewer)
	if len(dirs) == 0 {
		t.Fatal("no reviewer session")
	}
	prompt, _ := os.ReadFile(filepath.Join(dirs[0], "prompt.md"))
	if !strings.Contains(string(prompt), "the change was sized `s`") {
		t.Fatalf("reviewer task does not carry the brief's size:\n%s", prompt)
	}
}

// An issue a human filed without a state label is counted under "no_state",
// never under the empty string, and the count is refreshed after reconcile has
// moved it out — not left stale until the next poll. It leaves the state
// machine entirely: bees:feedback is a kind, so it lands in no queue at all.
func TestNoStateQueueIsNamedAndRecountedAfterReconcile(t *testing.T) {
	h := newHarness(t, baseTOML)
	h.sched.OnlyRoles = map[string]bool{}
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Filed from the GitHub UI", State: "OPEN", Labels: []github.Label{{Name: "bees"}}}
	// A blocked issue whose question reconcile is about to answer: it must
	// leave the blocked bucket, not be counted in both.
	h.gh.Issues[2] = &github.Issue{Number: 2, Title: "Vague", State: "OPEN", Labels: []github.Label{{Name: "bees"}, {Name: "bees:blocked"}}}
	if _, err := h.box.Send(mail.Message{From: config.RoleProjectManager, To: config.RoleDeveloper, Subject: "Re: Vague", Body: "do X", Work: ghwork.New(2, 0)}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	snap, err := h.sched.poll(ctx)
	if err != nil {
		t.Fatal(err)
	}
	h.sched.writeStatus()
	st, err := h.store.LoadStatus()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := st.Queues[""]; ok {
		t.Errorf("unnamed queue key in status: %+v", st.Queues)
	}
	if st.Queues["no_state"] != 1 {
		t.Errorf("after poll: no_state = %d, want 1 (%+v)", st.Queues["no_state"], st.Queues)
	}
	if st.Queues["blocked"] != 1 {
		t.Errorf("after poll: blocked = %d, want 1 (%+v)", st.Queues["blocked"], st.Queues)
	}

	if err := h.sched.reconcile(ctx, snap); err != nil {
		t.Fatal(err)
	}
	h.sched.writeStatus()
	if st, err = h.store.LoadStatus(); err != nil {
		t.Fatal(err)
	}
	if st.Queues["no_state"] != 0 {
		t.Errorf("after reconcile: no_state = %d, want 0 (%+v)", st.Queues["no_state"], st.Queues)
	}
	// Issue 1 became feedback, which is no queue: it must not have been
	// dropped into triage on the way out.
	if st.Queues["triage"] != 0 {
		t.Errorf("after reconcile: triage = %d, want 0 (%+v)", st.Queues["triage"], st.Queues)
	}
	if !github.HasLabel(h.gh.Issues[1].Labels, "bees:feedback") {
		t.Errorf("issue 1 labels %v, want bees:feedback", h.gh.Issues[1].Labels)
	}
	// The unblocked issue moved to ready; counting it in both buckets would
	// make `bees status` report more issues than exist.
	if st.Queues["blocked"] != 0 {
		t.Errorf("after reconcile: blocked = %d, want 0 (%+v)", st.Queues["blocked"], st.Queues)
	}
	if st.Queues["ready"] != 1 {
		t.Errorf("after reconcile: ready = %d, want 1 (%+v)", st.Queues["ready"], st.Queues)
	}
}

// Attaching loose work items to their feature is once-a-pass product manager
// work, so its task must say which items are already attached: runProductManager
// looks each open work item's parent up and the prompt renders it.
func TestProductManagerSeesEachWorkItemsParent(t *testing.T) {
	h := newHarness(t, baseTOML+"\n[roles.qa]\nenabled = false\n[roles.developer]\nenabled = false\n[roles.project_manager]\nenabled = false\n")
	now := time.Now()
	// The fake answers the parent query for issue 1 with feature #5.
	h.gh.Issues[5] = &github.Issue{Number: 5, Title: "Exports", Body: "csv please", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:feature"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		Comments: []github.Comment{{Author: github.Author{Login: "kyle"}, Body: "work items: #1\n\n<!-- bees:product_manager -->", CreatedAt: now}}}
	h.gh.Issues[1] = &github.Issue{Number: 1, Title: "Export to CSV", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:ready"}, {Name: "bees:size/s"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	// A bug QA filed: attached to nothing, which is what the column is for.
	h.gh.Issues[2] = &github.Issue{Number: 2, Title: "Header is wrong", State: "OPEN", Author: github.Author{Login: "kyle"},
		Labels: []github.Label{{Name: "bees"}, {Name: "bees:triage"}, {Name: "bees:bug"}, {Name: "bees:size/s"}}, CreatedAt: now.Add(-time.Hour), UpdatedAt: now}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	pm := h.sessions(config.RoleProductManager)
	if len(pm) != 1 {
		t.Fatalf("product manager sessions: %d", len(pm))
	}
	prompt, _ := os.ReadFile(filepath.Join(pm[0], "prompt.md"))
	if !strings.Contains(string(prompt), "| 1 | ready | - | #5 Exports | - | Export to CSV |") {
		t.Errorf("work item #1 should name its parent feature:\n%s", prompt)
	}
	if !strings.Contains(string(prompt), "| 2 | triage | bug | - | - | Header is wrong |") {
		t.Errorf("loose work item #2 should show - for its parent:\n%s", prompt)
	}
}

// TestQAReceivesHumanMail: `bees mail send --from human --to qa` is a
// documented channel, and until #199 runQA built its session with no Inbox at
// all, so a message sat unread forever. The message reaches the next QA
// session — this test's own next run is its interval plus a new merge, not
// the mail — and is marked read there, which is what keeps the following
// session's mail section empty.
func TestQAReceivesHumanMail(t *testing.T) {
	h := newHarnessAt(t, baseTOML+"\n[roles.developer]\nenabled = false\n[roles.project_manager]\nenabled = false\n[roles.product_manager]\nenabled = false\n", time.Now())
	merged := h.clock.now().Add(-time.Minute)
	h.gh.PRs[300] = &github.PR{Number: 300, Title: "Merged", State: "MERGED", HeadRefName: "bees/issue-9",
		Labels: []github.Label{{Name: "bees"}}, MergedAt: &merged}
	if _, err := h.box.Send(mail.Message{From: HumanSender, To: config.RoleQA,
		Subject: "Focus", Body: "test the mail commands by hand this time"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	qa := h.sessions(config.RoleQA)
	if len(qa) != 1 {
		t.Fatalf("qa sessions: %d", len(qa))
	}
	first, _ := os.ReadFile(filepath.Join(qa[0], "prompt.md"))
	for _, want := range []string{"## Mail for you (1)", "test the mail commands by hand this time"} {
		if !strings.Contains(string(first), want) {
			t.Errorf("the qa session is missing %q:\n%s", want, first)
		}
	}
	if unread, _ := h.box.List(mail.Filter{To: config.RoleQA, UnreadOnly: true}); len(unread) != 0 {
		t.Errorf("qa mail left unread: %+v", unread)
	}
	// The next run, once the interval has passed and something else merged,
	// is not told the same thing again.
	next := h.clock.now().Add(time.Hour)
	h.gh.PRs[300].MergedAt = &next
	h.clock.advance(2 * time.Hour)
	forcePoll(h)
	if err := h.sched.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if qa = h.sessions(config.RoleQA); len(qa) != 2 {
		t.Fatalf("qa sessions after the second pass: %d", len(qa))
	}
	second, _ := os.ReadFile(filepath.Join(qa[1], "prompt.md"))
	if strings.Contains(string(second), "test the mail commands by hand this time") {
		t.Errorf("the message was delivered twice:\n%s", second)
	}
	if !strings.Contains(string(second), "## Mail for you (0)") || !strings.Contains(string(second), "_No new mail._") {
		t.Errorf("the second qa session has no empty mail section:\n%s", second)
	}
}

func envIntFixture(value string) int { n, _ := strconv.Atoi(value); return n }
