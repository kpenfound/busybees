package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/vcs"
)

// realTemp is a temporary directory with its symbolic links resolved.
func realTemp(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// boxedRequest is a claude-sandbox request granted its working directory.
func boxedRequest(t *testing.T) Request {
	dir := realTemp(t)
	return Request{
		Workspace: vcs.Directory(dir),
		Profile:   Profile{Name: "worker", Sandbox: SandboxClaude},
		Grants: &Grants{
			Env:    []string{"PATH"},
			Tools:  []string{"Read", "Edit"},
			Mounts: []Mount{{Path: "/", Access: ReadOnly}, {Path: dir, Access: ReadWrite}},
		},
	}
}

var hostEnviron = []string{
	"PATH=/usr/bin",
	"HOME=/home/someone",
	"GH_TOKEN=gh-secret",
	"GITHUB_TOKEN=github-secret",
	"GIT_ASKPASS=/askpass",
	"SSH_AUTH_SOCK=/agent.sock",
	"ANTHROPIC_API_KEY=provider-secret",
	"DELIVERY_TOKEN=delivery-secret",
	"OWN_STALE=stale",
}

func TestHostEnvironmentIsOnlyTheAllowlist(t *testing.T) {
	req := boxedRequest(t)
	req.Grants.Env = []string{"PATH", "HOME", "SETTING", "OWN_*"}
	req.Env = map[string]string{"SETTING": "request"}
	h := HostBoundary{Environ: func() []string { return hostEnviron }, StripPrefix: "OWN_"}
	turn, err := h.Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"PATH=/usr/bin", "HOME=/home/someone", "SETTING=request"}
	if !slices.Equal(turn.Env, want) {
		t.Fatalf("env = %q, want %q", turn.Env, want)
	}
}

func TestVCSCredentialsNeedTheVCSGrant(t *testing.T) {
	for _, entry := range []string{"GH_TOKEN", "GITHUB_TOKEN", "GIT_ASKPASS", "SSH_AUTH_SOCK", "G*", "GIT*", "GH_*", "SSH_*"} {
		t.Run(entry, func(t *testing.T) {
			req := boxedRequest(t)
			req.Grants.Env = []string{"PATH", entry}
			if _, err := (HostBoundary{Environ: func() []string { return hostEnviron }}).Verify(req); !errors.Is(err, ErrNotGranted) {
				t.Fatalf("allowlist %q without VCS: %v, want ErrNotGranted", entry, err)
			}
		})
	}
	req := boxedRequest(t)
	req.Grants.Env = []string{"PATH", "GH_TOKEN", "SSH_AUTH_SOCK"}
	req.Grants.VCS = true
	turn, err := (HostBoundary{Environ: func() []string { return hostEnviron }}).Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	// Granted, but the profile did not ask for VCS: still scrubbed.
	if slices.Contains(turn.Env, "GH_TOKEN=gh-secret") || !slices.Contains(turn.DeniedExecutables, "git") {
		t.Fatalf("VCS narrowed by the profile leaked: %+v", turn)
	}
	req.Profile.VCSAccess = true
	turn, err = (HostBoundary{Environ: func() []string { return hostEnviron }}).Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"GH_TOKEN=gh-secret", "SSH_AUTH_SOCK=/agent.sock"} {
		if !slices.Contains(turn.Env, want) {
			t.Errorf("granted %s missing from %q", want, turn.Env)
		}
	}
	if len(turn.DeniedExecutables) != 0 {
		t.Errorf("VCS granted but executables denied: %v", turn.DeniedExecutables)
	}
}

func TestVCSAccessWithoutTheGrantIsRefused(t *testing.T) {
	req := boxedRequest(t)
	req.Profile.VCSAccess = true
	if _, err := (HostBoundary{}).Verify(req); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("VCS access without grant: %v", err)
	}
}

