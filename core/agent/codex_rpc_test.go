package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// codexFakeRPC is an in-process codex app-server double: it reads the
// driver's JSON-RPC lines off in and writes its own to out, one exchange
// at a time, so a test's script can play the server's half of one
// conversation without the fake binary (codex_rpc.go's driver is not yet
// called by codexBackend, which is what would otherwise need one).
type codexFakeRPC struct {
	t  *testing.T
	sc *bufio.Scanner
	w  io.Writer
}

func newCodexFakeRPC(t *testing.T, in io.Reader, out io.Writer) *codexFakeRPC {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 1024*1024), 64*1024*1024)
	return &codexFakeRPC{t: t, sc: sc, w: out}
}

// recv reads one line off the driver's stdin. false at its end: a test
// that expects the driver to have closed it checks for that directly, and
// one that does not treats false as the fake server's own bug, recorded
// with Errorf rather than Fatalf because this runs on its own goroutine.
func (f *codexFakeRPC) recv() (codexRPCLine, bool) {
	if !f.sc.Scan() {
		return codexRPCLine{}, false
	}
	var l codexRPCLine
	if err := json.Unmarshal(f.sc.Bytes(), &l); err != nil {
		f.t.Errorf("codex fake server: unmarshal %q: %v", f.sc.Text(), err)
		return codexRPCLine{}, false
	}
	return l, true
}

func (f *codexFakeRPC) send(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		f.t.Errorf("codex fake server: marshal: %v", err)
		return
	}
	_, _ = f.w.Write(append(data, '\n'))
}

func (f *codexFakeRPC) respond(id json.RawMessage, result any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func (f *codexFakeRPC) respondError(id json.RawMessage, code int, message string) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func (f *codexFakeRPC) notify(method string, params any) {
	v := map[string]any{"jsonrpc": "2.0", "method": method}
	if params != nil {
		v["params"] = params
	}
	f.send(v)
}

func (f *codexFakeRPC) request(id int, method string, params any) {
	f.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
}

// runCodexFakeConversation drives turn through codexRPCRun against a fake
// server playing script, and returns the outcome, the error and the
// complete transcript.
func runCodexFakeConversation(t *testing.T, turn codexRPCTurn, script func(f *codexFakeRPC)) (*codexRPCOutcome, error, string) {
	t.Helper()
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f := newCodexFakeRPC(t, stdinR, stdoutW)
		script(f)
		_ = stdoutW.Close()
		_ = stdinR.Close()
	}()
	var transcript bytes.Buffer
	out, err := codexRPCRun(stdinW, stdoutR, &transcript, turn)
	<-done
	return out, err, transcript.String()
}

