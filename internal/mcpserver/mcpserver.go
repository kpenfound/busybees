// Package mcpserver serves the factory's own operations to a Claude Code
// session as MCP tools, so a session calls a tool with a schema instead of
// guessing a `bees` command line and running it through Bash.
//
// The server is the same code the CLI runs: mail_send/mail_list go through
// internal/mail, issue_create/issue_link through internal/issues and done
// through session.Report. The GitHub tools (issue_view, pr_view, comment,
// issue_edit_body, issue_set_state, issue_question, submit_review) go through a `gh` client
// and enforce the factory's rules — the visibility filter, the comment
// marker, who owns which issue — instead of restating them in a prompt. It is started by claude as `bees mcp serve` and
// takes its context (role, state dir, session dir, issue, PR) from the BEES_*
// environment, exactly like the session commands.
//
// The tool set and the enums in the schemas depend on the role: a developer's
// done tool offers pr-opened, pr-updated, question and failed, a reviewer's
// approved, changes-requested and failed. An unknown or empty role gets the
// full tool set with unrestricted enums, so `bees mcp serve` is usable by hand.
package mcpserver

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/issues"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// Env is the session context the server serves. It mirrors the BEES_*
// variables a session runs with.
type Env struct {
	Role       string
	StateDir   string
	SessionDir string
	// Issue and PR are the defaults for tools that take an issue or PR number.
	Issue int
	PR    int
}

// EnvFromOS reads the session context from the BEES_* environment.
func EnvFromOS() Env {
	return Env{
		Role:       os.Getenv(session.EnvRole),
		StateDir:   os.Getenv(session.EnvStateDir),
		SessionDir: os.Getenv(session.EnvSessionDir),
		Issue:      envInt(session.EnvIssue),
		PR:         envInt(session.EnvPR),
	}
}

func envInt(name string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(name)))
	return n
}

// Issues is the backend behind issue_create and issue_link. It is an
// interface so the server can be built before bees.toml is read (and so
// tests can drive it with a fake gh client).
type Issues interface {
	Create(ctx context.Context, opts issues.Options) (issues.Result, error)
	Link(ctx context.Context, parent, child int) (issues.LinkResult, error)
}

// Deps are the server's collaborators.
type Deps struct {
	// Mail is the mailbox. When nil it is opened under Env.StateDir.
	Mail *mail.Box
	// Issues creates and links issues. When nil the issue tools report that
	// they are unavailable instead of failing at startup.
	Issues Issues
	// GitHub reads and writes issues and pull requests. When nil the GitHub
	// tools report that they are unavailable, exactly like Issues.
	GitHub GitHub
}

// server holds the state shared by the tool handlers.
type server struct {
	env    Env
	mail   *mail.Box
	issues Issues
	github GitHub
}

// New builds the MCP server for env. It never fails: a missing collaborator
// turns into an error from the tool that needs it, not a server that will
// not start, so a session always sees the tools it was told about.
func New(env Env, deps Deps) *mcp.Server {
	s := &server{env: env, mail: deps.Mail, issues: deps.Issues, github: deps.GitHub}
	if s.mail == nil && env.StateDir != "" {
		s.mail = mail.Open(state.New(env.StateDir).MailDir())
	}
	srv := mcp.NewServer(&mcp.Implementation{
		Name:    config.BuiltinMCPServer,
		Title:   "busybees",
		Version: Version,
	}, nil)
	s.addMailTools(srv)
	s.addIssueTools(srv)
	s.addGitHubTools(srv)
	s.addDoneTool(srv)
	return srv
}

// Version is reported to the client during the initialize handshake. It is
// the tool set's version, not the bees binary's.
const Version = "1"

// text is the one-line success result every tool returns.
func text(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}},
	}
}

// schemaFor infers the input schema of In and constrains the named
// properties to the given values. A nil or empty value list leaves the
// property unconstrained.
func schemaFor[In any](enums map[string][]string) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("mcpserver: schema for %T: %v", *new(In), err))
	}
	for prop, values := range enums {
		p, ok := s.Properties[prop]
		if !ok {
			panic(fmt.Sprintf("mcpserver: %T has no property %q", *new(In), prop))
		}
		for _, v := range values {
			p.Enum = append(p.Enum, v)
		}
	}
	return s
}
