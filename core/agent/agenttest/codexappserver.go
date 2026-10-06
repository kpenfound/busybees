package agenttest

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CodexAppServerFakeEnv set to "1" makes the running test binary a fake
// `codex app-server` (RunCodexAppServer), the same way FAKE_CLAUDE makes it
// a fake `claude` in internal/scheduler/scheduler_test.go: the name does
// not start with BEES_, because the runner strips every inherited BEES_*
// variable from a session, so a flag in that namespace would never reach
// the fake. A test binary that wants to offer this fake, to its own tests
// or to a session a production Runner starts, checks this variable first
// in its TestMain and calls RunCodexAppServer when it is set, as this
// package's own tests do.
const CodexAppServerFakeEnv = "FAKE_CODEX_APP_SERVER"

// codexAppServerDirEnv names the directory CodexAppServer wrote the fake's
// script to (script.json) and RunCodexAppServer appends its record to
// (record.jsonl).
const codexAppServerDirEnv = "FAKE_CODEX_APP_SERVER_DIR"

// CodexRPCError is a JSON-RPC error a scripted fake `codex app-server`
// answers a request with.
type CodexRPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// CodexAppServerMessage is a notification's or a request's method and
// params, as CodexAppServerScript schedules the fake to send one.
type CodexAppServerMessage struct {
	Method string `json:"method"`
	Params any    `json:"params,omitempty"`
}

// CodexAppServerAction is one step a fake `codex app-server` performs once
// it has answered `turn/start`, run in order. Exactly one of Notify and
// Request is set: Notify is sent as a notification of the fake's own;
// Request is sent as a request of the fake's own (any method, known to the
// real protocol or not), with an id the fake assigns, and the fake waits
// for the client's answer to it, which is recorded like any other line the
// fake receives, before moving on to the next action.
type CodexAppServerAction struct {
	Notify  *CodexAppServerMessage `json:"notify,omitempty"`
	Request *CodexAppServerMessage `json:"request,omitempty"`
}

// CodexAppServerInterrupt scripts how a fake `codex app-server` answers
// `turn/interrupt`. A script with no CodexAppServerInterrupt ignores every
// `turn/interrupt` it is sent: it never answers it. Given one, every
// `turn/interrupt` is answered with an empty result, followed by a
// `turn/completed` notification carrying Status.
type CodexAppServerInterrupt struct {
	Status string `json:"status"`
}

// CodexAppServerScript scripts a fake `codex app-server`'s conversation,
// as CodexAppServer hands it to the fake: see that function.
type CodexAppServerScript struct {
	// ExitBefore names the step the fake exits at without answering: one
	// of "initialize", "config/read", "thread/start", "thread/resume" or
	// "turn/start" (whichever of thread/start and thread/resume the
	// client actually sends). Left empty, the fake answers every step it
	// reaches.
	ExitBefore string `json:"exitBefore,omitempty"`

	// InitializeResult is initialize's answer. Left nil, it answers with
	// an empty result.
	InitializeResult any `json:"initializeResult,omitempty"`

	// ConfigResult and ConfigError script config/read's answer; at most
	// one should be set. Neither set answers with an empty result.
	ConfigResult any            `json:"configResult,omitempty"`
	ConfigError  *CodexRPCError `json:"configError,omitempty"`

	// ThreadID and ThreadError script thread/start's answer, as
	// {"thread":{"id":ThreadID}} or the error, and thread/resume's too,
	// unless ResumeID or ResumeError is set.
	ThreadID    string         `json:"threadID,omitempty"`
	ThreadError *CodexRPCError `json:"threadError,omitempty"`
	// ResumeID and ResumeError, whichever is set, override ThreadID and
	// ThreadError for thread/resume only.
	ResumeID    string         `json:"resumeID,omitempty"`
	ResumeError *CodexRPCError `json:"resumeError,omitempty"`

	// TurnID is turn/start's answer, as {"turn":{"id":TurnID}}.
	TurnID string `json:"turnID,omitempty"`

	// Actions runs in order once turn/start is answered.
	Actions []CodexAppServerAction `json:"actions,omitempty"`

	// Interrupt scripts turn/interrupt; see CodexAppServerInterrupt.
	Interrupt *CodexAppServerInterrupt `json:"interrupt,omitempty"`
}

