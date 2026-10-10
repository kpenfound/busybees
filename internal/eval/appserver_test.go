package eval

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strconv"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
)

// rpcConversation drives this test binary, run as `codex app-server`, over
// its own stdin and stdout, the way a converted codexBackend will.
type rpcConversation struct {
	t    *testing.T
	cmd  *exec.Cmd
	enc  *json.Encoder
	in   *bufio.Scanner
	next int
}

// startFakeAppServer starts this test binary as `codex app-server` with
// env on top of a fake-claude session's own, and wires a JSON-RPC
// conversation to it over pipes.
func startFakeAppServer(t *testing.T, env ...string) *rpcConversation {
	t.Helper()
	cmd := exec.Command(os.Args[0], "app-server")
	cmd.Env = append(append([]string{"FAKE_CLAUDE=1"}, os.Environ()...), env...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	c := &rpcConversation{t: t, cmd: cmd, enc: json.NewEncoder(stdin), in: sc}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Wait()
	})
	return c
}

// request sends a request with the next id and returns the result of the
// response the server sends back for it.
func (c *rpcConversation) request(method string, params map[string]any) json.RawMessage {
	c.t.Helper()
	c.next++
	id := c.next
	if err := c.enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		c.t.Fatal(err)
	}
	var msg struct {
		ID     int             `json:"id"`
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if !c.in.Scan() {
		c.t.Fatalf("%s: the app server ended without answering: %v", method, c.in.Err())
	}
	if err := json.Unmarshal(c.in.Bytes(), &msg); err != nil {
		c.t.Fatalf("%s: decode response: %v (%s)", method, err, c.in.Text())
	}
	if msg.ID != id {
		c.t.Fatalf("%s: answered id %d, want %d", method, msg.ID, id)
	}
	if msg.Error != nil {
		c.t.Fatalf("%s: %s", method, msg.Error.Message)
	}
	return msg.Result
}

// notify sends a notification, which the server does not answer.
func (c *rpcConversation) notify(method string, params map[string]any) {
	c.t.Helper()
	if err := c.enc.Encode(map[string]any{"jsonrpc": "2.0", "method": method, "params": params}); err != nil {
		c.t.Fatal(err)
	}
}

// notification reads the next line the server sent as a notification: its
// method and the item or status its params carry.
func (c *rpcConversation) notification() (method string, item, status string) {
	c.t.Helper()
	if !c.in.Scan() {
		c.t.Fatalf("the app server ended before a notification: %v", c.in.Err())
	}
	var msg struct {
		Method string `json:"method"`
		Params struct {
			Item struct {
				Text string `json:"text"`
			} `json:"item"`
			Turn struct {
				Status string `json:"status"`
			} `json:"turn"`
		} `json:"params"`
	}
	if err := json.Unmarshal(c.in.Bytes(), &msg); err != nil {
		c.t.Fatalf("decode notification: %v (%s)", err, c.in.Text())
	}
	return msg.Method, msg.Params.Item.Text, msg.Params.Turn.Status
}

// A full conversation with the fake run as `codex app-server` instead of
// `codex exec --json ...`: initialize, config/read, thread/start with a
// restricted (read-only) sandbox and turn/start with a rubric as its
// input, the way a grader session's turn runs. The fake does the same
// scripted work isReviewSession does for `codex exec` — answers the
// rubric — reported as item/completed and turn/completed instead of
// exec's own stream, and the process exits once stdin closes.
func TestFakeCodexAppServerAnswersARubricOverAFullConversation(t *testing.T) {
	c := startFakeAppServer(t, "FAKE_SCORE=0.42")

	c.request("initialize", map[string]any{"clientInfo": map[string]any{"name": "bees", "version": "0"}})
	c.notify("initialized", nil)
	c.request("config/read", map[string]any{"cwd": "."})

	thread := c.request("thread/start", map[string]any{"cwd": ".", "approvalPolicy": "never", "sandbox": "read-only"})
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(thread, &started); err != nil || started.Thread.ID == "" {
		t.Fatalf("thread/start result: %s (%v)", thread, err)
	}

	c.request("turn/start", map[string]any{"threadId": started.Thread.ID, "input": []map[string]any{{"type": "text", "text": "## The rubric\nScore it."}}})

	method, item, _ := c.notification()
	if method != "item/completed" {
		t.Fatalf("notification: %s", method)
	}
	var answer struct {
		Score float64 `json:"score"`
	}
	if err := json.Unmarshal([]byte(item), &answer); err != nil || strconv.FormatFloat(answer.Score, 'f', -1, 64) != "0.42" {
		t.Fatalf("item/completed text: %q (%v)", item, err)
	}

	method, _, status := c.notification()
	if method != "turn/completed" || status != "completed" {
		t.Fatalf("notification: %s %s", method, status)
	}
}

