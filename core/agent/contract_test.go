package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/agent/procs"
	"github.com/kpenfound/busybees/core/vcs"
)

// These tests use Runner directly: no caller fixture supplies a namespace,
// outcome policy, role vocabulary, or built-in MCP server for them.
func TestCallerDefinedEnvironmentPrefix(t *testing.T) {
	for _, prefix := range []string{"OTHER_", "BUILD_CONTEXT_", ""} {
		t.Run(prefix, func(t *testing.T) {
			stale := prefix + "STALE"
			t.Setenv(stale, "outer")
			t.Setenv("UNRELATED_VALUE", "preserved")
			t.Setenv("UNGRANTED_VALUE", "dropped")
			dir := t.TempDir()
			bin := agenttest.Script(t, "claude", `env > "$DUMP"
echo '{"type":"result","subtype":"success","result":"ok"}'`)
			r := Runner{ClaudeBin: bin, EnvironmentPrefix: prefix}
			req := Request{SessionDir: dir, Workspace: fakeWorkspace{dir: t.TempDir()}, Profile: Profile{Name: "custom", Shell: "/bin/sh", Env: map[string]string{"SETTING": "profile"}}, Env: map[string]string{"DUMP": filepath.Join(dir, "env"), "SETTING": "request"}}
			res, err := r.Run(context.Background(), grantAll(req, stale, "UNRELATED_VALUE"))
			if err != nil || res.IsError {
				t.Fatalf("run: %+v, %v", res, err)
			}
			data, err := os.ReadFile(req.Env["DUMP"])
			if err != nil {
				t.Fatal(err)
			}
			env := strings.Split(string(data), "\n")
			if got := slices.Contains(env, stale+"=outer"); got != (prefix == "") {
				t.Errorf("inherited %s present=%v", stale, got)
			}
			if slices.Contains(env, "UNGRANTED_VALUE=dropped") {
				t.Error("inherited a variable outside the allowlist")
			}
			for _, want := range []string{"UNRELATED_VALUE=preserved", "SETTING=request", "SHELL=/bin/sh"} {
				if !slices.Contains(env, want) {
					t.Errorf("missing %s", want)
				}
			}
			if entries := readMCPConfig(t, dir); len(entries) != 0 {
				t.Errorf("runner invented MCP servers: %+v", entries)
			}
		})
	}
}

func TestCallerDefinedOutcomes(t *testing.T) {
	valid := []string{"shipped", "deferred"}
	dir := t.TempDir()
	out, err := Report(dir, "publisher", valid, Outcome{Status: " SHIPPED ", Note: "ready"})
	if err != nil || out.Status != "shipped" {
		t.Fatalf("report: %+v, %v", out, err)
	}
	if _, err := Report(dir, "publisher", valid, Outcome{Status: "unknown"}); err == nil || !strings.Contains(err.Error(), "shipped, deferred") {
		t.Fatalf("invalid outcome: %v", err)
	}
	for _, tc := range []struct {
		valid  []string
		accept bool
	}{{valid, true}, {[]string{"deferred"}, false}, {nil, true}, {[]string{}, false}} {
		r := Runner{ClaudeBin: agenttest.Script(t, "claude", `echo '{"type":"result","subtype":"success","result":"ok"}'`)}
		res, err := r.Run(context.Background(), grantAll(Request{Profile: Profile{Name: "publisher"}, Workspace: fakeWorkspace{dir: t.TempDir()}, SessionDir: dir, ValidOutcomes: tc.valid}))
		if err != nil {
			t.Fatal(err)
		}
		if res.HasOutcome != tc.accept {
			t.Errorf("statuses %v: HasOutcome=%v, want %v", tc.valid, res.HasOutcome, tc.accept)
		}
	}
}

func TestCanceledSessionRemainsInterrupted(t *testing.T) {
	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := Runner{ClaudeBin: agenttest.Script(t, "claude", `echo '{"type":"assistant"}'
exec sleep 60`)}
	ended := make(chan error, 1)
	go func() {
		_, err := r.Run(ctx, grantAll(Request{SessionDir: dir, Workspace: fakeWorkspace{dir: dir}, Profile: Profile{Name: "custom"}}))
		ended <- err
	}()
	deadline := time.Now().Add(5 * time.Second)
	for CountTurns(filepath.Join(dir, TranscriptFile)) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if err := <-ended; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ResultFile)); !os.IsNotExist(err) {
		t.Fatalf("result persisted after cancellation: %v", err)
	}
	in, running := CheckInterrupted("custom", dir, nil)
	if running || in == nil || in.Turns != 1 {
		t.Fatalf("interruption: %+v, running=%v", in, running)
	}
	if _, err := os.Stat(filepath.Join(dir, procs.PIDFile)); !os.IsNotExist(err) {
		t.Fatalf("pid survived: %v", err)
	}
}

