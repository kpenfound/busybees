package agent

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"reflect"
	"strings"
	"testing"
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
