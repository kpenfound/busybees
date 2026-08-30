package doctor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/skills"
	"github.com/kpenfound/busybees/internal/workspace"
)

// TestMain doubles as the fake MCP server the role checks are pointed at, the
// way internal/scheduler's fake claude does: $FAKE_MCP turns the test binary
// into a server instead of a test run, so nothing outside the test tree is
// started. The real claude and gh are never involved either way.
func TestMain(m *testing.M) {
	switch os.Getenv("FAKE_MCP") {
	case "":
		os.Exit(m.Run())
	case "ok":
		srv := mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
		if err := srv.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	case "silent":
		// Reads every request and answers none: initialize never completes.
		_, _ = io.Copy(io.Discard, os.Stdin)
		os.Exit(0)
	default:
		os.Exit(1)
	}
}

// testBinary is the path of the running test binary, used as the command of
// the fake MCP servers below.
func testBinary(t *testing.T) string {
	t.Helper()
	p, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// roleFixture is a fixture whose skills manager caches under a temp directory
// and records every git invocation, so a test can assert that a check did (or
// did not) reach for the network.
type roleFixture struct {
	*fixture
	gitCalls *int
}

func setupRoles(t *testing.T, extra string, replies map[string]ghReply) *roleFixture {
	t.Helper()
	t.Setenv("BEES_CACHE_DIR", t.TempDir())
	f := setup(t, extra, replies)
	calls := 0
	real := f.Skills.Git
	f.Skills.Git = func(ctx context.Context, dir string, args ...string) (string, error) {
		calls++
		return real(ctx, dir, args...)
	}
	return &roleFixture{fixture: f, gitCalls: &calls}
}

// roleCheck returns the single role check whose name matches, so a test does
// not depend on the position of the role checks in the set.
func (f *roleFixture) roleCheck(t *testing.T, name string) Check {
	t.Helper()
	var found []string
	for _, c := range f.Checks() {
		if !c.Expensive {
			continue
		}
		r := c.Run(context.Background())
		found = append(found, r.Name)
		if r.Name == name {
			return c
		}
	}
	t.Fatalf("no role check named %q; got %v", name, found)
	return Check{}
}

// ---- skills ----------------------------------------------------------------

// skillRepo makes a git repository holding one skill and returns its path,
// which is a usable git URL. Nothing here touches the network.
func skillRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# tdd\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@e.st", "-c", "user.name=t", "add", "SKILL.md"},
		{"-c", "user.email=t@e.st", "-c", "user.name=t", "commit", "-qm", "skill"},
	} {
		if _, err := workspace.Git(ctx, dir, args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	return dir
}

func TestRoleSkillsClone(t *testing.T) {
	repo := skillRepo(t)
	f := setupRoles(t, fmt.Sprintf("[roles.developer]\nskills = [%q]\n", repo), nil)
	role, err := f.Config.Role(config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	r := f.run(t, f.checkRoleSkills(role))
	wantResult(t, r, Pass, "1 skill ready", repo)
	if r.Group != GroupRoles || r.Name != "developer skills" {
		t.Errorf("result should name the role: %+v", r)
	}
	if *f.gitCalls == 0 {
		t.Error("the check should have cloned through the skills manager")
	}
}

func TestRoleSkillsThatDoesNotCloneIsAFailure(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "no-such-skill.git")
	second := filepath.Join(dir, "also-missing.git")
	good := skillRepo(t)
	f := setupRoles(t, fmt.Sprintf("[roles.reviewer]\nskills = [%q, %q, %q]\n", first, good, second), nil)
	role, err := f.Config.Role(config.RoleReviewer)
	if err != nil {
		t.Fatal(err)
	}
	r := f.run(t, f.checkRoleSkills(role))
	// The URL that failed and git's own complaint, so the line is actionable.
	// Both broken references are reported: the check prepares them one at a
	// time, so the first failure does not hide the ones behind it.
	wantResult(t, r, Fail, first, second, "reviewer")
	if strings.Contains(r.Detail, good) {
		t.Errorf("only the broken references belong in the detail: %q", r.Detail)
	}
}

// ---- MCP servers -----------------------------------------------------------

func mcpTOML(t *testing.T, role, mode string) string {
	t.Helper()
	return fmt.Sprintf("[roles.%s.mcp.probe]\ncommand = %q\nenv = { FAKE_MCP = %q }\n",
		role, testBinary(t), mode)
}

func TestRoleMCPServerAnswersInitialize(t *testing.T) {
	f := setupRoles(t, mcpTOML(t, "qa", "ok"), nil)
	role, err := f.Config.Role(config.RoleQA)
	if err != nil {
		t.Fatal(err)
	}
	r := f.run(t, f.checkRoleMCP(role))
	wantResult(t, r, Pass, "probe")
	if r.Name != "qa mcp" {
		t.Errorf("result should name the role: %+v", r)
	}
}

func TestRoleMCPServerThatNeverAnswersTimesOut(t *testing.T) {
	f := setupRoles(t, mcpTOML(t, "developer", "silent"), nil)
	role, err := f.Config.Role(config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	// probeMCP caps its own wait at MCPTimeout, but it honours a shorter
	// deadline from its caller: that is what keeps this test quick instead of
	// sitting out the real 15s budget.
	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	done := make(chan Result, 1)
	go func() { done <- f.checkRoleMCP(role)(ctx) }()
	select {
	case r := <-done:
		wantResult(t, r, Fail, "probe", "developer")
	case <-time.After(30 * time.Second):
		t.Fatal("the check hung: an unresponsive MCP server must time out")
	}
}

func TestRoleMCPServerThatCannotBeStarted(t *testing.T) {
	f := setupRoles(t, fmt.Sprintf("[roles.developer.mcp.gone]\ncommand = %q\n",
		filepath.Join(t.TempDir(), "no-such-binary")), nil)
	role, err := f.Config.Role(config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	wantResult(t, f.run(t, f.checkRoleMCP(role)), Fail, "gone")
}

func TestRoleMCPCheckGetsTheServersOwnBudget(t *testing.T) {
	f := setupRoles(t, mcpTOML(t, "developer", "ok"), nil)
	c := f.roleCheck(t, "developer mcp")
	if c.Timeout < MCPTimeout {
		t.Errorf("the mcp check must be given at least MCPTimeout (%s), got %s", MCPTimeout, c.Timeout)
	}
}

// ---- shell -----------------------------------------------------------------

func TestRoleShell(t *testing.T) {
	sh := filepath.Join(t.TempDir(), "shell")
	if err := os.WriteFile(sh, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	f := setupRoles(t, fmt.Sprintf("[roles.developer]\nshell = %q\n", sh), nil)
	role, err := f.Config.Role(config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	wantResult(t, f.run(t, f.checkRoleShell(role)), Pass, sh)
}

// TestRoleShellThatCannotBeRun covers the failure config validation does not:
// it stats the shell (so a missing one never loads) but ignores the mode.
func TestRoleShellThatCannotBeRun(t *testing.T) {
	notExec := filepath.Join(t.TempDir(), "not-executable")
	if err := os.WriteFile(notExec, []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	f := setupRoles(t, fmt.Sprintf("[global]\nshell = %q\n", notExec), nil)
	role, err := f.Config.Role(config.RoleQA)
	if err != nil {
		t.Fatal(err)
	}
	r := f.run(t, f.checkRoleShell(role))
	wantResult(t, r, Fail, "not executable", "roles.qa.shell")
	if r.Name != "qa shell" {
		t.Errorf("result should name the role: %+v", r)
	}

	// And the shell disappearing under a loaded configuration.
	if err := os.Remove(notExec); err != nil {
		t.Fatal(err)
	}
	wantResult(t, f.run(t, f.checkRoleShell(role)), Fail, "no such file")
}

// ---- the role set ----------------------------------------------------------

func TestDisabledRoleIsSkippedNotDropped(t *testing.T) {
	f := setupRoles(t, "[roles.qa]\nenabled = false\nskills = [\"/nope\"]\n", nil)
	var got *Result
	for _, r := range Run(context.Background(), f.roleChecks()) {
		if strings.HasPrefix(r.Name, config.RoleQA) {
			if got != nil {
				t.Fatalf("a disabled role gets one row, got %q and %q", got.Name, r.Name)
			}
			r := r
			got = &r
		}
	}
	if got == nil {
		t.Fatal("a disabled role must still be reported, not dropped")
	}
	wantResult(t, *got, Pass, "disabled")
	if *f.gitCalls != 0 {
		t.Errorf("a disabled role's skills must not be cloned (%d git calls)", *f.gitCalls)
	}
}

func TestEveryRoleIsReported(t *testing.T) {
	f := setupRoles(t, "", nil)
	seen := map[string]bool{}
	for _, r := range Run(context.Background(), f.roleChecks()) {
		if r.Group != GroupRoles {
			t.Errorf("%s: group %q, want %q", r.Name, r.Group, GroupRoles)
		}
		seen[strings.Fields(r.Name)[0]] = true
	}
	for _, role := range config.Roles {
		if !seen[role] {
			t.Errorf("no check reported role %q", role)
		}
	}
}

// TestPreflightLeavesTheSkillsManagerAlone is the `bees run` half: the cheap
// subset it runs must never clone a skill or start an MCP server, and the full
// set must.
func TestPreflightLeavesTheSkillsManagerAlone(t *testing.T) {
	repo := skillRepo(t)
	f := setupRoles(t, fmt.Sprintf("[roles.developer]\nskills = [%q]\n", repo), map[string]ghReply{
		"auth status": {out: "  - Token scopes: 'repo'"},
		"repo view":   {out: `{"viewerPermission":"WRITE"}`},
		"label list":  {out: "[]"},
		"issue list":  {out: "[]"},
	})
	f.Workspaces.Root = t.TempDir()
	checks := f.Checks()
	cheap := CheapChecks(checks)
	if len(cheap) >= len(checks) {
		t.Fatalf("the role checks must be marked expensive: %d cheap of %d", len(cheap), len(checks))
	}
	Run(context.Background(), cheap)
	if *f.gitCalls != 0 {
		t.Errorf("the preflight cloned skills: %d git calls through the skills manager", *f.gitCalls)
	}
	for _, r := range Run(context.Background(), checks) {
		if r.Group == GroupRoles && r.Status == Fail {
			t.Errorf("full run: %+v", r)
		}
	}
	if *f.gitCalls == 0 {
		t.Error("the full run must clone the configured skills")
	}
}

func TestSkillsManagerUsesTheSharedCache(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BEES_CACHE_DIR", dir)
	f := setup(t, "", nil)
	if f.Skills == nil {
		t.Fatal("New must build a skills manager")
	}
	if f.Skills.CacheDir != skills.CacheDir() || f.Skills.CacheDir != dir {
		t.Errorf("skills cache %q, want the shared one %q", f.Skills.CacheDir, dir)
	}
}

// TestRoleHTTPMCPServer covers the http transport and the configured headers:
// the server answers only when the header the configuration carries is there,
// so a probe that dropped them would fail.
func TestRoleHTTPMCPServer(t *testing.T) {
	const token = "s3cret"
	handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcp.NewServer(&mcp.Implementation{Name: "fake", Version: "1"}, nil)
	}, nil)
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Token") != token {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		sawHeader = true
		handler.ServeHTTP(w, r)
	}))
	defer srv.Close()

	f := setupRoles(t, fmt.Sprintf("[roles.developer.mcp.remote]\ntype = \"http\"\nurl = %q\nheaders = { X-Token = %q }\n",
		srv.URL, token), nil)
	role, err := f.Config.Role(config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	wantResult(t, f.run(t, f.checkRoleMCP(role)), Pass, "remote")
	if !sawHeader {
		t.Error("the probe must send the configured headers")
	}

	// And an endpoint nobody is listening on is a failure naming the server.
	srv.Close()
	wantResult(t, f.run(t, f.checkRoleMCP(role)), Fail, "remote")
}