// TestCodexThreadStartParams asserts the sandbox/approval table (spec#5):
// an ordinary turn or one granted ToolsAll (Tools nil) and one granted
// apply_patch get danger-full-access, a held turn without apply_patch and
// a restricted turn get read-only, every row gets approvalPolicy "never",
// developerInstructions holding the system prompt and neither ephemeral
// nor approvalsReviewer, model is present only when one is named, and
// thread/resume carries exactly the same parameters as thread/start, plus
// threadId.
func TestCodexThreadStartParams(t *testing.T) {
	const prompt = "be careful, and read before you write"
	cases := []struct {
		name       string
		restricted bool
		tools      []string
		sandbox    string
	}{
		{"ordinary or granted ToolsAll", false, nil, "danger-full-access"},
		{"granted apply_patch", false, []string{"apply_patch", "shell"}, "danger-full-access"},
		{"held without apply_patch", false, []string{"web_search"}, "read-only"},
		{"restricted", true, []string{"read_file"}, "read-only"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := codexThreadStartParams("/work", "", prompt, c.restricted, c.tools)
			if p.Sandbox != c.sandbox {
				t.Errorf("sandbox = %q, want %q", p.Sandbox, c.sandbox)
			}
			if p.ApprovalPolicy != "never" {
				t.Errorf("approvalPolicy = %q, want %q", p.ApprovalPolicy, "never")
			}
			if p.DeveloperInstructions != prompt {
				t.Errorf("developerInstructions = %q, want %q", p.DeveloperInstructions, prompt)
			}
			if p.Cwd != "/work" {
				t.Errorf("cwd = %q, want /work", p.Cwd)
			}
			if p.Model != "" {
				t.Errorf("model = %q, want empty: the profile named none", p.Model)
			}
			data, err := json.Marshal(p)
			if err != nil {
				t.Fatal(err)
			}
			for _, absent := range []string{"ephemeral", "approvalsReviewer", `"model"`} {
				if strings.Contains(string(data), absent) {
					t.Errorf("params %s must not contain %q", data, absent)
				}
			}

			named := codexThreadStartParams("/work", "gpt-7", prompt, c.restricted, c.tools)
			if named.Model != "gpt-7" {
				t.Errorf("model = %q, want gpt-7 when the profile names one", named.Model)
			}

			startMethod, startParams := codexThreadMethod("", p)
			resumeMethod, resumeParams := codexThreadMethod("resume-1", p)
			if startMethod != "thread/start" {
				t.Errorf("method = %q, want thread/start", startMethod)
			}
			if resumeMethod != "thread/resume" {
				t.Errorf("method = %q, want thread/resume", resumeMethod)
			}
			startData, err := json.Marshal(startParams)
			if err != nil {
				t.Fatal(err)
			}
			resumeData, err := json.Marshal(resumeParams)
			if err != nil {
				t.Fatal(err)
			}
			var startMap, resumeMap map[string]any
			if err := json.Unmarshal(startData, &startMap); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(resumeData, &resumeMap); err != nil {
				t.Fatal(err)
			}
			delete(resumeMap, "threadId")
			if !reflect.DeepEqual(startMap, resumeMap) {
				t.Errorf("thread/resume params = %v, want the same as thread/start's %v", resumeMap, startMap)
			}
			var resumed struct {
				ThreadID string `json:"threadId"`
			}
			if err := json.Unmarshal(resumeData, &resumed); err != nil {
				t.Fatal(err)
			}
			if resumed.ThreadID != "resume-1" {
				t.Errorf("threadId = %q, want resume-1", resumed.ThreadID)
			}
		})
	}
}

