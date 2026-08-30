package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tools lists the tools a session with this environment sees, in the order
// the server offers them. It connects a client over an in-memory transport,
// so it exercises the same path a session does.
func Tools(ctx context.Context, env Env) ([]*mcp.Tool, error) {
	c, srv, err := Connect(ctx, env, Deps{})
	if err != nil {
		return nil, err
	}
	defer func() {
		_ = c.Close()
		_ = srv.Wait()
	}()
	res, err := c.ListTools(ctx, nil)
	if err != nil {
		return nil, err
	}
	return res.Tools, nil
}

// Connect builds the server for env and returns a client session connected to
// it over an in-memory transport. Callers close the client session and wait
// for the server one. It is used by `bees mcp tools` and by the tests.
func Connect(ctx context.Context, env Env, deps Deps) (*mcp.ClientSession, *mcp.ServerSession, error) {
	serverT, clientT := mcp.NewInMemoryTransports()
	srv, err := New(env, deps).Connect(ctx, serverT, nil)
	if err != nil {
		return nil, nil, err
	}
	c, err := mcp.NewClient(&mcp.Implementation{Name: "bees-cli", Version: Version}, nil).Connect(ctx, clientT, nil)
	if err != nil {
		// Close before waiting: nothing will ever close the server session
		// from the other end.
		_ = srv.Close()
		_ = srv.Wait()
		return nil, nil, err
	}
	return c, srv, nil
}
