package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/mcpserver"
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

// The HTTP server a container session talks to serves the same tools the
// stdio one does, to a client that presents the session's token, and
// refuses everyone else before a request reaches the server.
func TestMCPOverHTTPNeedsTheToken(t *testing.T) {
	st := state.New(t.TempDir())
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	env := mcpserver.Env{Role: "developer", StateDir: st.Dir, SessionDir: t.TempDir(), Issue: 3}
	ts := httptest.NewServer(mcpHTTPHandler(mcpserver.New(env, mcpserver.Deps{}), "s3cret"))
	defer ts.Close()
	ctx := context.Background()

	for _, wrong := range []bearer{"", "guess"} {
		resp, err := (&http.Client{Transport: wrong}).Post(ts.URL+"/mcp", "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("token %q: got %d, want 401", wrong, resp.StatusCode)
		}
	}

	transport := &mcp.StreamableClientTransport{Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer("s3cret")}}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t", Version: "0"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
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
			t.Errorf("tools over HTTP lack %s: %v", want, names)
		}
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "mail_send", Arguments: map[string]any{"to": "project_manager", "subject": "hi", "body": "over http"}})
	if err != nil || res.IsError {
		t.Fatalf("mail_send over HTTP: %v %+v", err, res)
	}
	msgs, err := mail.Open(st.MailDir()).List(mail.Filter{To: "project_manager"})
	if err != nil || len(msgs) != 1 || msgs[0].Body != "over http" || msgs[0].Issue != 3 {
		t.Fatalf("mail written by the HTTP server: %v %+v", err, msgs)
	}
}
