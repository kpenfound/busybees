package agent

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

// stdinConversationAgent is the name of the Backends entry
// withStdinConversationBackend borrows for the life of a test. Its
// descriptor already covers host, confined host, container and sbx
// (codexWritableTools), so the fake backend runs through every placement
// the stdinBackend interface must work on without a new agent name, a
// credential list, an SbxTemplates entry or a procs executable of its own.
const stdinConversationAgent = AgentCodex

// fakeStdinBackend is a test-only backend.command/consume implementation
// that exercises the optional stdinBackend interface (backend.go): command
// asks for no stdin of its own, and consumeStdin writes one request to the
// agent's stdin, reads its reply off stdout, and closes stdin to end the
// turn — answering the way a real stdinBackend implementation would answer
// a server's own requests while it reads and close stdin when its turn
// ends.
type fakeStdinBackend struct{}

func (fakeStdinBackend) command(_ context.Context, r *Runner, b Backend, _ Request, _ sessionPaths) (string, []string, string, []envVar, error) {
	return b.executable(r), nil, "", nil, nil
}

// consume is never called: the runner finds consumeStdin by type assertion
// first and calls that instead. It exists only so fakeStdinBackend
// satisfies backend, the interface Backend.impl is declared to hold.
func (fakeStdinBackend) consume(*Runner, io.Reader, io.Writer, *costMeter) (*streamEnd, *RateLimit, error) {
	panic("fakeStdinBackend: consume called; the runner must dispatch to stdinBackend.consumeStdin")
}

func (fakeStdinBackend) consumeStdin(r *Runner, stdin io.WriteCloser, stdout io.Reader, transcript io.Writer, cost *costMeter) (*streamEnd, *RateLimit, error) {
	if _, err := io.WriteString(stdin, `{"type":"request","text":"ping"}`+"\n"); err != nil {
		_ = stdin.Close()
		return nil, nil, err
	}
	var reply string
	err := r.tee(stdout, transcript, func(line []byte, typ string) {
		if typ != "reply" {
			return
		}
		var ev struct {
			Text string `json:"text"`
		}
		if jerr := json.Unmarshal(line, &ev); jerr == nil {
			reply = ev.Text
		}
		// The reply is the fake agent's turn ending; closing stdin now,
		// while its stream is still being read, is what lets the fake
		// agent's own blocking read of stdin see EOF and exit, the
		// contract consumeStdin follows: close stdin when the turn ends.
		_ = stdin.Close()
	})
	_ = stdin.Close()
	if err != nil {
		return nil, nil, err
	}
	return &streamEnd{SessionID: "fake-thread", Result: reply, Subtype: "success", NumTurns: 1}, nil, nil
}

// withStdinConversationBackend swaps stdinConversationAgent's
// implementation for fakeStdinBackend for the life of the test, and
// restores the original after: Backends is a package-level list every
// session reads, so the swap must not outlast the test that needs it. No
// test in this package runs in parallel, so the swap is never visible to
// another test's session.
func withStdinConversationBackend(t *testing.T) {
	t.Helper()
	for i := range Backends {
		if Backends[i].Name != stdinConversationAgent {
			continue
		}
		original := Backends[i].impl
		Backends[i].impl = fakeStdinBackend{}
		t.Cleanup(func() { Backends[i].impl = original })
		return
	}
	t.Fatalf("no backend named %q", stdinConversationAgent)
}

// stdinConversationScript is the fake agent fakeStdinBackend converses
// with: it reads one line off its stdin, the request, records it where the
// shell variable named runDirVar names a directory, replies on stdout, and
// then blocks reading its stdin until fakeStdinBackend closes it, which is
// when it exits.
func stdinConversationScript(t *testing.T, runDirVar string) string {
	t.Helper()
	body := `IFS= read -r line
printf '%s\n' "$line" > "$` + runDirVar + `/stdin-request.txt"
echo '{"type":"reply","text":"pong"}'
cat >/dev/null
`
	return agenttest.Script(t, stdinConversationAgent, body)
}

// checkStdinConversation holds what every placement's session must have
// produced: the fake agent saw the request fakeStdinBackend wrote to its
// stdin, the session completed with the runner's normal result (no
// error, the reply as the result text, the stream's session id), and the
// reply the fake agent printed is in the transcript, the runner's normal
// transcript handling for a backend it reads over stdout.
func checkStdinConversation(t *testing.T, res *Result, err error, runDir string) {
	t.Helper()
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.IsError || res.ResultText != "pong" || res.ClaudeID != "fake-thread" || res.NumTurns != 1 {
		t.Fatalf("result: %+v", res)
	}
	data, err := os.ReadFile(filepath.Join(runDir, "stdin-request.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(data)); got != `{"type":"request","text":"ping"}` {
		t.Errorf("request the fake agent read off its stdin = %q", got)
	}
	transcript, err := os.ReadFile(res.Transcript)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(transcript), `"reply"`) {
		t.Errorf("transcript missing the agent's reply: %s", transcript)
	}
}

// TestStdinBackendConversesOverStdio drives fakeStdinBackend through the
// runner on the host, inside the fake container engine and inside the fake
// sbx CLI: the runner finds the optional stdinBackend interface by type
// assertion and hands consumeStdin the session's stdin writer, on every
// placement, while the process group, the timeout, the transcript, the pid
// file, the outcome and the result stay the runner's own, exactly as for a
// backend that only reads stdout.
func TestStdinBackendConversesOverStdio(t *testing.T) {
	t.Run("host", func(t *testing.T) {
		withStdinConversationBackend(t)
		sessionDir, runDir := t.TempDir(), t.TempDir()
		bin := stdinConversationScript(t, "RUN_DIR")
		r := Runner{CodexBin: bin}
		req := grantAll(Request{SessionDir: sessionDir, Workspace: fakeWorkspace{dir: t.TempDir()}, Profile: Profile{Name: "custom", Agent: stdinConversationAgent}, Env: map[string]string{"RUN_DIR": runDir}})
		res, err := r.Run(context.Background(), req)
		checkStdinConversation(t, res, err, runDir)
	})

	t.Run("container", func(t *testing.T) {
		withStdinConversationBackend(t)
		sessionDir, runDir := t.TempDir(), t.TempDir()
		bin := stdinConversationScript(t, "RUN_DIR")
		r := Runner{CodexBin: bin, DockerBin: agenttest.Docker(t, "image", "RUN_DIR")}
		req := grantAll(Request{SessionDir: sessionDir, Workspace: fakeWorkspace{dir: t.TempDir()}, Profile: Profile{Name: "custom", Agent: stdinConversationAgent, Sandbox: SandboxContainer, SandboxImage: "image"}, Env: map[string]string{"RUN_DIR": runDir}})
		res, err := r.Run(context.Background(), req)
		checkStdinConversation(t, res, err, runDir)
	})

	t.Run("sbx", func(t *testing.T) {
		withStdinConversationBackend(t)
		sessionDir, runDir := t.TempDir(), t.TempDir()
		bin := stdinConversationScript(t, "RUN_DIR")
		r := Runner{CodexBin: bin, SbxBin: agenttest.Sbx(t, "RUN_DIR")}
		req := grantAll(Request{SessionDir: sessionDir, Workspace: fakeWorkspace{dir: t.TempDir()}, Profile: Profile{Name: "custom", Agent: stdinConversationAgent, Sandbox: SandboxSbx}, Env: map[string]string{"RUN_DIR": runDir}})
		res, err := r.Run(context.Background(), req)
		checkStdinConversation(t, res, err, runDir)
	})
}
