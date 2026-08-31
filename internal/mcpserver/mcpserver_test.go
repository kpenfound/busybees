package mcpserver

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/issues"
	"github.com/kpenfound/busybees/internal/mail"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/state"
)

// harness is a server connected to a client over the in-memory transport,
// with a state dir and a session dir of its own.
type harness struct {
	t          *testing.T
	client     *mcp.ClientSession
	env        Env
	mail       *mail.Box
	sessionDir string
}

func newHarness(t *testing.T, role string, deps Deps) *harness {
	t.Helper()
	stateDir := t.TempDir()
	st := state.New(stateDir)
	if err := st.Init(); err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(t.TempDir(), "session")
	if err := os.MkdirAll(sessionDir, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, sessionDir: sessionDir, mail: mail.Open(st.MailDir())}
	h.env = Env{Role: role, StateDir: stateDir, SessionDir: sessionDir, Issue: 36, PR: 72}

	ctx := context.Background()
	client, srv, err := Connect(ctx, h.env, deps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = srv.Wait()
	})
	h.client = client
	return h
}

// call runs a tool and returns its text result. It fails the test when the
// tool reported an error.
func (h *harness) call(name string, args map[string]any) string {
	h.t.Helper()
	res := h.callRaw(name, args)
	if res.IsError {
		h.t.Fatalf("%s: %s", name, resultText(res))
	}
	return resultText(res)
}

func (h *harness) callRaw(name string, args map[string]any) *mcp.CallToolResult {
	h.t.Helper()
	res, err := h.client.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	return res
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if t, ok := c.(*mcp.TextContent); ok {
			b.WriteString(t.Text)
		}
	}
	return b.String()
}

func (h *harness) tools() []*mcp.Tool {
	h.t.Helper()
	res, err := h.client.ListTools(context.Background(), nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return res.Tools
}

// TestSessionToolsAreOfferedToEveryRole: the tools that are not about
// GitHub are the same for everybody. TestGitHubToolsPerRole covers the ones
// that are not.
func TestSessionToolsAreOfferedToEveryRole(t *testing.T) {
	for _, role := range append(config.Roles, "") {
		h := newHarness(t, role, Deps{})
		var names []string
		for _, tool := range h.tools() {
			names = append(names, tool.Name)
		}
		for _, want := range []string{"done", "issue_create", "issue_link", "mail_list", "mail_send"} {
			if !slices.Contains(names, want) {
				t.Errorf("role %q: tools = %s, want %s", role, strings.Join(names, ", "), want)
			}
		}
	}
}

func TestDoneEnumPerRole(t *testing.T) {
	for role, want := range map[string]string{
		config.RoleDeveloper:      "pr-opened, pr-updated, question, failed",
		config.RoleReviewer:       "approved, changes-requested, failed",
		config.RoleQA:             "done, failed",
		config.RoleProductManager: "done, idle, failed",
		"":                        "", // unknown role: unrestricted
	} {
		h := newHarness(t, role, Deps{})
		if got := strings.Join(statusEnum(t, h.tools()), ", "); got != want {
			t.Errorf("role %q: status enum = %q, want %q", role, got, want)
		}
	}
}

// statusEnum reads the done tool's status enum out of the advertised schema,
// which is the JSON a session actually sees.
func statusEnum(t *testing.T, tools []*mcp.Tool) []string {
	t.Helper()
	for _, tool := range tools {
		if tool.Name != "done" {
			continue
		}
		b, err := json.Marshal(tool.InputSchema)
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Properties struct {
				Status struct {
					Enum []string `json:"enum"`
				} `json:"status"`
			} `json:"properties"`
		}
		if err := json.Unmarshal(b, &schema); err != nil {
			t.Fatal(err)
		}
		return schema.Properties.Status.Enum
	}
	t.Fatal("no done tool")
	return nil
}

func TestMailSendDefaultsIssueAndPR(t *testing.T) {
	h := newHarness(t, config.RoleDeveloper, Deps{})
	got := h.call("mail_send", map[string]any{
		"to": "project_manager", "subject": "Which format?", "body": "CSV or JSON?",
	})
	if !strings.HasPrefix(got, "sent ") || !strings.HasSuffix(got, " to project_manager") {
		t.Fatalf("result: %q", got)
	}
	msgs, err := h.mail.List(mail.Filter{To: config.RoleProjectManager})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("messages: %+v", msgs)
	}
	m := msgs[0]
	if m.From != config.RoleDeveloper || m.Subject != "Which format?" || m.Body != "CSV or JSON?" {
		t.Fatalf("message: %+v", m)
	}
	if m.Issue != 36 || m.PR != 72 {
		t.Fatalf("issue/pr not defaulted from the environment: %+v", m)
	}

	// mail_list reads the same mailbox back.
	if out := h.call("mail_list", map[string]any{"unread": true}); !strings.Contains(out, "Which format?") {
		t.Fatalf("mail_list: %q", out)
	}
	// Explicit numbers win over the environment.
	h.call("mail_send", map[string]any{"to": "reviewer", "subject": "s", "body": "b", "issue": 1, "pr": 2})
	msgs, _ = h.mail.List(mail.Filter{To: config.RoleReviewer})
	if len(msgs) != 1 || msgs[0].Issue != 1 || msgs[0].PR != 2 {
		t.Fatalf("explicit issue/pr: %+v", msgs)
	}
}

