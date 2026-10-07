package agenttest_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

// TestMain lets this package's own test binary double as the fake `codex
// app-server` it tests, the way FAKE_CLAUDE does for claude in
// internal/scheduler/scheduler_test.go: see agenttest.CodexAppServerFakeEnv.
func TestMain(m *testing.M) {
	if os.Getenv(agenttest.CodexAppServerFakeEnv) == "1" {
		os.Exit(agenttest.RunCodexAppServer())
	}
	os.Exit(m.Run())
}

// codexConversation drives a fake `codex app-server` over pipes: send
// writes a JSON-RPC message to its stdin, recv reads the next line from
// its stdout decoded as one.
type codexConversation struct {
	t      *testing.T
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    *bufio.Scanner
	waited sync.Once
	err    error
}

func startCodex(t *testing.T, bin string, args ...string) *codexConversation {
	t.Helper()
	cmd := exec.Command(bin, args...)
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
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	c := &codexConversation{t: t, cmd: cmd, in: stdin, out: sc}
	t.Cleanup(func() {
		_ = c.in.Close()
		c.wait()
	})
	return c
}

func (c *codexConversation) wait() error {
	c.waited.Do(func() { c.err = c.cmd.Wait() })
	return c.err
}

func (c *codexConversation) send(v map[string]any) {
	c.t.Helper()
	v["jsonrpc"] = "2.0"
	b, err := json.Marshal(v)
	if err != nil {
		c.t.Fatal(err)
	}
	if _, err := c.in.Write(append(b, '\n')); err != nil {
		c.t.Fatal(err)
	}
}

func (c *codexConversation) recv() map[string]any {
	c.t.Helper()
	if !c.out.Scan() {
		c.t.Fatalf("fake codex app-server exited without answering: %v", c.out.Err())
	}
	var msg map[string]any
	if err := json.Unmarshal(c.out.Bytes(), &msg); err != nil {
		c.t.Fatalf("decode %q: %v", c.out.Text(), err)
	}
	return msg
}

func (c *codexConversation) closeStdin() {
	c.t.Helper()
	if err := c.in.Close(); err != nil {
		c.t.Fatal(err)
	}
}

// handshake drives the fake through initialize and config/read, common to
// every conversation.
func (c *codexConversation) handshake() {
	c.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "bees"}}})
	if msg := c.recv(); msg["id"] != float64(1) || msg["result"] == nil {
		c.t.Fatalf("initialize answer: %v", msg)
	}
	c.send(map[string]any{"method": "initialized"})
	c.send(map[string]any{"id": 2, "method": "config/read", "params": map[string]any{"cwd": "/work"}})
	if msg := c.recv(); msg["id"] != float64(2) || msg["result"] == nil {
		c.t.Fatalf("config/read answer: %v", msg)
	}
}

