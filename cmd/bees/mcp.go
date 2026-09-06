package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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
	cmd.Long = `Every session gets a stdio MCP server named "bees" that exposes the factory's
own operations — the mailbox, issue creation and the session outcome — as
tools, so a session does not have to build a command line for them. bees hands
the server to the agent (in mcp.json for claude, as mcp_servers.bees overrides
for codex) and the agent starts it; you only run "bees mcp serve" yourself to
debug it. A container session cannot start it (the bees binary is not in the
container), so for one the runner starts "bees mcp serve --listen" on the host
and the session reaches it over HTTP.`
	cmd.Hidden = true
	var listen string
	serve := &cobra.Command{
		Use:   "serve",
		Short: "Serve the bees tools on stdio (started by the agent, not by hand)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			b := &backend{g: g}
			srv := mcpserver.New(mcpserver.EnvFromOS(), mcpserver.Deps{Issues: b, GitHub: b})
			if listen != "" {
				return serveMCPHTTP(cmd.Context(), srv, listen, os.Getenv(session.EnvMCPToken), cmd.OutOrStdout())
			}
			err := srv.Run(cmd.Context(), &mcp.StdioTransport{})
			if isCleanShutdown(err) {
				return nil
			}
			return err
		},
	}
	serve.Flags().StringVar(&listen, "listen", "", "serve over HTTP on this address instead of stdio, for a container session (needs $"+session.EnvMCPToken+")")
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

// serveMCPHTTP serves the session's tools over streamable HTTP on addr until
// ctx ends, printing the address it is listening on — the one the runner
// puts into the session's mcp.json — as its first line of output. It is how
// a container session reaches the server, which runs on the host with the
// session's environment and paths.
//
// The token is required: the port is reachable from every container on the
// machine (and, on Linux, from the host's other users), and without one
// anything that found it could report the session's outcome or write mail
// in its name. A request that does not present it is refused before it
// reaches the server.
func serveMCPHTTP(ctx context.Context, srv *mcp.Server, addr, token string, out io.Writer) error {
	if token == "" {
		return fmt.Errorf("bees mcp serve --listen needs %s: the server does not serve a session's tools to whoever finds the port", session.EnvMCPToken)
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(out, "%s%s\n", session.MCPListening, ln.Addr()); err != nil {
		return err
	}
	hs := &http.Server{Handler: mcpHTTPHandler(srv, token)}
	go func() {
		<-ctx.Done()
		_ = hs.Close()
	}()
	if err := hs.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// mcpHTTPHandler serves srv over streamable HTTP to a client presenting
// token as its bearer credential, and answers 401 to any other request.
// The SDK's own guard, which refuses a request to a loopback listener whose
// Host header is not a loopback name, is switched off: it protects a local
// server from a browser tricked into reaching it (DNS rebinding), which the
// token does here, and the container reaches the loopback listener through
// the host's alias, which is exactly such a Host header.
func mcpHTTPHandler(srv *mcp.Server, token string) http.Handler {
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, &mcp.StreamableHTTPOptions{DisableLocalhostProtection: true})
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
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
	// self is the login the factory acts as, when filter.creator needs it
	// (resolveFilterSelf); policy is only valid once gh is set.
	self   string
	policy issues.Policy
}

// issuePolicy is the policy `bees issue create`, `bees issue link` and the
// MCP tools behind them create issues under: one place, so the command and
// the tool cannot disagree about it.
func issuePolicy(cfg *config.Config) issues.Policy {
	return issues.Policy{Filter: cfg.Filter, Labels: cfg.Labels(), FeatureProposals: cfg.Scheduler.Proposals()}
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
	if err := resolveFilterAssignee(ctx, cfg); err != nil {
		return err
	}
	self, err := resolveFilterSelf(ctx, cfg)
	if err != nil {
		return err
	}
	b.self = self
	b.policy = issuePolicy(cfg)
	b.gh = githubClient(cfg)
	return nil
}

func (b *backend) Create(ctx context.Context, opts issues.Options) (issues.Result, error) {
	if err := b.load(ctx); err != nil {
		return issues.Result{}, err
	}
	return issues.Create(ctx, b.gh, b.policy, opts)
}

func (b *backend) Link(ctx context.Context, parent, child int) (issues.LinkResult, error) {
	if err := b.load(ctx); err != nil {
		return issues.LinkResult{}, err
	}
	return issues.Link(ctx, b.gh, b.policy, parent, child)
}

// Rules returns the factory's visibility filter and label set. The query is
// built exactly as scheduler.New builds it, so "matches the filter" means
// the same thing to a tool and to the orchestrator.
func (b *backend) Rules(ctx context.Context) (github.Query, config.Labels, error) {
	if err := b.load(ctx); err != nil {
		return github.Query{}, config.Labels{}, err
	}
	q := github.Query{Assignee: b.policy.Filter.Assignee, Milestone: b.policy.Filter.Milestone, Creator: b.policy.Filter.Creator, Self: b.self}
	if b.policy.Filter.LabelRequired() {
		q.Label = b.policy.Filter.Label
	}
	return q, b.policy.Labels, nil
}

// ActsAs returns the GitHub login the factory acts as, which is what
// [github].login configures and what githubClient already gave the client.
// It is configuration, not a question for GitHub — Client.Login is the one
// that asks.
func (b *backend) ActsAs(ctx context.Context) (string, error) {
	if err := b.load(ctx); err != nil {
		return "", err
	}
	return b.gh.ActsAs, nil
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

func (b *backend) SubmitReview(ctx context.Context, number int, event, body string) error {
	if err := b.load(ctx); err != nil {
		return err
	}
	return b.gh.SubmitReview(ctx, number, event, body)
}