// TestCodexRPCConversation drives a whole conversation through
// codexRPCRun: the order initialize → initialized → config/read →
// thread/start → turn/start, stdin closed only after turn/completed, no
// system prompt written outside a JSON-RPC message, the session id from
// thread.id, every item/completed counted as a turn, the last agent
// message as the result, "success" from turn/completed's "completed"
// status, config/read's answer returned unexamined, and every line the
// server sent and every request and response the driver sent recorded in
// the transcript (spec#5, #8, #9).
func TestCodexRPCConversation(t *testing.T) {
	const systemPrompt = "be careful, and read before you write"
	var methods []string

	turn := codexRPCTurn{Cwd: "/work", SystemPrompt: systemPrompt, Prompt: "fix the bug", ConfigDir: "/work"}
	out, err, transcript := runCodexFakeConversation(t, turn, func(f *codexFakeRPC) {
		l, _ := f.recv()
		methods = append(methods, l.Method)
		f.respond(l.ID, map[string]any{})

		l, _ = f.recv()
		methods = append(methods, l.Method)

		l, _ = f.recv()
		methods = append(methods, l.Method)
		f.respond(l.ID, map[string]any{"config": map[string]any{"approval_policy": "never"}, "origins": map[string]any{}})

		l, _ = f.recv()
		methods = append(methods, l.Method)
		f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "thread-abc"}})

		l, _ = f.recv()
		methods = append(methods, l.Method)
		f.respond(l.ID, map[string]any{})

		f.notify("thread/tokenUsage/updated", map[string]any{"inputTokens": 10})
		f.notify("item/completed", map[string]any{"item": map[string]any{"type": "agent_message", "text": "first"}})
		f.notify("item/completed", map[string]any{"item": map[string]any{"type": "command_execution"}})
		f.notify("item/completed", map[string]any{"item": map[string]any{"type": "agent_message", "text": "final answer"}})
		f.notify("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})

		if _, ok := f.recv(); ok {
			t.Error("the driver's stdin was readable after turn/completed; it must be closed")
		}
	})
	if err != nil {
		t.Fatalf("codexRPCRun: %v", err)
	}

	wantMethods := []string{"initialize", "initialized", "config/read", "thread/start", "turn/start"}
	if !reflect.DeepEqual(methods, wantMethods) {
		t.Errorf("method order = %v, want %v", methods, wantMethods)
	}

	if out.SessionID != "thread-abc" {
		t.Errorf("SessionID = %q, want thread-abc", out.SessionID)
	}
	if out.NumTurns != 3 {
		t.Errorf("NumTurns = %d, want 3", out.NumTurns)
	}
	if out.Result != "final answer" {
		t.Errorf("Result = %q, want %q", out.Result, "final answer")
	}
	if out.Subtype != "success" {
		t.Errorf("Subtype = %q, want success", out.Subtype)
	}
	if !strings.Contains(string(out.Config), "approval_policy") {
		t.Errorf("Config = %s, want config/read's answer passed through", out.Config)
	}

	foundPrompt := false
	for _, line := range strings.Split(strings.TrimRight(transcript, "\n"), "\n") {
		if !json.Valid([]byte(line)) {
			t.Errorf("transcript line is not a JSON-RPC message: %s", line)
			continue
		}
		if strings.Contains(line, systemPrompt) {
			foundPrompt = true
			if !strings.Contains(line, "developerInstructions") {
				t.Errorf("system prompt appears outside developerInstructions: %s", line)
			}
		}
	}
	if !foundPrompt {
		t.Error("the system prompt never appears in the transcript")
	}
	if !strings.Contains(transcript, "thread/tokenUsage/updated") {
		t.Error("transcript is missing the thread/tokenUsage/updated notification")
	}
	if !strings.Contains(transcript, "turn/start") || !strings.Contains(transcript, "thread-abc") {
		t.Error("transcript is missing a request or response the driver sent")
	}
}

// TestCodexRPCResume asserts thread/resume is used, with the same
// parameters thread/start would carry, when ResumeID is set.
func TestCodexRPCResume(t *testing.T) {
	var gotMethod string
	var gotThreadID string
	turn := codexRPCTurn{Cwd: "/work", Prompt: "go on", ResumeID: "earlier-thread"}
	out, err, _ := runCodexFakeConversation(t, turn, func(f *codexFakeRPC) {
		l, _ := f.recv()
		f.respond(l.ID, map[string]any{})
		f.recv()
		l, _ = f.recv()
		gotMethod = l.Method
		var p struct {
			ThreadID string `json:"threadId"`
		}
		_ = json.Unmarshal(l.Params, &p)
		gotThreadID = p.ThreadID
		f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "earlier-thread"}})
		l, _ = f.recv()
		f.respond(l.ID, map[string]any{})
		f.notify("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
		f.recv()
	})
	if err != nil {
		t.Fatalf("codexRPCRun: %v", err)
	}
	if gotMethod != "thread/resume" {
		t.Errorf("method = %q, want thread/resume", gotMethod)
	}
	if gotThreadID != "earlier-thread" {
		t.Errorf("threadId = %q, want earlier-thread", gotThreadID)
	}
	if out.SessionID != "earlier-thread" {
		t.Errorf("SessionID = %q, want earlier-thread", out.SessionID)
	}
}