func TestMailSendRejectsUnknownRole(t *testing.T) {
	h := newHarness(t, config.RoleDeveloper, Deps{})
	res := h.callRaw("mail_send", map[string]any{"to": "nobody", "subject": "s", "body": "b"})
	if !res.IsError {
		t.Fatal("want an error result for an unknown role")
	}
}

func TestDoneWritesTheOutcome(t *testing.T) {
	h := newHarness(t, config.RoleDeveloper, Deps{})
	if got := h.call("done", map[string]any{"status": "pr-opened", "note": "opened #72", "pr": 72}); got != "outcome recorded: pr-opened" {
		t.Fatalf("result: %q", got)
	}
	o, ok, err := session.ReadOutcome(h.sessionDir)
	if err != nil || !ok {
		t.Fatalf("outcome: %v %v", ok, err)
	}
	// The same outcome `bees done pr-opened --pr 72 -m "opened #72"` writes.
	want, err := session.Report(t.TempDir(), config.RoleDeveloper, session.Outcome{Status: "pr-opened", Note: "opened #72", PR: 72, Issue: 36})
	if err != nil {
		t.Fatal(err)
	}
	if o != want {
		t.Fatalf("outcome = %+v, want %+v", o, want)
	}
}

func TestDoneRejectsAStatusTheRoleMayNotReport(t *testing.T) {
	h := newHarness(t, config.RoleReviewer, Deps{})
	// The advertised enum is what rejects this: the SDK validates the input
	// against the schema before the handler runs. The handler's own check is
	// covered directly by session.ValidateOutcome's tests.
	res := h.callRaw("done", map[string]any{"status": "pr-opened"})
	if !res.IsError {
		t.Fatal("want an error result")
	}
	if _, ok, _ := session.ReadOutcome(h.sessionDir); ok {
		t.Fatal("a rejected outcome was written")
	}
}

func TestDonePROpenedNeedsAPR(t *testing.T) {
	// No PR in the environment and none in the call.
	h := newHarnessWithEnv(t, Env{Role: config.RoleDeveloper, StateDir: t.TempDir(), SessionDir: t.TempDir()})
	res := h.callRaw("done", map[string]any{"status": "pr-opened"})
	if !res.IsError || !strings.Contains(resultText(res), "requires a pull request number") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
}

func newHarnessWithEnv(t *testing.T, env Env) *harness {
	t.Helper()
	client, srv, err := Connect(context.Background(), env, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = client.Close()
		_ = srv.Wait()
	})
	return &harness{t: t, client: client, env: env, sessionDir: env.SessionDir, mail: mail.Open(state.New(env.StateDir).MailDir())}
}

// fakeGH stands in for the gh CLI.
type fakeGH struct {
	calls []string
}

func (f *fakeGH) client(t *testing.T) *github.Client {
	c := github.New("acme/widgets")
	c.Exec = func(ctx context.Context, args ...string) ([]byte, error) {
		call := strings.Join(args, " ")
		f.calls = append(f.calls, call)
		switch {
		case strings.HasPrefix(call, "api repos/acme/widgets/issues/36"):
			return []byte(`{"id": 9036, "milestone": {"title":"v0.1.0"}}`), nil
		case strings.HasPrefix(call, "api repos/acme/widgets/issues/90"):
			return []byte(`{"id": 9090, "milestone": null}`), nil
		case strings.HasPrefix(call, "api repos/acme/widgets/milestones"):
			return []byte(`[{"number": 3, "title": "v0.1.0"}]`), nil
		case strings.HasPrefix(call, "issue create"):
			return []byte("https://github.com/acme/widgets/issues/90\n"), nil
		case strings.HasPrefix(call, "api --method POST"), strings.HasPrefix(call, "api --method PATCH"):
			return []byte(`{}`), nil
		}
		t.Fatalf("unexpected gh call: %s", call)
		return nil, nil
	}
	return c
}

// ghIssues is the real internal/issues backend on a fake gh client.
type ghIssues struct {
	gh     *github.Client
	filter config.Filter
	labels config.Labels
}

func (g *ghIssues) Create(ctx context.Context, opts issues.Options) (issues.Result, error) {
	return issues.Create(ctx, g.gh, g.filter, g.labels, opts)
}

func (g *ghIssues) Link(ctx context.Context, parent, child int) (issues.LinkResult, error) {
	return issues.Link(ctx, g.gh, g.labels, parent, child)
}

