package mcphost

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EndpointPath is the URL path Start serves on. The handler answers on any
// path; the endpoint names this one.
const EndpointPath = "/mcp"

// ErrLeaseClosed is returned by Lease.Connect once the lease is closed.
var ErrLeaseClosed = errors.New("mcphost: lease closed")

// Endpoint is where one turn reaches its server: what an agent's MCP
// configuration points at. Token is the bearer credential the endpoint
// requires; hand it to the turn in an environment variable, not an argument.
type Endpoint struct {
	// URL is the endpoint as the host reaches it.
	URL string
	// Host and Port are the address the listener is bound to.
	Host, Port string
	Token      string
}

// Via returns the endpoint's URL with the host replaced, keeping the port
// and path: a container reaches a host listener by the host's alias
// (Docker's host.docker.internal), not by the address it is bound to.
func (e Endpoint) Via(host string) string {
	return "http://" + net.JoinHostPort(host, e.Port) + EndpointPath
}

// Lease is one turn's hold on a started server. Close stops the server,
// frees its endpoint and returns the error serving ended with, if any; it
// is safe to call more than once. Connect returns a client session of the
// endpoint for callers that talk to it themselves; over HTTP it presents the
// endpoint's token.
type Lease interface {
	Connect(ctx context.Context, client mcp.Implementation) (*mcp.ClientSession, error)
	Close() error
}

// StartFunc is the shape of Start and StartMemory, so an embedder can take
// either and tests can run without opening listeners.
type StartFunc func(ctx context.Context, srv *mcp.Server) (Endpoint, Lease, error)

var (
	_ StartFunc = Start
	_ StartFunc = StartMemory
)

// Start serves srv to one turn over streamable HTTP on a fresh loopback port
// with a fresh bearer token, until the lease is closed or ctx ends. Each call
// has its own listener and token, so one turn cannot reach another's server.
// A host turn reaches the loopback; so does a container under Docker Desktop
// through Endpoint.Via. On Linux a container reaches the host only on the
// bridge gateway: use StartOn with that address.
func Start(ctx context.Context, srv *mcp.Server) (Endpoint, Lease, error) {
	return StartOn(ctx, srv, "127.0.0.1:0")
}

// StartOn is Start on a given listen address, such as the Docker bridge
// gateway with port 0.
func StartOn(ctx context.Context, srv *mcp.Server, addr string) (Endpoint, Lease, error) {
	if srv == nil {
		return Endpoint{}, nil, errors.New("mcphost: no server to start")
	}
	token, err := newToken()
	if err != nil {
		return Endpoint{}, nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return Endpoint{}, nil, fmt.Errorf("mcphost: listen on %s: %w", addr, err)
	}
	host, port, err := net.SplitHostPort(ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		return Endpoint{}, nil, err
	}
	ep := Endpoint{
		URL:   "http://" + ln.Addr().String() + EndpointPath,
		Host:  host,
		Port:  port,
		Token: token,
	}
	ctx, cancel := context.WithCancel(ctx)
	l := &httpLease{ep: ep, cancel: cancel, done: make(chan struct{})}
	go func() {
		l.err = ServeHTTP(ctx, srv, ln, token)
		close(l.done)
	}()
	return ep, l, nil
}

type httpLease struct {
	ep     Endpoint
	cancel context.CancelFunc
	done   chan struct{}
	err    error
	closed atomic.Bool
}

func (l *httpLease) Connect(ctx context.Context, client mcp.Implementation) (*mcp.ClientSession, error) {
	if l.closed.Load() {
		return nil, ErrLeaseClosed
	}
	return mcp.NewClient(&client, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   l.ep.URL,
		HTTPClient: &http.Client{Transport: BearerTransport(l.ep.Token, nil)},
	}, nil)
}

func (l *httpLease) Close() error {
	l.closed.Store(true)
	l.cancel()
	<-l.done
	return l.err
}

// BearerTransport returns an http.RoundTripper that presents token as the
// bearer credential on every request through base (http.DefaultTransport
// when nil): the client side of an Endpoint.
func BearerTransport(token string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return bearer{token: token, base: base}
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.token)
	return b.base.RoundTrip(r)
}

// memoryTurns numbers the endpoints StartMemory hands out.
var memoryTurns atomic.Uint64

// StartMemory is Start without a listener, for tests: the endpoint's URL is
// a memory:// name nothing outside this process can open, and
// Lease.Connect reaches the server over an in-memory transport, with no
// token. Closing the lease, or the end of ctx, closes every session it
// connected and refuses further ones.
func StartMemory(ctx context.Context, srv *mcp.Server) (Endpoint, Lease, error) {
	if srv == nil {
		return Endpoint{}, nil, errors.New("mcphost: no server to start")
	}
	token, err := newToken()
	if err != nil {
		return Endpoint{}, nil, err
	}
	name := "turn-" + strconv.FormatUint(memoryTurns.Add(1), 10)
	ep := Endpoint{URL: "memory://" + name + EndpointPath, Host: name, Token: token}
	l := &memoryLease{ctx: ctx, srv: srv}
	// Like Start, the end of ctx drops every connection.
	context.AfterFunc(ctx, func() { _ = l.Close() })
	return ep, l, nil
}

type memoryLease struct {
	ctx    context.Context
	srv    *mcp.Server
	mu     sync.Mutex
	closed bool
	open   []*mcp.ServerSession
}

func (l *memoryLease) Connect(ctx context.Context, client mcp.Implementation) (*mcp.ClientSession, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed || l.ctx.Err() != nil {
		return nil, ErrLeaseClosed
	}
	c, ss, err := Connect(ctx, l.srv, client)
	if err != nil {
		return nil, err
	}
	l.open = append(l.open, ss)
	return c, nil
}

func (l *memoryLease) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	for _, ss := range l.open {
		_ = ss.Close()
		_ = ss.Wait()
	}
	l.open = nil
	return nil
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("mcphost: token: %w", err)
	}
	return hex.EncodeToString(b), nil
}
