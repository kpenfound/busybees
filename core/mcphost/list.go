package mcphost

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Tools lists the tools a server exposes, in the order
// the server offers them. It connects a client over an in-memory transport,
// so it exercises the same path a session does.
func Tools(ctx context.Context, server *mcp.Server, client mcp.Implementation) ([]*mcp.Tool, error) {
	c, srv, err := Connect(ctx, server, client)
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

// Connect returns a client session connected to server over an in-memory
// transport. Callers close the client session and wait
// for the server one. Client metadata is supplied by the caller.
func Connect(ctx context.Context, server *mcp.Server, client mcp.Implementation) (*mcp.ClientSession, *mcp.ServerSession, error) {
	serverT, clientT := mcp.NewInMemoryTransports()
	srv, err := server.Connect(ctx, serverT, nil)
	if err != nil {
		return nil, nil, err
	}
	c, err := mcp.NewClient(&client, nil).Connect(ctx, clientT, nil)
	if err != nil {
		// Close before waiting: nothing will ever close the server session
		// from the other end.
		_ = srv.Close()
		_ = srv.Wait()
		return nil, nil, err
	}
	return c, srv, nil
}