func TestSessionVariablesMustBeGranted(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"request": func(r *Request) { r.Env = map[string]string{"EXTRA": "x"} },
		"profile": func(r *Request) { r.Profile.Env = map[string]string{"EXTRA": "x"} },
		"shell":   func(r *Request) { r.Profile.Shell = "/bin/sh" },
		"mcp": func(r *Request) {
			r.Grants.Tools = append(r.Grants.Tools, "mcp__tools")
			r.Profile.MCP = map[string]MCPEntry{"tools": {Command: "x", EnvVars: []string{"TOOLS_TOKEN"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			req := boxedRequest(t)
			mutate(&req)
			if _, err := (HostBoundary{}).Verify(req); !errors.Is(err, ErrNotGranted) {
				t.Fatalf("ungranted variable: %v", err)
			}
		})
	}
	req := boxedRequest(t)
	req.Grants.Env = []string{"*"}
	if _, err := (HostBoundary{}).Verify(req); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("a wildcard is not an allowlist: %v", err)
	}
}

func TestToolsNarrowButNeverWiden(t *testing.T) {
	narrow := []func(*Request){
		func(r *Request) { r.Profile.AllowedTools = []string{"Read"} },
		func(r *Request) { r.Profile.AllowedTools = []string{"Edit(src/**)"} },
		func(r *Request) { r.Profile.DisallowedTools = []string{"Bash", "Read"} },
		func(r *Request) {
			r.Grants.Tools = append(r.Grants.Tools, "mcp__docs", "mcp__tools")
			r.Profile.MCP = map[string]MCPEntry{"docs": {Command: "x"}}
			r.Profile.AllowedTools = []string{"mcp__docs__search"}
		},
	}
	for i, mutate := range narrow {
		req := boxedRequest(t)
		mutate(&req)
		if _, err := (HostBoundary{}).Verify(req); err != nil {
			t.Errorf("narrowing %d refused: %v", i, err)
		}
	}
	widen := map[string]func(*Request){
		"allowed tool":     func(r *Request) { r.Profile.AllowedTools = []string{"Bash(git:*)"} },
		"mcp server":       func(r *Request) { r.Profile.MCP = map[string]MCPEntry{"docs": {Command: "x"}} },
		"allowed mcp tool": func(r *Request) { r.Profile.AllowedTools = []string{"mcp__docs__search"} },
		"host mcp":         func(r *Request) { r.HostMCP = &HostMCP{Name: "tools"} },
		"all tools": func(r *Request) {
			r.Grants.Tools = []string{"Read", "mcp__*"}
			r.Profile.MCP = map[string]MCPEntry{"x": {}}
		},
		"mcp tool as a grant": func(r *Request) { r.Grants.Tools = []string{"mcp__docs__search"} },
	}
	for name, mutate := range widen {
		t.Run(name, func(t *testing.T) {
			req := boxedRequest(t)
			mutate(&req)
			if _, err := (HostBoundary{}).Verify(req); err == nil {
				t.Fatal("widening accepted")
			}
		})
	}
}

