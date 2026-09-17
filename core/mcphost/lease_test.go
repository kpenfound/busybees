package mcphost_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/mcphost"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// turnRegistry gives each of two roles a tool the other cannot see.
func turnRegistry() *mcphost.Registry {
	r := mcphost.NewRegistry([]string{"pilot", "copilot"}, mcphost.RejectRole, mcphost.RejectRole)
	mcphost.AddTool(r, &mcp.Tool{Name: "pilot_echo"}, echo, "pilot")
	mcphost.AddTool(r, &mcp.Tool{Name: "copilot_echo"}, echo, "copilot")
	return r
}

// dial connects an agent's client the way its configuration would: the URL
// and the token, nothing from the lease.
func dial(ctx context.Context, url, token string) (*mcp.ClientSession, error) {
	return mcp.NewClient(&clientInfo, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: url, HTTPClient: &http.Client{Transport: mcphost.BearerTransport(token, nil)},
	}, nil)
}

func toolNames(t *testing.T, ctx context.Context, c *mcp.ClientSession) []string {
	t.Helper()
	res, err := c.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
	}
	return names
}

func callEcho(ctx context.Context, c *mcp.ClientSession, tool string) error {
	res, err := c.CallTool(ctx, &mcp.CallToolParams{Name: tool, Arguments: echoInput{Value: "hello"}})
	if err != nil {
		return err
	}
	if res.IsError {
		return errors.New("tool error")
	}
	return nil
}

