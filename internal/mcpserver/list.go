package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/core/mcphost"
)

// Tools lists the session's tools through the same handlers as MCP clients.
func Tools(ctx context.Context, env Env) ([]*mcp.Tool, error) {
	return mcphost.Tools(ctx, New(env, Deps{}), mcp.Implementation{Name: "bees-cli", Version: Version})
}

// Connect returns an in-memory client and server session. Close the client,
// then wait for the server session when finished.
func Connect(ctx context.Context, env Env, deps Deps) (*mcp.ClientSession, *mcp.ServerSession, error) {
	return mcphost.Connect(ctx, New(env, deps), mcp.Implementation{Name: "bees-cli", Version: Version})
}
