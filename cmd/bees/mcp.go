package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/spf13/cobra"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/issues"
	"github.com/kpenfound/busybees/internal/mcpserver"
	"github.com/kpenfound/busybees/internal/session"
)

func newMCPCmd(g *globalFlags) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "mcp",
		Short: "The built-in MCP server every session talks to",
		Long: `Every session claude runs gets a stdio MCP server named "bees" that exposes
the factory's own operations — the mailbox, issue creation and the session
outcome — as tools, so a session does not have to build a command line for
them. bees writes the server into the session's mcp.json and claude starts it;
you only run "bees mcp serve" yourself to debug it.`,
		Hidden: true,
	}
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve the bees tools on stdio (started by claude, not by hand)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			srv := mcpserver.New(mcpserver.EnvFromOS(), mcpserver.Deps{Issues: &issueBackend{g: g}})
			err := srv.Run(cmd.Context(), &mcp.StdioTransport{})
			if isCleanShutdown(err) {
				return nil
			}
			return err
		},
	}
	tools := &cobra.Command{
		Use:   "tools",
		Short: "List the tools a role's session sees",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			env := mcpserver.EnvFromOS()
			if len(args) == 1 {
				role, err := config.CanonicalRole(args[0])
				if err != nil {
					return err
				}
				env.Role = role
			}
			list, err := mcpserver.Tools(cmd.Context(), env)
			if err != nil {
				return err
			}
			for _, t := range list {
				fmt.Printf("mcp__%s__%-14s %s\n", config.BuiltinMCPServer, t.Name, t.Title)
			}
			return nil
		},
	}
	cmd.AddCommand(serve, tools)
	return cmd
}

// codeServerClosing is the jsonrpc2 error code the SDK answers with once the
// connection is going away ("server is closing"). It is not exported, and the
// read error it reports is formatted with %v rather than wrapped, so matching
// on the code is the only way to recognise it.
const codeServerClosing = -32004

// isCleanShutdown reports whether an error from mcp.Server.Run is an ordinary
// end of session rather than a failure. claude closes the server's stdin when
// it is done with it and kills the process on shutdown; neither is worth a
// nonzero exit status, which claude would record as the server having crashed.
func isCleanShutdown(err error) bool {
	return err == nil ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, context.Canceled) ||
		errors.Is(err, &jsonrpc.Error{Code: codeServerClosing})
}

// issueBackend creates issues through internal/issues, loading bees.toml on
// first use. The MCP server must start even when the configuration cannot be
// read, so the failure surfaces from the tool that needs it.
type issueBackend struct {
	g  *globalFlags
	mu sync.Mutex
	gh *github.Client
	// filter and labels are only valid once gh is set.
	filter config.Filter
	labels config.Labels
}

func (b *issueBackend) load(ctx context.Context) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.gh != nil {
		return nil
	}
	p, err := configPath(b.g)
	if err != nil {
		return err
	}
	cfg, err := config.Load(p)
	if err != nil {
		return err
	}
	// The repo is already resolved in a session; only fall back to reading
	// the git remote outside one.
	if cfg.Project.Repo == "" {
		cfg.Project.Repo = os.Getenv(session.EnvRepo)
	}
	if cfg.Project.Repo == "" {
		if err := cfg.Resolve(ctx); err != nil {
			return err
		}
	}
	if cfg.Filter.Assignee == "@me" {
		login, err := github.CurrentUser(ctx)
		if err != nil {
			return fmt.Errorf("resolve filter.assignee=@me: %w", err)
		}
		cfg.Filter.Assignee = login
	}
	b.filter, b.labels, b.gh = cfg.Filter, cfg.Labels(), github.New(cfg.Project.Repo)
	return nil
}

func (b *issueBackend) Create(ctx context.Context, opts issues.Options) (issues.Result, error) {
	if err := b.load(ctx); err != nil {
		return issues.Result{}, err
	}
	return issues.Create(ctx, b.gh, b.filter, b.labels, opts)
}

func (b *issueBackend) Link(ctx context.Context, parent, child int) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	return issues.Link(ctx, b.gh, parent, child)
}