func status(t *testing.T, url, token string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func TestStartServesOneTurnUntilTheLeaseCloses(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ep, lease, err := mcphost.Start(ctx, server(t, turnRegistry(), "pilot"))
	if err != nil {
		t.Fatal(err)
	}
	if ep.Host != "127.0.0.1" || ep.Port == "" || ep.Token == "" || ep.URL != "http://127.0.0.1:"+ep.Port+"/mcp" {
		t.Fatalf("endpoint %+v", ep)
	}
	if got := ep.Via("host.docker.internal"); got != "http://host.docker.internal:"+ep.Port+"/mcp" {
		t.Fatalf("Via: %s", got)
	}
	// A container reaches the listener under another host name.
	c, err := dial(ctx, ep.Via("localhost"), ep.Token)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, ctx, c); !slices.Equal(got, []string{"pilot_echo"}) {
		t.Fatalf("tools %v", got)
	}
	if err := callEcho(ctx, c, "pilot_echo"); err != nil {
		t.Fatalf("call through the endpoint: %v", err)
	}
	viaLease, err := lease.Connect(ctx, clientInfo)
	if err != nil {
		t.Fatal(err)
	}
	if err := callEcho(ctx, viaLease, "pilot_echo"); err != nil {
		t.Fatalf("call through the lease: %v", err)
	}

	closed := make(chan error, 1)
	go func() { closed <- lease.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatalf("close: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if err := callEcho(ctx, c, "pilot_echo"); err == nil {
		t.Fatal("a call succeeded after the lease closed")
	}
	if _, err := lease.Connect(ctx, clientInfo); !errors.Is(err, mcphost.ErrLeaseClosed) {
		t.Fatalf("connect after close: %v", err)
	}
	if conn, err := net.Dial("tcp", net.JoinHostPort(ep.Host, ep.Port)); err == nil {
		_ = conn.Close()
		t.Fatal("the endpoint still accepts connections")
	}
	ln, err := net.Listen("tcp", net.JoinHostPort(ep.Host, ep.Port))
	if err != nil {
		t.Fatalf("the port was not freed: %v", err)
	}
	_ = ln.Close()
}

func TestStartStopsWhenTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ep, lease, err := mcphost.Start(ctx, server(t, turnRegistry(), "pilot"))
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	// Only the context: Close would stop the server on its own.
	addr := net.JoinHostPort(ep.Host, ep.Port)
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			break
		}
		_ = conn.Close()
		if time.Now().After(deadline) {
			t.Fatal("the endpoint still accepts connections after the context ended")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestStartMemoryRefusesAfterTheContextEnds(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, lease, err := mcphost.StartMemory(ctx, server(t, turnRegistry(), "pilot"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := lease.Connect(ctx, clientInfo)
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := lease.Connect(context.Background(), clientInfo); !errors.Is(err, mcphost.ErrLeaseClosed) {
		t.Fatalf("connect after the context ended: %v", err)
	}
	// The session connected before is closed without Close being called.
	callCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	for callEcho(callCtx, c, "pilot_echo") == nil {
		if callCtx.Err() != nil {
			t.Fatal("a session outlived the context")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestConcurrentTurnsCannotReachEachOther(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := turnRegistry()
	roles := []string{"pilot", "copilot"}
	eps := make([]mcphost.Endpoint, len(roles))
	leases := make([]mcphost.Lease, len(roles))
	var wg sync.WaitGroup
	for i, role := range roles {
		wg.Go(func() {
			ep, lease, err := mcphost.Start(ctx, server(t, r, role))
			if err != nil {
				t.Error(err)
				return
			}
			eps[i], leases[i] = ep, lease
		})
	}
	wg.Wait()
	if t.Failed() {
		t.FailNow()
	}
	defer func() {
		for _, l := range leases {
			_ = l.Close()
		}
	}()
	a, b := eps[0], eps[1]
	if a.Port == b.Port || a.Token == b.Token {
		t.Fatalf("turns share an endpoint: %+v %+v", a, b)
	}
	// Each turn's own token opens its own server, concurrently.
	for i, role := range roles {
		wg.Go(func() {
			c, err := dial(ctx, eps[i].URL, eps[i].Token)
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = c.Close() }()
			if err := callEcho(ctx, c, role+"_echo"); err != nil {
				t.Errorf("%s: %v", role, err)
			}
			other := roles[1-i] + "_echo"
			if err := callEcho(ctx, c, other); err == nil {
				t.Errorf("%s reached %s", role, other)
			}
		})
	}
	wg.Wait()
	// Neither turn's token opens the other's endpoint.
	if got := status(t, b.URL, a.Token); got != http.StatusUnauthorized {
		t.Fatalf("pilot's token at copilot's endpoint: %d", got)
	}
	if got := status(t, a.URL, b.Token); got != http.StatusUnauthorized {
		t.Fatalf("copilot's token at pilot's endpoint: %d", got)
	}
	if _, err := dial(ctx, b.URL, a.Token); err == nil {
		t.Fatal("pilot's token connected to copilot's endpoint")
	}
	// Closing one turn leaves the other serving.
	if err := leases[0].Close(); err != nil {
		t.Fatal(err)
	}
	c, err := dial(ctx, b.URL, b.Token)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	if err := callEcho(ctx, c, "copilot_echo"); err != nil {
		t.Fatalf("copilot after pilot closed: %v", err)
	}
}

func TestStartMemoryOpensNoListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	r := turnRegistry()
	var start mcphost.StartFunc = mcphost.StartMemory
	a, la, err := start(ctx, server(t, r, "pilot"))
	if err != nil {
		t.Fatal(err)
	}
	b, lb, err := start(ctx, server(t, r, "copilot"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lb.Close() }()
	if !strings.HasPrefix(a.URL, "memory://") || a.URL == b.URL || a.Token == "" || a.Token == b.Token || a.Port != "" {
		t.Fatalf("endpoints %+v %+v", a, b)
	}
	ca, err := la.Connect(ctx, clientInfo)
	if err != nil {
		t.Fatal(err)
	}
	cb, err := lb.Connect(ctx, clientInfo)
	if err != nil {
		t.Fatal(err)
	}
	if got := toolNames(t, ctx, ca); !slices.Equal(got, []string{"pilot_echo"}) {
		t.Fatalf("pilot tools %v", got)
	}
	if got := toolNames(t, ctx, cb); !slices.Equal(got, []string{"copilot_echo"}) {
		t.Fatalf("copilot tools %v", got)
	}
	if err := callEcho(ctx, ca, "pilot_echo"); err != nil {
		t.Fatal(err)
	}
	if err := la.Close(); err != nil {
		t.Fatal(err)
	}
	if err := la.Close(); err != nil {
		t.Fatal(err)
	}
	if err := callEcho(ctx, ca, "pilot_echo"); err == nil {
		t.Fatal("a call succeeded after the lease closed")
	}
	if _, err := la.Connect(ctx, clientInfo); !errors.Is(err, mcphost.ErrLeaseClosed) {
		t.Fatalf("connect after close: %v", err)
	}
	if err := callEcho(ctx, cb, "copilot_echo"); err != nil {
		t.Fatalf("copilot after pilot closed: %v", err)
	}
}

func TestStartRefusesNoServer(t *testing.T) {
	for _, start := range []mcphost.StartFunc{mcphost.Start, mcphost.StartMemory} {
		if _, _, err := start(context.Background(), nil); err == nil {
			t.Fatal("nil server accepted")
		}
	}
}
