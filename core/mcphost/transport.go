package mcphost

import (
	"context"
	"crypto/subtle"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Run serves any SDK transport, including StdioTransport, until closure or
// cancellation. Normal client closure is successful shutdown.
func Run(ctx context.Context, srv *mcp.Server, transport mcp.Transport) error {
	err := srv.Run(ctx, transport)
	if IsCleanShutdown(err) {
		return nil
	}
	return err
}

// ServeHTTP serves an already-bound listener until cancellation or failure.
// It owns and closes the listener. An empty token is refused.
func ServeHTTP(ctx context.Context, srv *mcp.Server, ln net.Listener, token string) error {
	defer func() { _ = ln.Close() }()
	if token == "" {
		return errors.New("mcphost: HTTP requires a bearer token")
	}
	hs := &http.Server{Handler: HTTPHandler(srv, token)}
	finished := make(chan struct{})
	defer close(finished)
	go func() {
		select {
		case <-ctx.Done():
			_ = hs.Close()
		case <-finished:
		}
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// HTTPHandler serves srv over streamable HTTP to a client presenting
// token as its bearer credential, and answers 401 to any other request.
// The SDK's own guard, which refuses a request to a loopback listener whose
// Host header is not a loopback name, is switched off: it protects a local
// server from a browser tricked into reaching it (DNS rebinding), which the
// token does here, and the container reaches the loopback listener through
// the host's alias, which is exactly such a Host header.
func HTTPHandler(srv *mcp.Server, token string) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if token == "" || !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// codeServerClosing is the jsonrpc2 error code the SDK answers with once the
// connection is going away ("server is closing"). It is not exported, and the
// read error it reports is formatted with %v rather than wrapped, so matching
// on the code is the only way to recognise it.
const codeServerClosing = -32004

// IsCleanShutdown reports whether an error from mcp.Server.Run is an ordinary
// end of session rather than a failure. A client closes the server's stdin when
// it is done with it and kills the process on shutdown; neither is worth a
// nonzero exit status, which the client would record as the server having crashed.
func IsCleanShutdown(err error) bool {
	return err == nil ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, &jsonrpc.Error{Code: codeServerClosing})
}