// CodexAppServer arranges for the running test binary, re-executed, to be a
// fake `codex app-server` scripted by script, the way FAKE_CLAUDE and
// Runner.ClaudeBin = os.Args[0] do for claude in
// internal/scheduler/scheduler_test.go (see CONTRIBUTING.md): it sets
// CodexAppServerFakeEnv and the script's location for the rest of the
// calling test (t.Setenv, undone when it ends) and returns the test
// binary's own path, which the caller hands a Runner as CodexBin, or runs
// directly. Invoked with "app-server" among its arguments (any others are
// read and recorded but otherwise ignored), the fake speaks the app-server
// JSON-RPC conversation over stdin and stdout, one message per line, and
// exits when stdin closes. Every line it receives, and its own argv, go to
// the record file whose path CodexAppServer also returns, which
// ReadCodexAppServerRecord reads back; it also counts how many times the
// fake started.
//
// A test binary that wants to offer this fake must check
// CodexAppServerFakeEnv in its own TestMain and call RunCodexAppServer when
// it is set, as this package's own tests do; CodexAppServer only arranges
// the environment, since it has no way to install a TestMain in the
// caller's package.
func CodexAppServer(t *testing.T, script CodexAppServerScript) (bin, record string) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	data, err := json.Marshal(script)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "script.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(CodexAppServerFakeEnv, "1")
	t.Setenv(codexAppServerDirEnv, dir)
	return self, filepath.Join(dir, "record.jsonl")
}

// codexWireMessage is a JSON-RPC message as RunCodexAppServer reads it off
// stdin: a notification, a request or a response, told apart by which
// fields are present.
type codexWireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
}

// codexRecordEntry is one line of a fake `codex app-server`'s record file,
// recording either a start (its argv) or a line it received on stdin.
type codexRecordEntry struct {
	Type string   `json:"type"`
	Argv []string `json:"argv,omitempty"`
	Text string   `json:"text,omitempty"`
}

// CodexAppServerRecord is what a fake `codex app-server` wrote to its
// record file (see CodexAppServer): how many times it started, each
// start's argv in the order the starts happened, and every line it
// received on stdin across every start, in the order it arrived.
type CodexAppServerRecord struct {
	Starts int
	Argv   [][]string
	Lines  []string
}

// ReadCodexAppServerRecord reads back the record file at path, the second
// value CodexAppServer returned.
func ReadCodexAppServerRecord(t *testing.T, path string) CodexAppServerRecord {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rec CodexAppServerRecord
	for _, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		var e codexRecordEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatal(err)
		}
		switch e.Type {
		case "start":
			rec.Starts++
			rec.Argv = append(rec.Argv, e.Argv)
		case "line":
			rec.Lines = append(rec.Lines, e.Text)
		}
	}
	return rec
}