// TestCodexRPCAnswersServerRequests sends the driver a request while it is
// awaiting the response to its own thread/start request: the driver must
// answer it right there, with a JSON-RPC error naming it unsupported,
// without leaving it pending, and go on to finish the turn normally.
func TestCodexRPCAnswersServerRequests(t *testing.T) {
	var answer codexRPCLine
	turn := codexRPCTurn{Cwd: "/work", Prompt: "do it"}
	out, err, transcript := runCodexFakeConversation(t, turn, func(f *codexFakeRPC) {
		l, _ := f.recv()
		f.respond(l.ID, map[string]any{})
		f.recv()

		l, _ = f.recv() // thread/start, not yet answered
		f.request(100, "item/commandExecution/requestApproval", map[string]any{"command": "rm -rf /"})
		answer, _ = f.recv()
		f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "t1"}})

		l, _ = f.recv()
		f.respond(l.ID, map[string]any{})
		f.notify("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
		f.recv()
	})
	if err != nil {
		t.Fatalf("codexRPCRun: %v", err)
	}
	if out.SessionID != "t1" {
		t.Fatalf("session did not finish normally: %+v", out)
	}
	if string(answer.ID) != "100" {
		t.Errorf("answer id = %s, want 100", answer.ID)
	}
	if answer.Error == nil {
		t.Fatalf("answer = %+v, want a JSON-RPC error", answer)
	}
	const want = "item/commandExecution/requestApproval is not supported in a bees session"
	if answer.Error.Message != want {
		t.Errorf("answer error message = %q, want %q", answer.Error.Message, want)
	}
	if !strings.Contains(transcript, "item/commandExecution/requestApproval") {
		t.Error("transcript is missing the server's request")
	}
	if !strings.Contains(transcript, want) {
		t.Error("transcript is missing the driver's answer")
	}
}

// TestCodexRPCServerRequestsDuringTurn sends the driver one
// mcpServer/elicitation/request, an approval request,
// item/tool/requestUserInput and an unknown method while turn/completed is
// awaited, and checks each is answered as spec#7 requires — a cancelling
// result for the elicitation, a JSON-RPC error naming the matching id for
// every other one — and that the conversation still reaches turn/completed
// without hanging.
func TestCodexRPCServerRequestsDuringTurn(t *testing.T) {
	answers := map[string]codexRPCLine{}
	turn := codexRPCTurn{Cwd: "/work", Prompt: "do it"}
	requests := []struct {
		id     int
		method string
	}{
		{1, "mcpServer/elicitation/request"},
		{2, "item/commandExecution/requestApproval"},
		{3, "item/tool/requestUserInput"},
		{4, "some/unknown/method"},
	}
	out, err, _ := runCodexFakeConversation(t, turn, func(f *codexFakeRPC) {
		l, _ := f.recv()
		f.respond(l.ID, map[string]any{})
		f.recv()
		l, _ = f.recv()
		f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "t1"}})
		l, _ = f.recv()
		f.respond(l.ID, map[string]any{})

		for _, r := range requests {
			f.request(r.id, r.method, map[string]any{})
			a, _ := f.recv()
			answers[r.method] = a
		}
		f.notify("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
		f.recv()
	})
	if err != nil {
		t.Fatalf("codexRPCRun: %v", err)
	}
	if out.SessionID != "t1" {
		t.Fatalf("session did not finish normally: %+v", out)
	}

	elicitation := answers["mcpServer/elicitation/request"]
	if string(elicitation.ID) != "1" {
		t.Errorf("elicitation answer id = %s, want 1", elicitation.ID)
	}
	if elicitation.Error != nil {
		t.Errorf("elicitation answer = %+v, want a result, not an error", elicitation)
	}
	var result struct {
		Action string `json:"action"`
	}
	if err := json.Unmarshal(elicitation.Result, &result); err != nil {
		t.Fatalf("elicitation result: %v", err)
	}
	if result.Action != "cancel" {
		t.Errorf("elicitation result action = %q, want cancel", result.Action)
	}

	for _, r := range requests[1:] {
		a := answers[r.method]
		if string(a.ID) != strconv.Itoa(r.id) {
			t.Errorf("%s answer id = %s, want %d", r.method, a.ID, r.id)
		}
		if a.Error == nil {
			t.Errorf("%s answer = %+v, want a JSON-RPC error", r.method, a)
			continue
		}
		want := r.method + " is not supported in a bees session"
		if a.Error.Message != want {
			t.Errorf("%s answer error message = %q, want %q", r.method, a.Error.Message, want)
		}
	}
}