func TestToolGrantsReachClaude(t *testing.T) {
	req := boxedRequest(t)
	extra := realTemp(t)
	req.Grants.Mounts = append(req.Grants.Mounts, Mount{Path: extra, Access: ReadWrite})
	req.Grants.Env = append(req.Grants.Env, "ARGS")
	dir := realTemp(t)
	req.SessionDir = dir
	req.Env = map[string]string{"ARGS": filepath.Join(dir, "args")}
	r := Runner{ClaudeBin: agenttest.Script(t, "claude", `printf '%s\n' "$@" > "$ARGS"
echo '{"type":"result","subtype":"success","result":"ok"}'`)}
	res, err := r.Run(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	data, err := os.ReadFile(req.Env["ARGS"])
	if err != nil {
		t.Fatal(err)
	}
	args := string(data)
	if !strings.Contains(args, "--tools\nRead,Edit\n") {
		t.Errorf("tools not restricted:\n%s", args)
	}
	if !strings.Contains(args, "--add-dir\n"+extra+"\n") {
		t.Errorf("writable grant not given to the box:\n%s", args)
	}
	if !strings.Contains(args, `"deny":["Bash(gh:*)","Bash(git:*)"`) {
		t.Errorf("VCS executables not denied in settings:\n%s", args)
	}

	// Every built-in tool: no --tools at all.
	req.Grants.Tools = []string{ToolsAll}
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	data, _ = os.ReadFile(req.Env["ARGS"])
	if strings.Contains(string(data), "--tools") {
		t.Errorf("all tools granted but restricted:\n%s", data)
	}
}

func TestReadOnlyAndReadWriteGrants(t *testing.T) {
	req := boxedRequest(t)
	dir := req.workDir()
	req.Grants.Mounts = []Mount{{Path: "/", Access: ReadOnly}, {Path: dir, Access: ReadOnly}}
	if _, err := (HostBoundary{}).Verify(req); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("read-only working directory in claude's box: %v", err)
	}
	req.Grants.Mounts = []Mount{{Path: "/", Access: ReadOnly}, {Path: dir, Access: "rwx"}}
	if _, err := (HostBoundary{}).Verify(req); err == nil {
		t.Fatal("unknown access mode accepted")
	}

	state := realTemp(t)
	req = boxedRequest(t)
	if _, err := (HostBoundary{AddDirs: []string{state}}).Verify(req); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("writable directory without a rw grant: %v", err)
	}
	req.Grants.Mounts = append(req.Grants.Mounts, Mount{Path: state, Access: ReadOnly})
	if _, err := (HostBoundary{AddDirs: []string{state}}).Verify(req); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("writable directory with a ro grant: %v", err)
	}
	req.Grants.Mounts[len(req.Grants.Mounts)-1].Access = ReadWrite
	turn, err := (HostBoundary{AddDirs: []string{state}}).Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(turn.WriteDirs, []string{state}) {
		t.Fatalf("write dirs = %v", turn.WriteDirs)
	}
}

func TestPathAndSymlinkEscape(t *testing.T) {
	root := realTemp(t)
	outside := realTemp(t)
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"symlink":  link,
		"dotdot":   filepath.Join(root, "work") + "/../../" + filepath.Base(outside),
		"relative": "work",
		"missing":  filepath.Join(root, "missing"),
		"outside":  outside,
	}
	for name, path := range cases {
		t.Run(name, func(t *testing.T) {
			req := Request{Workspace: vcs.Directory(work), Profile: Profile{Sandbox: SandboxClaude}, Grants: &Grants{
				Tools:  []string{ToolsAll},
				Within: root,
				Mounts: []Mount{{Path: work, Access: ReadWrite}, {Path: path, Access: ReadOnly}},
			}}
			if _, err := verifyCommon(req); err == nil {
				t.Fatalf("mount %s accepted", path)
			}
		})
	}
	// A working directory reached through a link is judged where it lands.
	req := Request{Workspace: vcs.Directory(link), Profile: Profile{Sandbox: SandboxClaude}, Grants: &Grants{
		Tools:  []string{ToolsAll},
		Mounts: []Mount{{Path: "/", Access: ReadOnly}, {Path: work, Access: ReadWrite}},
	}}
	if _, err := (HostBoundary{}).Verify(req); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("working directory escaping its rw grant through a link: %v", err)
	}
	req.Grants.Mounts = []Mount{{Path: work, Access: ReadWrite}}
	if _, err := verifyCommon(req); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("working directory outside every mount: %v", err)
	}
}