// TestCodexAppServerFullConversation drives a fake `codex app-server`
// through initialize, config/read, thread/start, turn/start and a
// scripted stream of notifications ending in turn/completed, then closes
// stdin and checks the fake exits and that its record names the right
// argv and received lines.
func TestCodexAppServerFullConversation(t *testing.T) {
	script := agenttest.CodexAppServerScript{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		Actions: []agenttest.CodexAppServerAction{
			{Notify: &agenttest.CodexAppServerMessage{Method: "item/completed", Params: map[string]any{"item": map[string]any{"type": "agent_message", "text": "hi"}}}},
			{Notify: &agenttest.CodexAppServerMessage{Method: "thread/tokenUsage/updated", Params: map[string]any{"usage": map[string]any{"total": 10}}}},
			{Notify: &agenttest.CodexAppServerMessage{Method: "turn/completed", Params: map[string]any{"status": "completed"}}},
		},
	}
	bin, record := agenttest.CodexAppServer(t, script)
	c := startCodex(t, bin, "app-server")
	c.handshake()

	c.send(map[string]any{"id": 3, "method": "thread/start", "params": map[string]any{"cwd": "/work"}})
	msg := c.recv()
	thread, _ := msg["result"].(map[string]any)["thread"].(map[string]any)
	if thread["id"] != "thread-1" {
		t.Fatalf("thread/start answer: %v", msg)
	}

	c.send(map[string]any{"id": 4, "method": "turn/start", "params": map[string]any{"threadId": "thread-1", "input": "TASK"}})
	msg = c.recv()
	turn, _ := msg["result"].(map[string]any)["turn"].(map[string]any)
	if turn["id"] != "turn-1" {
		t.Fatalf("turn/start answer: %v", msg)
	}

	if msg = c.recv(); msg["method"] != "item/completed" {
		t.Fatalf("expected item/completed, got %v", msg)
	}
	if msg = c.recv(); msg["method"] != "thread/tokenUsage/updated" {
		t.Fatalf("expected thread/tokenUsage/updated, got %v", msg)
	}
	if msg = c.recv(); msg["method"] != "turn/completed" {
		t.Fatalf("expected turn/completed, got %v", msg)
	}

	c.closeStdin()
	if err := c.wait(); err != nil {
		t.Fatalf("fake codex app-server exit: %v", err)
	}

	rec := agenttest.ReadCodexAppServerRecord(t, record)
	if rec.Starts != 1 {
		t.Errorf("starts = %d, want 1", rec.Starts)
	}
	if len(rec.Argv) != 1 || len(rec.Argv[0]) == 0 || rec.Argv[0][len(rec.Argv[0])-1] != "app-server" {
		t.Errorf("argv = %v", rec.Argv)
	}
	if len(rec.Lines) != 5 {
		t.Errorf("lines = %v, want 5 received lines", rec.Lines)
	}
	if !strings.Contains(rec.Lines[3], `"method":"thread/start"`) {
		t.Errorf("thread/start was not recorded: %v", rec.Lines)
	}
}

// TestCodexAppServerFullConversationWithoutConfigRead drives a fake `codex
// app-server` through a conversation that never sends config/read — the
// shape an ordinary or ToolsAll turn's conversation takes — straight from
// initialize to thread/start, turn/start and turn/completed.
func TestCodexAppServerFullConversationWithoutConfigRead(t *testing.T) {
	script := agenttest.CodexAppServerScript{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		Actions: []agenttest.CodexAppServerAction{
			{Notify: &agenttest.CodexAppServerMessage{Method: "turn/completed", Params: map[string]any{"turn": map[string]any{"status": "completed"}}}},
		},
	}
	bin, _ := agenttest.CodexAppServer(t, script)
	c := startCodex(t, bin, "app-server")

	c.send(map[string]any{"id": 1, "method": "initialize", "params": map[string]any{"clientInfo": map[string]any{"name": "bees"}}})
	if msg := c.recv(); msg["id"] != float64(1) || msg["result"] == nil {
		c.t.Fatalf("initialize answer: %v", msg)
	}
	c.send(map[string]any{"method": "initialized"})

	c.send(map[string]any{"id": 2, "method": "thread/start", "params": map[string]any{"cwd": "/work"}})
	msg := c.recv()
	thread, _ := msg["result"].(map[string]any)["thread"].(map[string]any)
	if thread["id"] != "thread-1" {
		t.Fatalf("thread/start answer: %v", msg)
	}

	c.send(map[string]any{"id": 3, "method": "turn/start", "params": map[string]any{"threadId": "thread-1", "input": "TASK"}})
	c.recv()

	if msg = c.recv(); msg["method"] != "turn/completed" {
		t.Fatalf("expected turn/completed, got %v", msg)
	}

	c.closeStdin()
	if err := c.wait(); err != nil {
		t.Fatalf("fake codex app-server exit: %v", err)
	}
}