// TestCodexRPCServerRequestDuringTurnStart sends the driver a server
// request while it awaits turn/start's response, and checks it is answered
// right there, before the turn proceeds, without leaving it pending.
func TestCodexRPCServerRequestDuringTurnStart(t *testing.T) {
	var answer codexRPCLine
	turn := codexRPCTurn{Cwd: "/work", Prompt: "do it"}
	out, err, _ := runCodexFakeConversation(t, turn, func(f *codexFakeRPC) {
		l, _ := f.recv()
		f.respond(l.ID, map[string]any{})
		f.recv()
		l, _ = f.recv()
		f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "t1"}})

		l, _ = f.recv() // turn/start, not yet answered
		f.request(200, "item/fileChange/requestApproval", map[string]any{"path": "a.txt"})
		answer, _ = f.recv()
		f.respond(l.ID, map[string]any{})

		f.notify("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
		f.recv()
	})
	if err != nil {
		t.Fatalf("codexRPCRun: %v", err)
	}
	if out.SessionID != "t1" {
		t.Fatalf("session did not finish normally: %+v", out)
	}
	if string(answer.ID) != "200" {
		t.Errorf("answer id = %s, want 200", answer.ID)
	}
	if answer.Error == nil {
		t.Fatalf("answer = %+v, want a JSON-RPC error", answer)
	}
	const want = "item/fileChange/requestApproval is not supported in a bees session"
	if answer.Error.Message != want {
		t.Errorf("answer error message = %q, want %q", answer.Error.Message, want)
	}
}

// TestCodexAnswerClosedRule is a table test over every method the task
// names — known approval and input requests, the elicitation, and an
// unknown method — asserting the closed rule: only
// mcpServer/elicitation/request gets a non-error answer, and every other
// one, named or not, gets a JSON-RPC error naming it unsupported. Nothing
// approves anything.
func TestCodexAnswerClosedRule(t *testing.T) {
	methods := []string{
		"item/commandExecution/requestApproval",
		"item/fileChange/requestApproval",
		"item/permissions/requestApproval",
		"execCommandApproval",
		"applyPatchApproval",
		"item/tool/requestUserInput",
		"item/tool/call",
		"account/chatgptAuthTokens/refresh",
		"attestation/generate",
		"currentTime/read",
		"some/unknown/method",
	}
	for _, method := range methods {
		t.Run(method, func(t *testing.T) {
			c := newCodexConversation(io.Discard, strings.NewReader(""), io.Discard)
			var sent map[string]any
			c.stdin = &capturingWriter{on: func(data []byte) {
				_ = json.Unmarshal(data, &sent)
			}}
			id := json.RawMessage(`7`)
			if err := c.answer(codexRPCLine{ID: id, Method: method}); err != nil {
				t.Fatalf("answer: %v", err)
			}
			if _, ok := sent["result"]; ok {
				t.Errorf("%s got a result, want only mcpServer/elicitation/request to", method)
			}
			errField, ok := sent["error"]
			if !ok {
				t.Fatalf("%s got no error field", method)
			}
			errMap, ok := errField.(map[string]any)
			if !ok {
				t.Fatalf("%s error field = %#v, want an object", method, errField)
			}
			want := method + " is not supported in a bees session"
			if errMap["message"] != want {
				t.Errorf("%s error message = %v, want %q", method, errMap["message"], want)
			}
		})
	}

	t.Run(codexElicitationMethod, func(t *testing.T) {
		c := newCodexConversation(io.Discard, strings.NewReader(""), io.Discard)
		var sent map[string]any
		c.stdin = &capturingWriter{on: func(data []byte) {
			_ = json.Unmarshal(data, &sent)
		}}
		id := json.RawMessage(`9`)
		if err := c.answer(codexRPCLine{ID: id, Method: codexElicitationMethod}); err != nil {
			t.Fatalf("answer: %v", err)
		}
		if _, ok := sent["error"]; ok {
			t.Errorf("%s got an error, want a result", codexElicitationMethod)
		}
		result, ok := sent["result"].(map[string]any)
		if !ok {
			t.Fatalf("%s result = %#v, want an object", codexElicitationMethod, sent["result"])
		}
		if result["action"] != "cancel" {
			t.Errorf("%s result action = %v, want cancel", codexElicitationMethod, result["action"])
		}
	})
}