func TestIssueCreateAndLink(t *testing.T) {
	f := &fakeGH{}
	backend := &ghIssues{gh: f.client(t), filter: config.Filter{Label: "bees", Assignee: "kyle"}, labels: config.LabelsFor("bees")}
	h := newHarness(t, config.RoleDeveloper, Deps{Issues: backend})

	got := h.call("issue_create", map[string]any{
		"title": "Crash on empty input", "body": "steps", "bug": true, "related": 36, "blocked_by": []int{12, 15},
	})
	if got != `created #90 milestone "v0.1.0"` {
		t.Fatalf("result: %q", got)
	}
	joined := strings.Join(f.calls, "\n")
	for _, want := range []string{"--label bees:bug", "--label bees:triage", "--assignee kyle", "--milestone v0.1.0", "--body Blocked by #12, #15\n\nsteps"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}

	f.calls = nil
	// #90 has no milestone of its own, so the link puts it in its new
	// parent's and says so.
	if got := h.call("issue_link", map[string]any{"parent": 36, "child": 90}); got != `#90 is now a sub-issue of #36 milestone "v0.1.0"` {
		t.Fatalf("result: %q", got)
	}
	if !strings.Contains(strings.Join(f.calls, "\n"), "repos/acme/widgets/issues/36/sub_issues") {
		t.Fatalf("calls: %v", f.calls)
	}
}

func TestIssueCreateRejectsBugAndFeature(t *testing.T) {
	h := newHarness(t, config.RoleProductManager, Deps{Issues: &ghIssues{}})
	res := h.callRaw("issue_create", map[string]any{"title": "t", "body": "b", "bug": true, "feature": true})
	if !res.IsError || !strings.Contains(resultText(res), "exclusive") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
}

func TestIssueToolsWithoutABackend(t *testing.T) {
	h := newHarness(t, config.RoleDeveloper, Deps{})
	res := h.callRaw("issue_create", map[string]any{"title": "t", "body": "b"})
	if !res.IsError || !strings.Contains(resultText(res), "bees.toml") {
		t.Fatalf("result: %v %q", res.IsError, resultText(res))
	}
}

// TestCLAUDEMdListsEveryTool pins the `internal/mcpserver` bullet in CLAUDE.md —
// the map every session reads before touching this repo — against the tools the
// server actually registers. It compares sets, not order: the order in the bullet
// is a readability choice, not something to fail a test on.
//
// The expected set is the union over config.Roles, so a role-scoped tool counts
// as documented rather than as an extra. Tool names are taken from the bullet by
// reading every backticked token on the line and dropping the ones that cannot be
// a tool name: anything containing a space, a `/` or a `.` (`internal/mcpserver`,
// `bees mcp serve`, a file name), plus the exact token `bees`, the server's own
// name. Everything else is compared as a tool name, so a bare backticked word
// added to the line fails this test loudly instead of going unchecked. That is
// deliberate: backtick it as part of a longer phrase, or expect to hear about it.
func TestCLAUDEMdListsEveryTool(t *testing.T) {
	// Go runs a test with its package directory as cwd, so the repo file is
	// two levels up. Reading it (rather than embedding a copy) is the point:
	// the doc is what has to stay true.
	const claudeMd = "../../CLAUDE.md"
	const bullet = "- `internal/mcpserver`"

	data, err := os.ReadFile(claudeMd)
	if err != nil {
		t.Fatalf("read %s: %v", claudeMd, err)
	}
	line := ""
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, bullet) {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("CLAUDE.md: no line starts with %q. That bullet lists the built-in MCP tools and is kept in sync with internal/mcpserver; restore it (or update this test) rather than leaving the layout map without one.", bullet)
	}

	documented := map[string]bool{}
	for _, name := range backtickedTokens(line) {
		if strings.ContainsAny(name, " /.") || name == config.BuiltinMCPServer {
			continue
		}
		documented[name] = true
	}

	registered := map[string]bool{}
	for _, role := range config.Roles {
		list, err := Tools(context.Background(), Env{Role: role})
		if err != nil {
			t.Fatalf("role %q: %v", role, err)
		}
		for _, tool := range list {
			registered[tool.Name] = true
		}
	}

	var missing, extra []string
	for name := range registered {
		if !documented[name] {
			missing = append(missing, name)
		}
	}
	for name := range documented {
		if !registered[name] {
			extra = append(extra, name)
		}
	}
	slices.Sort(missing)
	slices.Sort(extra)

	if len(missing) > 0 {
		t.Errorf("CLAUDE.md: the %q line does not list %s, registered by the server for at least one role (internal/mcpserver/mcpserver.go). Add the missing names to that line:\n%s",
			bullet, strings.Join(missing, ", "), line)
	}
	if len(extra) > 0 {
		t.Errorf("CLAUDE.md: the %q line lists %s, not a tool the server registers for any role. Drop the stale names from that line — or, if one is a word that merely happens to be backticked there, unbacktick it:\n%s",
			bullet, strings.Join(extra, ", "), line)
	}
}

// backtickedTokens returns the `…`-quoted spans of a markdown line, in order.
// It is a copy of cmd/bees's backticked: six lines, and the two doc-pinning
// tests should not couple.
func backtickedTokens(line string) []string {
	var out []string
	for {
		i := strings.Index(line, "`")
		if i < 0 {
			return out
		}
		rest := line[i+1:]
		j := strings.Index(rest, "`")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		line = rest[j+1:]
	}
}
