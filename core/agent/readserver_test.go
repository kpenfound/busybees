package agent

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// bearerTransport presents a token on every request, the way an agent given
// bearer_token_env_var does.
type bearerTransport string

func (b bearerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+string(b))
	return http.DefaultTransport.RoundTrip(r)
}

// connectRead connects to a read server as an agent would.
func connectRead(t *testing.T, url, token string) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "agent"}, nil).Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   url,
		HTTPClient: &http.Client{Transport: bearerTransport(token)},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// callRead calls one tool and returns its text and whether it was refused.
func callRead(t *testing.T, cs *mcp.ClientSession, tool string, args map[string]any) (string, bool) {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: tool, Arguments: args})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var out strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			out.WriteString(tc.Text)
		}
	}
	return out.String(), res.IsError
}

// readWorkspace is a workspace with a file, a subdirectory, a .git
// directory, and links that point inside and outside it, beside a file
// outside it.
func readWorkspace(t *testing.T) (dir, outside string) {
	t.Helper()
	base := t.TempDir()
	dir = filepath.Join(base, "work")
	outside = filepath.Join(base, "secret.txt")
	for name, content := range map[string]string{
		outside:                                   "outside the workspace\n",
		filepath.Join(dir, "todo", "overdue.go"):  "package todo\n\nfunc Overdue() bool {\n\treturn !due.After(day)\n}\n",
		filepath.Join(dir, "README.md"):           "# todo\n",
		filepath.Join(dir, ".git", "config"):      "func Overdue in git\n",
		filepath.Join(dir, "todo", "overdue.txt"): "Overdue notes\n",
	} {
		if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(name, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape.txt")); err != nil {
		t.Fatal(err)
	}
	// os.Root follows a relative link that stays inside, and refuses an
	// absolute one wherever it points.
	if err := os.Symlink("README.md", filepath.Join(dir, "inside.txt")); err != nil {
		t.Fatal(err)
	}
	return dir, outside
}

func startTestReadServer(t *testing.T, dir string) *readServer {
	t.Helper()
	s, err := startReadServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.close)
	return s
}