// TestCodexAppServerResumeRejected drives a fake `codex app-server` through
// a `thread/resume` the script answers with a JSON-RPC -32600 error, as a
// resume for a thread id codex does not know would be.
func TestCodexAppServerResumeRejected(t *testing.T) {
	script := agenttest.CodexAppServerScript{
		ResumeError: &agenttest.CodexRPCError{Code: -32600, Message: "no rollout found for thread id abc"},
	}
	bin, _ := agenttest.CodexAppServer(t, script)
	c := startCodex(t, bin, "app-server")
	c.handshake()

	c.send(map[string]any{"id": 3, "method": "thread/resume", "params": map[string]any{"threadId": "abc"}})
	msg := c.recv()
	errv, _ := msg["error"].(map[string]any)
	if errv == nil || errv["code"] != float64(-32600) || !strings.Contains(fmt.Sprint(errv["message"]), "no rollout found for thread id abc") {
		t.Fatalf("thread/resume answer: %v", msg)
	}
	if _, ok := msg["result"]; ok {
		t.Fatalf("an error answer also carried a result: %v", msg)
	}

	c.closeStdin()
	if err := c.wait(); err != nil {
		t.Fatalf("fake codex app-server exit: %v", err)
	}
}

// TestCodexAppServerServerRequest drives a fake `codex app-server` scripted
// to send a server request of its own mid-turn, answers it and checks the
// answer line was recorded, then that the turn still completes.
func TestCodexAppServerServerRequest(t *testing.T) {
	script := agenttest.CodexAppServerScript{
		ThreadID: "thread-1",
		TurnID:   "turn-1",
		Actions: []agenttest.CodexAppServerAction{
			{Request: &agenttest.CodexAppServerMessage{Method: "mcpServer/elicitation/request", Params: map[string]any{"message": "proceed?"}}},
			{Notify: &agenttest.CodexAppServerMessage{Method: "turn/completed", Params: map[string]any{"status": "completed"}}},
		},
	}
	bin, record := agenttest.CodexAppServer(t, script)
	c := startCodex(t, bin, "app-server")
	c.handshake()
	c.send(map[string]any{"id": 3, "method": "thread/start", "params": map[string]any{"cwd": "/work"}})
	c.recv()
	c.send(map[string]any{"id": 4, "method": "turn/start", "params": map[string]any{"threadId": "thread-1"}})
	c.recv()

	msg := c.recv()
	if msg["method"] != "mcpServer/elicitation/request" {
		t.Fatalf("expected a server request, got %v", msg)
	}
	id := msg["id"]
	c.send(map[string]any{"id": id, "result": map[string]any{"action": "cancel"}})

	if msg = c.recv(); msg["method"] != "turn/completed" {
		t.Fatalf("expected turn/completed after the server request was answered, got %v", msg)
	}

	c.closeStdin()
	if err := c.wait(); err != nil {
		t.Fatalf("fake codex app-server exit: %v", err)
	}

	rec := agenttest.ReadCodexAppServerRecord(t, record)
	found := false
	for _, line := range rec.Lines {
		if strings.Contains(line, `"action":"cancel"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("the server request's answer was not recorded: %v", rec.Lines)
	}
}

// TestCodexAppServerExitsWithoutAnswering drives a fake `codex app-server`
// scripted to exit at config/read without answering it, and checks it
// records the request yet writes nothing back before exiting.
func TestCodexAppServerExitsWithoutAnswering(t *testing.T) {
	script := agenttest.CodexAppServerScript{ExitBefore: "config/read"}
	bin, record := agenttest.CodexAppServer(t, script)
	c := startCodex(t, bin, "app-server")
	c.send(map[string]any{"id": 1, "method": "initialize"})
	c.recv()
	c.send(map[string]any{"id": 2, "method": "config/read", "params": map[string]any{"cwd": "/work"}})

	if c.out.Scan() {
		t.Fatalf("fake codex app-server answered config/read: %s", c.out.Text())
	}
	if err := c.out.Err(); err != nil {
		t.Fatalf("reading stdout: %v", err)
	}

	c.closeStdin()
	if err := c.wait(); err != nil {
		t.Fatalf("fake codex app-server exit: %v", err)
	}

	rec := agenttest.ReadCodexAppServerRecord(t, record)
	if len(rec.Lines) != 2 {
		t.Errorf("lines = %v, want the initialize and config/read requests only", rec.Lines)
	}
}

// TestCodexAppServerCountsStarts runs the same fake `codex app-server`
// twice and checks its record counts both starts and keeps each one's argv.
func TestCodexAppServerCountsStarts(t *testing.T) {
	bin, record := agenttest.CodexAppServer(t, agenttest.CodexAppServerScript{ExitBefore: "initialize"})
	for i := 0; i < 2; i++ {
		c := startCodex(t, bin, "app-server", fmt.Sprintf("run-%d", i))
		c.closeStdin()
		if err := c.wait(); err != nil {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	rec := agenttest.ReadCodexAppServerRecord(t, record)
	if rec.Starts != 2 {
		t.Errorf("starts = %d, want 2", rec.Starts)
	}
	if len(rec.Argv) != 2 || rec.Argv[0][len(rec.Argv[0])-1] != "run-0" || rec.Argv[1][len(rec.Argv[1])-1] != "run-1" {
		t.Errorf("argv = %v", rec.Argv)
	}
}

// TestCodexAppServerInterrupt drives a fake `codex app-server` scripted to
// answer `turn/interrupt` with `turn/completed`.
func TestCodexAppServerInterrupt(t *testing.T) {
	script := agenttest.CodexAppServerScript{
		ThreadID:  "thread-1",
		TurnID:    "turn-1",
		Interrupt: &agenttest.CodexAppServerInterrupt{Status: "interrupted"},
	}
	bin, _ := agenttest.CodexAppServer(t, script)
	c := startCodex(t, bin, "app-server")
	c.handshake()
	c.send(map[string]any{"id": 3, "method": "thread/start", "params": map[string]any{"cwd": "/work"}})
	c.recv()
	c.send(map[string]any{"id": 4, "method": "turn/start", "params": map[string]any{"threadId": "thread-1"}})
	c.recv()

	c.send(map[string]any{"id": 5, "method": "turn/interrupt"})
	if msg := c.recv(); msg["id"] != float64(5) || msg["result"] == nil {
		t.Fatalf("turn/interrupt answer: %v", msg)
	}
	msg := c.recv()
	if msg["method"] != "turn/completed" {
		t.Fatalf("expected turn/completed after the interrupt, got %v", msg)
	}
	params, _ := msg["params"].(map[string]any)
	turn, _ := params["turn"].(map[string]any)
	if turn["status"] != "interrupted" {
		t.Fatalf("turn/completed status: %v", msg)
	}

	c.closeStdin()
	if err := c.wait(); err != nil {
		t.Fatalf("fake codex app-server exit: %v", err)
	}
}

// TestCodexAppServerIgnoresInterruptByDefault drives a fake `codex
// app-server` with no CodexAppServerInterrupt scripted, and checks
// `turn/interrupt` goes unanswered until stdin closes.
func TestCodexAppServerIgnoresInterruptByDefault(t *testing.T) {
	script := agenttest.CodexAppServerScript{ThreadID: "thread-1", TurnID: "turn-1"}
	bin, _ := agenttest.CodexAppServer(t, script)
	c := startCodex(t, bin, "app-server")
	c.handshake()
	c.send(map[string]any{"id": 3, "method": "thread/start", "params": map[string]any{"cwd": "/work"}})
	c.recv()
	c.send(map[string]any{"id": 4, "method": "turn/start", "params": map[string]any{"threadId": "thread-1"}})
	c.recv()

	c.send(map[string]any{"id": 5, "method": "turn/interrupt"})
	c.closeStdin()
	if err := c.wait(); err != nil {
		t.Fatalf("fake codex app-server exit: %v", err)
	}
	if c.out.Scan() {
		t.Fatalf("turn/interrupt was answered though not scripted: %s", c.out.Text())
	}
}