// capturingWriter calls on with every write it receives, for tests that
// inspect what a conversation sent without a real pipe.
type capturingWriter struct {
	on func([]byte)
}

func (w *capturingWriter) Write(p []byte) (int, error) {
	w.on(p)
	return len(p), nil
}

// TestCodexRPCErrorEndings checks every ending spec#9 names: an "error"
// notification, a failed turn, and an error response to thread/start,
// thread/resume or turn/start.
func TestCodexRPCErrorEndings(t *testing.T) {
	initializeAnd := func(f *codexFakeRPC) {
		l, _ := f.recv()
		f.respond(l.ID, map[string]any{})
		f.recv()
	}

	cases := []struct {
		name   string
		turn   codexRPCTurn
		script func(f *codexFakeRPC)
		want   string
	}{
		{
			name: "error notification",
			turn: codexRPCTurn{Cwd: "/w", Prompt: "p"},
			script: func(f *codexFakeRPC) {
				initializeAnd(f)
				l, _ := f.recv()
				f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "t1"}})
				l, _ = f.recv()
				f.respond(l.ID, map[string]any{})
				f.notify("error", map[string]any{"message": "codex blew up"})
			},
			want: "codex blew up",
		},
		{
			name: "failed turn",
			turn: codexRPCTurn{Cwd: "/w", Prompt: "p"},
			script: func(f *codexFakeRPC) {
				initializeAnd(f)
				l, _ := f.recv()
				f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "t1"}})
				l, _ = f.recv()
				f.respond(l.ID, map[string]any{})
				f.notify("turn/completed", map[string]any{"turn": map[string]any{"status": "failed"}})
			},
			want: "failed",
		},
		{
			name: "error response to thread/start",
			turn: codexRPCTurn{Cwd: "/w", Prompt: "p"},
			script: func(f *codexFakeRPC) {
				initializeAnd(f)
				l, _ := f.recv()
				f.respondError(l.ID, -32000, "thread/start blew up")
			},
			want: "thread/start blew up",
		},
		{
			name: "error response to thread/resume",
			turn: codexRPCTurn{Cwd: "/w", Prompt: "p", ResumeID: "gone"},
			script: func(f *codexFakeRPC) {
				initializeAnd(f)
				l, _ := f.recv()
				f.respondError(l.ID, -32600, "no rollout found for thread id gone")
			},
			want: "no rollout found for thread id gone",
		},
		{
			name: "error response to turn/start",
			turn: codexRPCTurn{Cwd: "/w", Prompt: "p"},
			script: func(f *codexFakeRPC) {
				initializeAnd(f)
				l, _ := f.recv()
				f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "t1"}})
				l, _ = f.recv()
				f.respondError(l.ID, -32000, "turn/start blew up")
			},
			want: "turn/start blew up",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err, _ := runCodexFakeConversation(t, c.turn, c.script)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}

