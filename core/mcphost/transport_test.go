package mcphost_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/mcphost"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type bearer string

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	r.Host = "container-host:4242"
	return http.DefaultTransport.RoundTrip(r)
}

func TestTransportsShareRegistry(t *testing.T) {
	r := mcphost.NewRegistry([]string{"pilot"}, mcphost.RejectRole, mcphost.RejectRole)
	mcphost.AddTool(r, &mcp.Tool{Name: "echo", InputSchema: mcphost.SchemaFor[echoInput](map[string][]string{"value": {"hello"}})}, echo, "pilot")
	baseline, err := mcphost.Tools(context.Background(), server(t, r, "pilot"), clientInfo)
	if err != nil {
		t.Fatal(err)
	}
	for _, transport := range []string{"memory", "stdio", "http"} {
		t.Run(transport, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			srv := server(t, r, "pilot")
			var c *mcp.ClientSession
			switch transport {
			case "memory":
				c = connect(t, srv)
			case "stdio":
				// Actual StdioTransport over OS pipes, without spawning a process.
				inR, inW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				outR, outW, err := os.Pipe()
				if err != nil {
					t.Fatal(err)
				}
				oldIn, oldOut := os.Stdin, os.Stdout
				os.Stdin, os.Stdout = inR, outW
				defer func() { os.Stdin, os.Stdout = oldIn, oldOut }()
				defer func() { _ = inR.Close(); _ = inW.Close(); _ = outR.Close(); _ = outW.Close() }()
				finished := make(chan error, 1)
				go func() { finished <- mcphost.Run(ctx, srv, &mcp.StdioTransport{}) }()
				c, err = mcp.NewClient(&clientInfo, nil).Connect(ctx, &mcp.IOTransport{Reader: outR, Writer: inW}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					_ = c.Close()
					select {
					case err := <-finished:
						if err != nil {
							t.Errorf("stdio closure: %v", err)
						}
					case <-ctx.Done():
						t.Error("stdio did not stop")
					}
				}()
			case "http":
				ln, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				finished := make(chan error, 1)
				go func() { finished <- mcphost.ServeHTTP(ctx, srv, ln, "secret") }()
				c, err = mcp.NewClient(&clientInfo, nil).Connect(ctx, &mcp.StreamableClientTransport{
					Endpoint: "http://" + ln.Addr().String() + "/mcp", HTTPClient: &http.Client{Transport: bearer("secret")},
				}, nil)
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					_ = c.Close()
					cancel()
					select {
					case err := <-finished:
						if err != nil {
							t.Errorf("HTTP shutdown: %v", err)
						}
					case <-time.After(5 * time.Second):
						t.Error("HTTP did not stop")
					}
				}()
			}
			got, err := c.ListTools(ctx, nil)
			if err != nil || !reflect.DeepEqual(got.Tools, baseline) {
				t.Fatalf("transport schema mismatch: %v %+v", err, got)
			}
			res, err := c.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: echoInput{Value: "hello"}})
			if err != nil || res.IsError {
				t.Fatalf("dispatch: %v %+v", err, res)
			}
			res, err = c.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: echoInput{Value: "invalid"}})
			if err != nil || !res.IsError {
				t.Fatalf("schema validation: %v %+v", err, res)
			}
		})
	}
}

func TestHTTPRejectsMissingAndWrongTokens(t *testing.T) {
	r := mcphost.NewRegistry(nil, mcphost.AllTools, mcphost.AllTools)
	for _, token := range []string{"secret", ""} {
		for _, header := range []string{"", "Bearer wrong", "Basic secret", "Bearer "} {
			req := httptest.NewRequest(http.MethodPost, "http://container-host/mcp", strings.NewReader(`{}`))
			req.Header.Set("Authorization", header)
			w := httptest.NewRecorder()
			mcphost.HTTPHandler(server(t, r, ""), token).ServeHTTP(w, req)
			if w.Code != http.StatusUnauthorized {
				t.Fatalf("token %q header %q: %d", token, header, w.Code)
			}
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := mcphost.ServeHTTP(context.Background(), server(t, r, ""), ln, ""); err == nil {
		t.Fatal("empty token accepted")
	}
	if err := ln.Close(); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("listener not closed: %v", err)
	}
}

func TestCleanShutdownClassification(t *testing.T) {
	for _, err := range []error{nil, io.EOF, context.Canceled, &jsonrpc.Error{Code: -32004}} {
		if !mcphost.IsCleanShutdown(err) {
			t.Errorf("normal closure: %v", err)
		}
	}
	if mcphost.IsCleanShutdown(errors.New("failed")) {
		t.Fatal("real failure suppressed")
	}
}