// The read server offers three tools, each declared read-only, and nothing
// that writes, runs or fetches.
func TestTheReadServerOffersOnlyReadOnlyTools(t *testing.T) {
	dir, _ := readWorkspace(t)
	s := startTestReadServer(t, dir)
	res, err := connectRead(t, s.url, s.token).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tool := range res.Tools {
		names = append(names, tool.Name)
		a := tool.Annotations
		if a == nil || !a.ReadOnlyHint || a.DestructiveHint == nil || *a.DestructiveHint || a.OpenWorldHint == nil || *a.OpenWorldHint {
			t.Errorf("%s is not declared read-only and closed-world: %+v", tool.Name, a)
		}
	}
	if want := []string{"list_directory", "read_file", "search_files"}; !slices.Equal(slices.Sorted(slices.Values(names)), want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}

func TestTheReadServerReadsTheWorkspace(t *testing.T) {
	dir, _ := readWorkspace(t)
	s := startTestReadServer(t, dir)
	cs := connectRead(t, s.url, s.token)

	for _, p := range []string{"todo/overdue.go", filepath.Join(dir, "todo", "overdue.go")} {
		got, refused := callRead(t, cs, "read_file", map[string]any{"path": p})
		if refused || !strings.Contains(got, "     4\t\treturn !due.After(day)\n") {
			t.Errorf("read_file %s = %q (refused %v), want the file with line numbers", p, got, refused)
		}
	}
	got, _ := callRead(t, cs, "read_file", map[string]any{"path": "todo/overdue.go", "offset": 3, "limit": 1})
	if got != "     3\tfunc Overdue() bool {\n[more lines follow: read on from offset 4]\n" {
		t.Errorf("read_file with offset and limit = %q", got)
	}
	if got, refused := callRead(t, cs, "read_file", map[string]any{"path": "inside.txt"}); refused || !strings.Contains(got, "# todo") {
		t.Errorf("a link inside the workspace was not followed: %q", got)
	}

	got, refused := callRead(t, cs, "list_directory", map[string]any{})
	if refused || !strings.Contains(got, "todo/\n") || !strings.Contains(got, "README.md\n") {
		t.Errorf("list_directory = %q (refused %v)", got, refused)
	}

	got, refused = callRead(t, cs, "search_files", map[string]any{"pattern": `Overdue\(`})
	if refused || got != "todo/overdue.go:3: func Overdue() bool {\n" {
		t.Errorf("search_files = %q (refused %v), want the Go file's line and nothing from .git", got, refused)
	}
	got, _ = callRead(t, cs, "search_files", map[string]any{"pattern": "Overdue", "glob": "*.txt"})
	if got != "todo/overdue.txt:1: Overdue notes\n" {
		t.Errorf("search_files with a glob = %q", got)
	}
}

// Nothing outside the workspace is read: not by an absolute path, not by
// climbing out of it, not through a link that points out of it.
func TestTheReadServerRefusesEverythingOutsideTheWorkspace(t *testing.T) {
	dir, outside := readWorkspace(t)
	s := startTestReadServer(t, dir)
	cs := connectRead(t, s.url, s.token)
	for _, tc := range []struct {
		tool string
		args map[string]any
	}{
		{"read_file", map[string]any{"path": outside}},
		{"read_file", map[string]any{"path": "../secret.txt"}},
		{"read_file", map[string]any{"path": "escape.txt"}},
		{"read_file", map[string]any{"path": "/etc/hosts"}},
		{"list_directory", map[string]any{"path": ".."}},
		{"list_directory", map[string]any{"path": filepath.Dir(dir)}},
		{"search_files", map[string]any{"pattern": "outside", "path": ".."}},
		{"search_files", map[string]any{"pattern": "outside", "path": "escape.txt"}},
	} {
		got, refused := callRead(t, cs, tc.tool, tc.args)
		if !refused || strings.Contains(got, "outside the workspace") {
			t.Errorf("%s %v = %q (refused %v), want it refused", tc.tool, tc.args, got, refused)
		}
	}
	// A search of the whole workspace does not follow the link out either.
	if got, _ := callRead(t, cs, "search_files", map[string]any{"pattern": "outside"}); got != "[no matches]\n" {
		t.Errorf("search_files followed a link out of the workspace: %q", got)
	}
}

// Only the turn given the token reaches the server, and it is gone once
// the turn ends.
func TestTheReadServerAnswersOnlyItsTokenAndStopsWithTheTurn(t *testing.T) {
	dir, _ := readWorkspace(t)
	s, err := startReadServer(dir)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.Post(s.url, "application/json", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("a request without the token got %d, want 401", resp.StatusCode)
	}
	s.close()
	if resp, err := http.Post(s.url, "application/json", strings.NewReader("{}")); err == nil {
		_ = resp.Body.Close()
		t.Error("the read server still answers after the turn ended")
	}
}

// A restricted Codex turn reaches the read server through the MCP server
// it is given by url, with the token in its environment, and reads its
// workspace through it while the turn runs; the turn gets nothing else.
func TestRunRestrictedGivesCodexTheReadServer(t *testing.T) {
	sync := t.TempDir()
	ready, release := filepath.Join(sync, "ready"), filepath.Join(sync, "release")
	bin, record := restrictedFake(t, "codex", `touch "`+ready+`"
while [ ! -f "`+release+`" ]; do sleep 0.05; done
`+restrictedCodexAnswer)
	dir, _ := readWorkspace(t)
	done := make(chan error, 1)
	go func() {
		_, err := restrictedRunner(t, "", bin).RunRestricted(context.Background(), restrictedRequestFor(AgentCodex, dir))
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = os.WriteFile(release, nil, 0o644)
			t.Fatalf("the fake codex never started: %v", <-done)
		}
		time.Sleep(20 * time.Millisecond)
	}
	args := strings.Join(restrictedArgs(t, record, ".args"), "\n")
	m := regexp.MustCompile(`mcp_servers=\{"restricted_read"=\{url="(http://127\.0\.0\.1:\d+/mcp)",bearer_token_env_var="` + readServerTokenEnv + `"\}\}`).FindStringSubmatch(args)
	if m == nil {
		_ = os.WriteFile(release, nil, 0o644)
		t.Fatalf("codex was not given the read server:\n%s", args)
	}
	var token string
	for _, line := range restrictedArgs(t, record, ".env") {
		if v, ok := strings.CutPrefix(line, readServerTokenEnv+"="); ok {
			token = v
		}
	}
	if strings.Contains(args, token) {
		t.Error("the read server's token is on the command line")
	}
	cs := connectRead(t, m[1], token)
	got, refused := callRead(t, cs, "read_file", map[string]any{"path": "todo/overdue.go"})
	_ = os.WriteFile(release, nil, 0o644)
	if refused || !strings.Contains(got, "return !due.After(day)") {
		t.Errorf("read_file during the turn = %q (refused %v)", got, refused)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"features.shell_tool=false", "features.unified_exec=false", "features.image_generation=false", "--sandbox"} {
		if !strings.Contains(args, want) {
			t.Errorf("the read server came with less of the floor: %q missing", want)
		}
	}
}

// Claude has read tools of its own, so its restricted turn gets no read
// server.
func TestRunRestrictedGivesClaudeNoReadServer(t *testing.T) {
	bin, record := restrictedFake(t, "claude", restrictedClaudeAnswer)
	if _, err := restrictedRunner(t, bin, "").RunRestricted(context.Background(), restrictedRequestFor(AgentClaude, t.TempDir())); err != nil {
		t.Fatal(err)
	}
	env, _ := os.ReadFile(record + ".env")
	args, _ := os.ReadFile(record + ".args")
	if strings.Contains(string(env), readServerTokenEnv) || strings.Contains(string(args), ReadServerName) {
		t.Errorf("restricted Claude was given the read server:\n%s", args)
	}
}
