package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/ghwork"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// bearer is an HTTP client that presents one bearer token, arriving the
// way a container's client does: with the host's alias as the Host header,
// not the loopback address it actually connects to.
type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	if b != "" {
		r.Header.Set("Authorization", "Bearer "+string(b))
	}
	r.Host = "host.docker.internal:4242"
	return http.DefaultTransport.RoundTrip(r)
}

// TestMCPListenModeServesSessionsOverHTTP drives the actual "bees mcp serve
// --listen" process, the command a container session's runner starts on the
// host (internal/session, HostMCP.ListenArgs), rather than calling the
// server's library functions in-process. It is the only test that exercises
// that CLI wiring: the --listen flag, $BEES_MCP_TOKEN, the printed listening
// line and the process's graceful shutdown. It replaces a prior version
// that built the server in-process and so could not catch a break in any of
// those (contract tracked in the unit report).
func TestMCPListenModeServesSessionsOverHTTP(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "bees")
	if out, err := exec.Command("go", "build", "-buildvcs=false", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	stateDir := t.TempDir()
	if err := state.New(stateDir).Init(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "mcp", "serve", "--listen", "127.0.0.1:0")
	cmd.Env = append(os.Environ(),
		session.EnvMCPToken+"=s3cret",
		session.EnvRole+"=developer",
		session.EnvStateDir+"="+stateDir,
		session.EnvSessionDir+"="+t.TempDir(),
		session.EnvIssue+"=3",
	)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })

	addr, err := waitForListenAddr(stdout, 10*time.Second)
	if err != nil {
		t.Fatalf("waiting for the listening address: %v\nstderr:\n%s", err, stderr.String())
	}
	url := "http://" + addr + "/mcp"

	for _, wrong := range []bearer{"", "guess"} {
		resp, err := (&http.Client{Transport: wrong}).Post(url, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
		if err != nil {
			t.Fatalf("token %q: %v\nstderr:\n%s", wrong, err, stderr.String())
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: got %d, want %d\nstderr:\n%s", wrong, resp.StatusCode, http.StatusUnauthorized, stderr.String())
		}
	}

	transport := &mcp.StreamableClientTransport{Endpoint: url, HTTPClient: &http.Client{Transport: bearer("s3cret")}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatalf("connect with the right token: %v\nstderr:\n%s", err, stderr.String())
	}
	defer func() { _ = cs.Close() }()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range tools.Tools {
		names = append(names, tl.Name)
	}
	for _, want := range []string{"done", "mail_send", "mail_list"} {
		if !slices.Contains(names, want) {
			t.Errorf("tools over --listen lack %s: %v", want, names)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "mail_send", Arguments: map[string]any{"to": "project_manager", "subject": "hi", "body": "over --listen"}})
	if err != nil || res.IsError {
		t.Fatalf("mail_send over --listen: %v %+v", err, res)
	}
	msgs, err := mail.Open(state.New(stateDir).MailDir()).List(mail.Filter{To: "project_manager"})
	if err != nil || len(msgs) != 1 || msgs[0].Body != "over --listen" || ghwork.Issue(msgs[0].Work) != 3 {
		t.Fatalf("mail written by the --listen server: %v %+v", err, msgs)
	}

	if err := cs.Close(); err != nil {
		t.Fatalf("client close: %v", err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	waitErr := make(chan error, 1)
	go func() { waitErr <- cmd.Wait() }()
	select {
	case err := <-waitErr:
		if err != nil {
			t.Fatalf("process did not shut down cleanly after SIGTERM: %v\nstderr:\n%s", err, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("process did not exit within 5s of SIGTERM\nstderr:\n%s", stderr.String())
	}
}

// waitForListenAddr reads lines from the server's stdout until it finds the
// one `serveMCPHTTP` prints once the listener is ready (session.MCPListening
// plus the address), or d elapses. It never sleeps a fixed amount: the
// process itself signals readiness by writing the line, and this blocks on
// that write rather than polling for one.
func waitForListenAddr(stdout io.Reader, d time.Duration) (string, error) {
	found := make(chan string, 1)
	go func() {
		// Keeps draining stdout after the match so the process's writes
		// never block on a reader that stopped looking.
		scanner := bufio.NewScanner(stdout)
		matched := false
		for scanner.Scan() {
			if !matched {
				if addr, ok := strings.CutPrefix(scanner.Text(), session.MCPListening); ok {
					matched = true
					found <- addr
				}
			}
		}
	}()
	select {
	case addr := <-found:
		return addr, nil
	case <-time.After(d):
		return "", fmt.Errorf("no %q line within %s", session.MCPListening, d)
	}
}