func TestWritableVCSMetadataIsDenied(t *testing.T) {
	for _, layout := range []string{"dir", "file", "inside"} {
		t.Run(layout, func(t *testing.T) {
			root := realTemp(t)
			work := root
			switch layout {
			case "dir":
				if err := os.Mkdir(filepath.Join(root, ".git"), 0o755); err != nil {
					t.Fatal(err)
				}
			case "file":
				if err := os.WriteFile(filepath.Join(root, ".git"), []byte("gitdir: /elsewhere\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "inside":
				work = filepath.Join(root, ".git", "hooks")
				if err := os.MkdirAll(work, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			req := Request{Workspace: vcs.Directory(work), Profile: Profile{Sandbox: SandboxClaude}, Grants: &Grants{
				Tools:  []string{ToolsAll},
				Mounts: []Mount{{Path: "/", Access: ReadOnly}, {Path: work, Access: ReadWrite}},
			}}
			if _, err := (HostBoundary{}).Verify(req); !errors.Is(err, ErrNotGranted) {
				t.Fatalf("writable .git without VCS: %v", err)
			}
			req.Grants.Mounts[1].Access = ReadOnly
			if _, err := verifyCommon(req); err != nil {
				t.Fatalf("read-only .git refused: %v", err)
			}
			req.Grants.Mounts[1].Access = ReadWrite
			req.Grants.VCS, req.Profile.VCSAccess = true, true
			if _, err := (HostBoundary{}).Verify(req); err != nil {
				t.Fatalf("writable .git with VCS refused: %v", err)
			}
		})
	}
}

func TestDeniedVCSExecutablesAreShadowed(t *testing.T) {
	req := boxedRequest(t)
	dir := realTemp(t)
	req.SessionDir = dir
	req.Grants.Env = append(req.Grants.Env, "OUT")
	req.Env = map[string]string{"OUT": filepath.Join(dir, "out")}
	r := Runner{ClaudeBin: agenttest.Script(t, "claude", `set +e
git status 2>/dev/null
echo "git=$?" > "$OUT"
echo '{"type":"result","subtype":"success","result":"ok"}'`)}
	if _, err := r.Run(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(req.Env["OUT"])
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(data)) != "git=126" {
		t.Fatalf("git ran for a session without VCS: %s", data)
	}
}

func TestUnsupportedConfigurationsAreRefusedBeforeLaunch(t *testing.T) {
	cases := map[string]func(*Request){
		"no grants": func(r *Request) { r.Grants = nil },
		"codex restricted tools": func(r *Request) {
			r.Profile.Sandbox, r.Profile.Agent = SandboxNone, AgentCodex
			r.Grants.VCS, r.Profile.VCSAccess = true, true
			r.Grants.Mounts[0].Access = ReadWrite
		},
		"unsandboxed without rw /": func(r *Request) { r.Profile.Sandbox = SandboxNone; r.Grants.VCS, r.Profile.VCSAccess = true, true },
		"unsandboxed without VCS":  func(r *Request) { r.Profile.Sandbox = SandboxNone; r.Grants.Mounts[0].Access = ReadWrite },
		"box without ro /":         func(r *Request) { r.Grants.Mounts = r.Grants.Mounts[1:] },
		"box with rw /":            func(r *Request) { r.Grants.Mounts[0].Access = ReadWrite },
		"no working directory":     func(r *Request) { r.Workspace = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			req := boxedRequest(t)
			mutate(&req)
			ran := filepath.Join(realTemp(t), "ran")
			r := Runner{ClaudeBin: agenttest.Script(t, "claude", `touch `+ran+`
echo '{"type":"result","subtype":"success","result":"ok"}'`), CodexBin: agenttest.Script(t, "codex", `touch `+ran), SessionsDir: realTemp(t)}
			if _, err := r.Run(context.Background(), req); err == nil {
				t.Fatal("unsupported configuration ran")
			}
			if _, err := os.Stat(ran); !os.IsNotExist(err) {
				t.Fatal("agent started before verification")
			}
			if entries, _ := os.ReadDir(r.SessionsDir); len(entries) != 0 {
				t.Fatalf("session directory created before verification: %v", entries)
			}
		})
	}
	req := boxedRequest(t)
	if _, err := (HostBoundary{}).Verify(req); err != nil {
		t.Fatalf("baseline refused: %v", err)
	}
	if _, err := (HostBoundary{}).Verify(Request{Workspace: req.Workspace}); !errors.Is(err, ErrNoGrants) {
		t.Fatalf("no grants: %v", err)
	}
}