func TestEveryBackendRunsThroughFakeContainer(t *testing.T) {
	for _, backend := range []string{AgentClaude, AgentCodex, AgentOpenCode} {
		t.Run(backend, func(t *testing.T) {
			dir := t.TempDir()
			body := `printf '%s\n' "$@" > "$RUN_DIR/args"
echo '{"type":"result","subtype":"success","result":"ok"}'`
			switch backend {
			case AgentCodex:
				body = `printf '%s\n' "$@" > "$RUN_DIR/args"
echo '{"type":"turn.completed"}'`
			case AgentOpenCode:
				body = `printf '%s\n' "$@" > "$RUN_DIR/args"
echo '{"type":"step_finish","part":{"reason":"stop"}}'`
			}
			bin := agenttest.Script(t, backend, body)
			r := Runner{ClaudeBin: bin, CodexBin: bin, OpenCodeBin: bin, DockerBin: agenttest.Docker(t, "image", "RUN_DIR"), ContainerLabel: "custom.session"}
			res, err := r.Run(context.Background(), grantAll(Request{SessionDir: dir, Workspace: fakeWorkspace{dir: t.TempDir()}, Profile: Profile{Name: "custom", Agent: backend, Sandbox: SandboxContainer, SandboxImage: "image"}, Env: map[string]string{"RUN_DIR": dir}}))
			if err != nil || res.IsError {
				t.Fatalf("container: %+v, %v", res, err)
			}
			data, err := os.ReadFile(filepath.Join(dir, "docker-args.txt"))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(data), "custom.session="+dir) || !strings.Contains(string(data), bin) {
				t.Errorf("container command: %s", data)
			}
			if procs.ContainerID(dir) != "" {
				t.Error("container id survived cleanup")
			}
		})
	}
}

func TestContainerEngineGuard(t *testing.T) {
	real, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	r := Runner{DockerBin: real}
	_, err = r.Run(context.Background(), grantAll(Request{SessionDir: dir, Workspace: fakeWorkspace{dir: dir}, Profile: Profile{Sandbox: SandboxContainer, SandboxImage: "image"}}))
	if !errors.Is(err, agentbin.ErrRealAgent) {
		t.Fatalf("unguarded engine: %v", err)
	}
}

func TestContainerAgentGuard(t *testing.T) {
	real, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	r := Runner{ClaudeBin: real, DockerBin: agenttest.Docker(t, "image", "RUN_DIR")}
	_, err = r.Run(context.Background(), grantAll(Request{SessionDir: dir, Workspace: fakeWorkspace{dir: dir},
		Env:     map[string]string{"RUN_DIR": dir},
		Profile: Profile{Sandbox: SandboxContainer, SandboxImage: "image"},
	}))
	if !errors.Is(err, agentbin.ErrRealAgent) {
		t.Fatalf("unguarded container agent: %v", err)
	}
}

func TestVCSAccessControlsSharedGitMount(t *testing.T) {
	metadata, worktree := workspaceFixture(t)
	for _, access := range []bool{true, false} {
		c, err := verifiedContainer(t, &Runner{}, grantAll(Request{Workspace: fakeWorkspace{dir: worktree, access: &vcs.Access{Mounts: []string{metadata}}}, Profile: Profile{Sandbox: SandboxContainer, VCSAccess: access}}), "")
		if err != nil {
			t.Fatal(err)
		}
		mounts := c.mounts()
		gitDir := metadata
		exposed := strings.Contains(strings.Join(mounts, " ")+" ", ",destination="+gitDir+" ")
		if exposed != access {
			t.Errorf("VCSAccess=%v: shared .git exposed=%v", access, exposed)
		}
	}
}

func TestContainerEnvironmentIsIsolated(t *testing.T) {
	t.Setenv("HOST_ONLY", "host-value")
	t.Setenv("ANTHROPIC_API_KEY", "host-credential")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	r := Runner{}
	req := grantAll(Request{Workspace: fakeWorkspace{dir: t.TempDir()}, Profile: Profile{Sandbox: SandboxContainer, Env: map[string]string{"EXPANDED": "$HOST_ONLY", "ANTHROPIC_API_KEY": "profile-credential"}, Shell: "/bin/bash"}, Env: map[string]string{"CONTEXT": "caller"}, ContainerEnv: map[string]string{"CONTEXT": "container"}}, "USER")
	c, err := verifiedContainer(t, &r, req, "")
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, v := range c.vars {
		env[v.name] = v.value
	}
	for key, want := range map[string]string{"EXPANDED": "host-value", "ANTHROPIC_API_KEY": "profile-credential", "SHELL": "/bin/bash", "CONTEXT": "container", "HOME": "/home/agent"} {
		if env[key] != want {
			t.Errorf("%s=%q, want %q", key, env[key], want)
		}
	}
	for _, key := range []string{"HOST_ONLY", "PATH", "USER", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if _, ok := env[key]; ok {
			t.Errorf("inherited host variable %s", key)
		}
	}
}

// Agents is procs' list of agent executables, so an agent this package
// names must be in it: a backend missing from the list is one no caller
// accepts in its configuration and procs takes for a stranger's process.
func TestAgentsHoldsEveryAgentThisPackageNames(t *testing.T) {
	for _, a := range []string{AgentClaude, AgentCodex, AgentOpenCode} {
		if !slices.Contains(Agents, a) {
			t.Errorf("agent %s is not in Agents (%v)", a, Agents)
		}
	}
}