// Over a conversation whose thread/resume carries a sandbox other than
// read-only — an ordinary role's turn — the fake does the role's scripted
// work (runFakeRole does for `codex exec`) and reports it done.
func TestFakeCodexAppServerRunsAnOrdinaryRoleOverResume(t *testing.T) {
	sessionDir := t.TempDir()
	c := startFakeAppServer(t,
		session.EnvRole+"="+config.RoleProjectManager,
		session.EnvSessionDir+"="+sessionDir,
	)

	c.request("initialize", map[string]any{"clientInfo": map[string]any{"name": "bees", "version": "0"}})
	c.notify("initialized", nil)
	thread := c.request("thread/resume", map[string]any{"threadId": "earlier-thread", "sandbox": "danger-full-access"})
	var resumed struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(thread, &resumed); err != nil || resumed.Thread.ID == "" {
		t.Fatalf("thread/resume result: %s (%v)", thread, err)
	}

	c.request("turn/start", map[string]any{"threadId": resumed.Thread.ID, "input": []map[string]any{{"type": "text", "text": "do the role's work"}}})

	method, item, _ := c.notification()
	if method != "item/completed" || item != "done" {
		t.Fatalf("notification: %s %q", method, item)
	}
	if method, _, status := c.notification(); method != "turn/completed" || status != "completed" {
		t.Fatalf("notification: %s %s", method, status)
	}

	o, ok, err := session.ReadOutcome(sessionDir)
	if err != nil || !ok || o.Status != "done" {
		t.Fatalf("outcome: %+v ok=%v err=%v", o, ok, err)
	}
}

// TestFakeCodexAppServerRejectsStringTurnStartInput checks that a
// turn/start whose input is a bare string — the shape codex_rpc.go must
// never regress to — gets JSON-RPC error -32600, with no scripted turn
// run afterwards.
func TestFakeCodexAppServerRejectsStringTurnStartInput(t *testing.T) {
	c := startFakeAppServer(t)
	c.request("initialize", map[string]any{"clientInfo": map[string]any{"name": "bees", "version": "0"}})
	c.notify("initialized", nil)
	thread := c.request("thread/start", map[string]any{"cwd": ".", "sandbox": "read-only"})
	var started struct {
		Thread struct {
			ID string `json:"id"`
		} `json:"thread"`
	}
	if err := json.Unmarshal(thread, &started); err != nil || started.Thread.ID == "" {
		t.Fatalf("thread/start result: %s (%v)", thread, err)
	}

	c.next++
	id := c.next
	if err := c.enc.Encode(map[string]any{"jsonrpc": "2.0", "id": id, "method": "turn/start", "params": map[string]any{"threadId": started.Thread.ID, "input": "## The rubric\nScore it."}}); err != nil {
		t.Fatal(err)
	}
	var msg struct {
		ID    int `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	if !c.in.Scan() {
		t.Fatalf("turn/start: the app server ended without answering: %v", c.in.Err())
	}
	if err := json.Unmarshal(c.in.Bytes(), &msg); err != nil {
		t.Fatalf("turn/start: decode response: %v (%s)", err, c.in.Text())
	}
	if msg.Error == nil || msg.Error.Code != -32600 {
		t.Fatalf("turn/start answer: %+v, want a -32600 error", msg)
	}
	if msg.Result != nil {
		t.Fatalf("an error answer also carried a result: %+v", msg)
	}
}
