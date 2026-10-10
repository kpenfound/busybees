package scheduler

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
)

// rpcConn drives one fake `codex app-server` process over its stdin and
// stdout, the way the runner will once core/agent speaks this conversation:
// no scheduler or session.Runner is involved, only the pipes themselves.
type rpcConn struct {
	t      *testing.T
	cmd    *exec.Cmd
	stdin  *json.Encoder
	stdout *bufio.Scanner
	nextID int
}

// rpcMsg is one line of the conversation, read generically: a response
// carries ID and Result, a notification carries Method and Params and no
// ID.
type rpcMsg struct {
	ID     *int           `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
	Result map[string]any `json:"result"`
	Error  map[string]any `json:"error"`
}

// startFakeCodexAppServer launches the test binary as `codex app-server`
// with FAKE_CLAUDE=1, so TestMain's fakeCodexAppServer answers it, wired to
// an app-server session: role, sessionDir and stateDir are exactly what the
// runner sets for a real session, and env adds whatever FAKE_* variables
// steer the role action under test.
func startFakeCodexAppServer(t *testing.T, role, sessionDir, stateDir string, env ...string) *rpcConn {
	t.Helper()
	cmd := exec.Command(os.Args[0], "app-server")
	cmd.Env = append(append([]string{}, os.Environ()...),
		"FAKE_CLAUDE=1",
		session.EnvRole+"="+role,
		session.EnvSessionDir+"="+sessionDir,
		session.EnvStateDir+"="+stateDir,
	)
	cmd.Env = append(cmd.Env, env...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	return &rpcConn{t: t, cmd: cmd, stdin: json.NewEncoder(stdin), stdout: sc}
}

// request sends method as a request and returns its result.
func (c *rpcConn) request(method string, params map[string]any) map[string]any {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	if err := c.stdin.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.t.Fatal(err)
	}
	for {
		msg := c.read()
		if msg.ID != nil && *msg.ID == id {
			return msg.Result
		}
	}
}

// notify sends method as a notification, which gets no answer.
func (c *rpcConn) notify(method string, params map[string]any) {
	c.t.Helper()
	if err := c.stdin.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
		c.t.Fatal(err)
	}
}

// requestRaw sends method as a request and returns the whole response
// message, result or error, unlike request which only ever returns Result.
func (c *rpcConn) requestRaw(method string, params map[string]any) rpcMsg {
	c.t.Helper()
	c.nextID++
	id := c.nextID
	if err := c.stdin.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.t.Fatal(err)
	}
	for {
		msg := c.read()
		if msg.ID != nil && *msg.ID == id {
			return msg
		}
	}
}

// read decodes the next line the fake wrote.
func (c *rpcConn) read() rpcMsg {
	c.t.Helper()
	if !c.stdout.Scan() {
		if err := c.stdout.Err(); err != nil {
			c.t.Fatal(err)
		}
		c.t.Fatal("fake codex app-server: stdout closed early")
	}
	var msg rpcMsg
	if err := json.Unmarshal(c.stdout.Bytes(), &msg); err != nil {
		c.t.Fatalf("decode %q: %v", c.stdout.Text(), err)
	}
	return msg
}

// untilMethod reads notifications until one named method arrives, and
// returns it.
func (c *rpcConn) untilMethod(method string) rpcMsg {
	c.t.Helper()
	for {
		if msg := c.read(); msg.Method == method {
			return msg
		}
	}
}

// touchedIssues reads the session's recorded touched issues, one number per
// line, in the form session.RecordTouched wrote them.
func touchedIssues(t *testing.T, sessionDir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(sessionDir, session.TouchedFile))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// readGHEdits reads every edit requestGHEdit recorded under stateDir, in the
// order they were made (RequestEdit's file names sort chronologically).
func readGHEdits(t *testing.T, stateDir string) []ghEdit {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(stateDir, "gh-edits"))
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, len(entries))
	for i, e := range entries {
		names[i] = e.Name()
	}
	sort.Strings(names)
	edits := make([]ghEdit, len(names))
	for i, name := range names {
		b, err := os.ReadFile(filepath.Join(stateDir, "gh-edits", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(b, &edits[i]); err != nil {
			t.Fatal(err)
		}
	}
	return edits
}

// TestFakeCodexAppServerSpeaksTheConversation drives the fake codex in
// app-server mode directly over pipes — no scheduler and no session.Runner,
// since no production code starts `codex app-server` yet — through
// initialize, thread/start, turn/start and the item/completed and
// turn/completed notifications the turn reports, then again through
// thread/resume for the follow-up turn codex's resume support runs as. Each
// turn performs the project manager's FAKE_TRIAGE action, the same action
// `codex exec` and claude perform, so a passing conversation proves the new
// branch, not just its plumbing.
func TestFakeCodexAppServerSpeaksTheConversation(t *testing.T) {
	sessionDir, stateDir := t.TempDir(), t.TempDir()

	conn := startFakeCodexAppServer(t, config.RoleProjectManager, sessionDir, stateDir, "FAKE_TRIAGE=7")
	init := conn.request("initialize", map[string]any{"clientInfo": map[string]any{"name": "bees", "version": "0"}})
	if init["serverInfo"] == nil {
		t.Fatalf("initialize result carries no serverInfo: %+v", init)
	}
	conn.notify("initialized", nil)
	cfg := conn.request("config/read", map[string]any{"cwd": sessionDir})
	if cfg["config"] == nil || cfg["origins"] == nil {
		t.Fatalf("config/read result: %+v", cfg)
	}
	started := conn.request("thread/start", map[string]any{"cwd": sessionDir, "developerInstructions": "be the project manager"})
	thread, _ := started["thread"].(map[string]any)
	threadID, _ := thread["id"].(string)
	if threadID == "" {
		t.Fatalf("thread/start result carries no thread id: %+v", started)
	}
	conn.request("turn/start", map[string]any{"threadId": threadID, "input": []map[string]any{{"type": "text", "text": "move #7 to ready"}}})
	var sawToolCall, sawAgentMessage bool
	for {
		msg := conn.read()
		if msg.Method == "item/completed" {
			item, _ := msg.Params["item"].(map[string]any)
			switch item["type"] {
			case "mcp_tool_call":
				sawToolCall = true
			case "agent_message":
				sawAgentMessage = true
			}
			continue
		}
		if msg.Method == "turn/completed" {
			break
		}
	}
	if !sawToolCall || !sawAgentMessage {
		t.Fatalf("turn/start did not report both completed items: tool call %v, agent message %v", sawToolCall, sawAgentMessage)
	}
	if got := touchedIssues(t, sessionDir); got != "7\n" {
		t.Fatalf("touched issues after turn/start: %q", got)
	}
	outcome, ok, err := session.ReadOutcome(sessionDir)
	if err != nil || !ok || outcome.Status != OutcomeDone {
		t.Fatalf("outcome after turn/start: %+v, ok=%v, err=%v", outcome, ok, err)
	}
	edits := readGHEdits(t, stateDir)
	if len(edits) != 1 || edits[0].Number != 7 {
		t.Fatalf("gh edits after turn/start: %+v", edits)
	}

	// A new session resumes the thread, the way a codex follow-up does: a
	// fresh process, `thread/resume` in place of `thread/start`, and the
	// same scripted action for a different issue.
	resumeSessionDir := t.TempDir()
	resumed := startFakeCodexAppServer(t, config.RoleProjectManager, resumeSessionDir, stateDir, "FAKE_TRIAGE=9")
	resumed.request("initialize", map[string]any{"clientInfo": map[string]any{"name": "bees", "version": "0"}})
	resumed.notify("initialized", nil)
	resumeResult := resumed.request("thread/resume", map[string]any{"threadId": threadID})
	resumedThread, _ := resumeResult["thread"].(map[string]any)
	if got, _ := resumedThread["id"].(string); got != threadID {
		t.Fatalf("thread/resume result: %+v, want thread id %q", resumeResult, threadID)
	}
	resumed.request("turn/start", map[string]any{"threadId": threadID, "input": []map[string]any{{"type": "text", "text": "move #9 to ready"}}})
	resumed.untilMethod("turn/completed")
	if got := touchedIssues(t, resumeSessionDir); got != "9\n" {
		t.Fatalf("touched issues after thread/resume's turn/start: %q", got)
	}
	edits = readGHEdits(t, stateDir)
	if len(edits) != 2 || edits[1].Number != 9 {
		t.Fatalf("gh edits after thread/resume's turn/start: %+v", edits)
	}
}

// TestFakeCodexAppServerRejectsMalformedTurnInput checks that
// fakeCodexAppServer answers a turn/start whose input regresses to a bare
// string with JSON-RPC error -32600, and never runs the scripted role
// action or reports turn/completed for that turn.
func TestFakeCodexAppServerRejectsMalformedTurnInput(t *testing.T) {
	sessionDir, stateDir := t.TempDir(), t.TempDir()

	conn := startFakeCodexAppServer(t, config.RoleProjectManager, sessionDir, stateDir, "FAKE_TRIAGE=7")
	conn.request("initialize", map[string]any{"clientInfo": map[string]any{"name": "bees", "version": "0"}})
	conn.notify("initialized", nil)
	conn.request("config/read", map[string]any{"cwd": sessionDir})
	started := conn.request("thread/start", map[string]any{"cwd": sessionDir, "developerInstructions": "be the project manager"})
	thread, _ := started["thread"].(map[string]any)
	threadID, _ := thread["id"].(string)

	resp := conn.requestRaw("turn/start", map[string]any{"threadId": threadID, "input": "move #7 to ready"})
	if resp.Error == nil || resp.Error["code"] != float64(-32600) {
		t.Fatalf("turn/start with a string input: %+v, want a -32600 error", resp)
	}
	if _, err := os.Stat(filepath.Join(sessionDir, session.TouchedFile)); !os.IsNotExist(err) {
		t.Fatalf("scripted role action ran after a malformed turn/start: touched file stat err=%v", err)
	}
	if _, ok, _ := session.ReadOutcome(sessionDir); ok {
		t.Fatalf("scripted role action recorded an outcome after a malformed turn/start")
	}
}