// RunCodexAppServer runs a fake `codex app-server` on stdin and stdout,
// scripted by the CodexAppServerScript that CodexAppServer wrote under the
// directory named by codexAppServerDirEnv, appending its own argv and
// every line it receives there (record.jsonl). It returns the process's
// exit code: always 0, short of being unable to read back its own script
// or open its record file.
func RunCodexAppServer() int {
	dir := os.Getenv(codexAppServerDirEnv)
	data, err := os.ReadFile(filepath.Join(dir, "script.json"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake codex app-server:", err)
		return 1
	}
	var script CodexAppServerScript
	if err := json.Unmarshal(data, &script); err != nil {
		fmt.Fprintln(os.Stderr, "fake codex app-server:", err)
		return 1
	}
	rf, err := os.OpenFile(filepath.Join(dir, "record.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		fmt.Fprintln(os.Stderr, "fake codex app-server:", err)
		return 1
	}
	defer func() { _ = rf.Close() }()
	appendCodexRecord(rf, codexRecordEntry{Type: "start", Argv: os.Args})

	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024*1024)
	out := bufio.NewWriter(os.Stdout)

	readLine := func() (codexWireMessage, bool) {
		if !sc.Scan() {
			return codexWireMessage{}, false
		}
		line := sc.Text()
		appendCodexRecord(rf, codexRecordEntry{Type: "line", Text: line})
		var msg codexWireMessage
		_ = json.Unmarshal([]byte(line), &msg)
		return msg, true
	}
	write := func(fields map[string]any) {
		fields["jsonrpc"] = "2.0"
		b, err := json.Marshal(fields)
		if err != nil {
			return
		}
		_, _ = out.Write(b)
		_ = out.WriteByte('\n')
		_ = out.Flush()
	}
	respond := func(id json.RawMessage, result any, errv *CodexRPCError) {
		f := map[string]any{"id": id}
		if errv != nil {
			f["error"] = errv
		} else {
			f["result"] = orEmptyAny(result)
		}
		write(f)
	}
	notify := func(method string, params any) {
		f := map[string]any{"method": method}
		if params != nil {
			f["params"] = params
		}
		write(f)
	}
	n := 0
	request := func(method string, params any) string {
		n++
		id := fmt.Sprintf("fake-%d", n)
		f := map[string]any{"id": id, "method": method}
		if params != nil {
			f["params"] = params
		}
		write(f)
		return id
	}
	handleInterrupt := func(msg codexWireMessage) bool {
		if msg.Method != "turn/interrupt" {
			return false
		}
		if script.Interrupt != nil {
			respond(msg.ID, map[string]any{}, nil)
			notify("turn/completed", map[string]any{"status": script.Interrupt.Status})
		}
		return true
	}
	drain := func() {
		for {
			msg, ok := readLine()
			if !ok {
				return
			}
			handleInterrupt(msg)
		}
	}
	await := func(method string) (codexWireMessage, bool) {
		for {
			msg, ok := readLine()
			if !ok {
				return codexWireMessage{}, false
			}
			if handleInterrupt(msg) {
				continue
			}
			if msg.Method == method {
				return msg, true
			}
		}
	}

	msg, ok := await("initialize")
	if !ok {
		return 0
	}
	if script.ExitBefore == "initialize" {
		return 0
	}
	respond(msg.ID, script.InitializeResult, nil)

	msg, ok = await("config/read")
	if !ok {
		return 0
	}
	if script.ExitBefore == "config/read" {
		return 0
	}
	respond(msg.ID, script.ConfigResult, script.ConfigError)

	var threadMethod string
	for {
		msg, ok = readLine()
		if !ok {
			return 0
		}
		if handleInterrupt(msg) {
			continue
		}
		if msg.Method == "thread/start" || msg.Method == "thread/resume" {
			threadMethod = msg.Method
			break
		}
	}
	if script.ExitBefore == threadMethod {
		return 0
	}
	threadID, threadErr := script.ThreadID, script.ThreadError
	if threadMethod == "thread/resume" && (script.ResumeID != "" || script.ResumeError != nil) {
		threadID, threadErr = script.ResumeID, script.ResumeError
	}
	if threadErr != nil {
		respond(msg.ID, nil, threadErr)
		drain()
		return 0
	}
	respond(msg.ID, map[string]any{"thread": map[string]any{"id": threadID}}, nil)

	msg, ok = await("turn/start")
	if !ok {
		return 0
	}
	if script.ExitBefore == "turn/start" {
		return 0
	}
	respond(msg.ID, map[string]any{"turn": map[string]any{"id": script.TurnID}}, nil)

	for _, action := range script.Actions {
		switch {
		case action.Notify != nil:
			notify(action.Notify.Method, action.Notify.Params)
		case action.Request != nil:
			id := request(action.Request.Method, action.Request.Params)
			for {
				msg, ok = readLine()
				if !ok {
					return 0
				}
				if handleInterrupt(msg) {
					continue
				}
				var gotID string
				if json.Unmarshal(msg.ID, &gotID) == nil && gotID == id {
					break
				}
			}
		}
	}
	drain()
	return 0
}

// orEmptyAny is v, or an empty object when v is nil, so a successful
// response always carries a result.
func orEmptyAny(v any) any {
	if v == nil {
		return map[string]any{}
	}
	return v
}

func appendCodexRecord(f *os.File, e codexRecordEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	_, _ = f.Write(b)
	_, _ = f.Write([]byte("\n"))
}