// TestCodexRPCRateLimitUpdated asserts codexRPCRun folds the latest
// account/rateLimits/updated notification into the outcome's RateLimit: a
// set rateLimitReachedType blocks, with Type and ResetsAt naming whichever
// window is at 100% usedPercent (secondary and primary each checked), and
// a null one yields the allowed snapshot, with a later notification
// overriding an earlier one.
func TestCodexRPCRateLimitUpdated(t *testing.T) {
	cases := []struct {
		name       string
		notify     map[string]any
		wantStatus string
		wantType   string
		wantResets int64
	}{
		{
			name: "secondary window at 100%",
			notify: map[string]any{
				"rateLimitReachedType": "secondary",
				"primary":              map[string]any{"usedPercent": 42, "resetsAt": 1700000000},
				"secondary":            map[string]any{"usedPercent": 100, "resetsAt": 1700003600},
			},
			wantStatus: "secondary",
			wantType:   "secondary",
			wantResets: 1700003600,
		},
		{
			name: "primary window at 100%",
			notify: map[string]any{
				"rateLimitReachedType": "primary",
				"primary":              map[string]any{"usedPercent": 100, "resetsAt": 1700007200},
			},
			wantStatus: "primary",
			wantType:   "primary",
			wantResets: 1700007200,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			turn := codexRPCTurn{Cwd: "/work", Prompt: "do it"}
			out, err, _ := runCodexFakeConversation(t, turn, func(f *codexFakeRPC) {
				l, _ := f.recv()
				f.respond(l.ID, map[string]any{})
				f.recv()
				l, _ = f.recv()
				f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "t1"}})
				l, _ = f.recv()
				f.respond(l.ID, map[string]any{})

				f.notify("account/rateLimits/updated", map[string]any{"rateLimitReachedType": nil})
				f.notify("account/rateLimits/updated", c.notify)
				f.notify("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
				f.recv()
			})
			if err != nil {
				t.Fatalf("codexRPCRun: %v", err)
			}
			if out.RateLimit == nil {
				t.Fatal("RateLimit is nil, want one built from the notification")
			}
			if out.RateLimit.Status != c.wantStatus {
				t.Errorf("Status = %q, want %q", out.RateLimit.Status, c.wantStatus)
			}
			if out.RateLimit.Type != c.wantType {
				t.Errorf("Type = %q, want %q", out.RateLimit.Type, c.wantType)
			}
			if !out.RateLimit.ResetsAt.Equal(time.Unix(c.wantResets, 0)) {
				t.Errorf("ResetsAt = %s, want %s", out.RateLimit.ResetsAt, time.Unix(c.wantResets, 0))
			}
		})
	}

	t.Run("rateLimitReachedType null", func(t *testing.T) {
		turn := codexRPCTurn{Cwd: "/work", Prompt: "do it"}
		out, err, _ := runCodexFakeConversation(t, turn, func(f *codexFakeRPC) {
			l, _ := f.recv()
			f.respond(l.ID, map[string]any{})
			f.recv()
			l, _ = f.recv()
			f.respond(l.ID, map[string]any{"thread": map[string]any{"id": "t1"}})
			l, _ = f.recv()
			f.respond(l.ID, map[string]any{})

			f.notify("account/rateLimits/updated", map[string]any{"rateLimitReachedType": nil})
			f.notify("turn/completed", map[string]any{"turn": map[string]any{"status": "completed"}})
			f.recv()
		})
		if err != nil {
			t.Fatalf("codexRPCRun: %v", err)
		}
		if out.RateLimit == nil || out.RateLimit.Status != "allowed" {
			t.Errorf("RateLimit = %+v, want the allowed snapshot", out.RateLimit)
		}
	})
}

// TestCodexRPCAppServerEndsDeterministically checks that a server which
// exits or closes its stdout before answering yields the identical error
// whether that shows up as a failed write to its stdin or a read that
// finds stdout already at its end.
func TestCodexRPCAppServerEndsDeterministically(t *testing.T) {
	const want = "the app server ended without answering initialize"

	t.Run("write fails", func(t *testing.T) {
		stdinR, stdinW := io.Pipe()
		if err := stdinR.Close(); err != nil {
			t.Fatal(err)
		}
		stdoutR, _ := io.Pipe()
		var transcript bytes.Buffer
		_, err := codexRPCRun(stdinW, stdoutR, &transcript, codexRPCTurn{Cwd: "/w", Prompt: "p"})
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})

	t.Run("read fails", func(t *testing.T) {
		stdinR, stdinW := io.Pipe()
		stdoutR, stdoutW := io.Pipe()
		done := make(chan struct{})
		go func() {
			defer close(done)
			f := newCodexFakeRPC(t, stdinR, stdoutW)
			f.recv() // reads "initialize" fully, so the driver's write succeeds
			_ = stdoutW.Close()
			_ = stdinR.Close()
		}()
		var transcript bytes.Buffer
		_, err := codexRPCRun(stdinW, stdoutR, &transcript, codexRPCTurn{Cwd: "/w", Prompt: "p"})
		<-done
		if err == nil || err.Error() != want {
			t.Fatalf("err = %v, want %q", err, want)
		}
	})
}
