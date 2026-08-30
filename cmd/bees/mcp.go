package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

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
	cmd := groupCmd("mcp", "The built-in MCP server every session talks to")
	cmd.Long = `Every session claude runs gets a stdio MCP server named "bees" that exposes
the factory's own operations — the mailbox, issue creation and the session
outcome — as tools, so a session does not have to build a command line for
them. bees writes the server into the session's mcp.json and claude starts it;
you only run "bees mcp serve" yourself to debug it.`
	cmd.Hidden = true
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve the bees tools on stdio (started by claude, not by hand)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			b := &backend{g: g}
			srv := mcpserver.New(mcpserver.EnvFromOS(), mcpserver.Deps{Issues: b, GitHub: b})
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
			fmt.Print(toolsText(list))
			return nil
		},
	}
	cmd.AddCommand(serve, tools)
	return cmd
}

// toolsText renders a session's tool list: one line per tool, followed by the
// enum of every constrained parameter. The enums are what differ between
// roles — done's status is the role's valid outcomes — so leaving them out
// would make the output the same for everybody.
func toolsText(tools []*mcp.Tool) string {
	var b strings.Builder
	for _, t := range tools {
		fmt.Fprintf(&b, "mcp__%s__%-16s %s\n", config.BuiltinMCPServer, t.Name, t.Title)
		for _, e := range enums(t.InputSchema) {
			fmt.Fprintf(&b, "    %s: %s\n", e.prop, strings.Join(e.values, " | "))
		}
	}
	return b.String()
}

type propEnum struct {
	prop   string
	values []string
}

// enums reads the constrained properties out of a tool's advertised input
// schema, in property-name order. The schema arrives as decoded JSON, so it
// is re-marshalled rather than type-asserted.
func enums(schema any) []propEnum {
	b, err := json.Marshal(schema)
	if err != nil {
		return nil
	}
	var s struct {
		Properties map[string]struct {
			Enum []string `json:"enum"`
		} `json:"properties"`
	}
	if err := json.Unmarshal(b, &s); err != nil {
		return nil
	}
	var out []propEnum
	for prop, p := range s.Properties {
		if len(p.Enum) > 0 {
			out = append(out, propEnum{prop: prop, values: p.Enum})
		}
	}
	slices.SortFunc(out, func(a, b propEnum) int { return strings.Compare(a.prop, b.prop) })
	return out
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

// backend is the production implementation of the server's Issues and
// GitHub interfaces: internal/issues for creation, a gh client for
// everything else, with bees.toml loaded on first use. The MCP server must
// start even when the configuration cannot be read, so the failure surfaces
// from the tool that needs it.
type backend struct {
	g  *globalFlags
	mu sync.Mutex
	gh *github.Client
	// filter and labels are only valid once gh is set.
	filter config.Filter
	labels config.Labels
}

func (b *backend) load(ctx context.Context) error {
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

func (b *backend) Create(ctx context.Context, opts issues.Options) (issues.Result, error) {
	if err := b.load(ctx); err != nil {
		return issues.Result{}, err
	}
	return issues.Create(ctx, b.gh, b.filter, b.labels, opts)
}

func (b *backend) Link(ctx context.Context, parent, child int) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	return issues.Link(ctx, b.gh, b.labels, parent, child)
}

// Rules returns the factory's visibility filter and label set. The query is
// built exactly as scheduler.New builds it, so "matches the filter" means
// the same thing to a tool and to the orchestrator.
func (b *backend) Rules(ctx context.Context) (github.Query, config.Labels, error) {
	if err := b.load(ctx); err != nil {
		return github.Query{}, config.Labels{}, err
	}
	q := github.Query{Assignee: b.filter.Assignee, Milestone: b.filter.Milestone}
	if b.filter.LabelRequired() {
		q.Label = b.filter.Label
	}
	return q, b.labels, nil
}

func (b *backend) Issue(ctx context.Context, number int) (github.Issue, error) {
	if err := b.load(ctx); err != nil {
		return github.Issue{}, err
	}
	return b.gh.GetIssue(ctx, number)
}

func (b *backend) Parent(ctx context.Context, number int) (*github.Parent, error) {
	if err := b.load(ctx); err != nil {
		return nil, err
	}
	return b.gh.ParentIssue(ctx, number)
}

func (b *backend) PR(ctx context.Context, number int) (github.PR, error) {
	if err := b.load(ctx); err != nil {
		return github.PR{}, err
	}
	return b.gh.GetPR(ctx, number)
}

func (b *backend) PRActivity(ctx context.Context, number int, since time.Time) ([]github.Activity, error) {
	if err := b.load(ctx); err != nil {
		return nil, err
	}
	return b.gh.PRActivity(ctx, number, since)
}

func (b *backend) Checks(ctx context.Context, number int) ([]github.Check, error) {
	if err := b.load(ctx); err != nil {
		return nil, err
	}
	return b.gh.RequiredChecks(ctx, number)
}

func (b *backend) Comment(ctx context.Context, number int, body string) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	return b.gh.Comment(ctx, number, body)
}

func (b *backend) EditBody(ctx context.Context, number int, body string) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	return b.gh.EditBody(ctx, number, body)
}

func (b *backend) EditLabels(ctx context.Context, number int, add, remove []string) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	return b.gh.EditLabels(ctx, number, add, remove)
}
