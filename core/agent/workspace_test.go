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

// A denied profile must not even inspect optional repository capabilities.
type deniedWorkspace struct{ dir string }

func (w deniedWorkspace) Directory() string { return w.dir }

func (w deniedWorkspace) VCS() *vcs.Access { panic("VCS inspected for a denied profile") }

func TestWorkspaceVCSMountPaths(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "work")
	metadata := filepath.Join(dir, ".git")
	if err := os.MkdirAll(metadata, 0755); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "metadata")
	if err := os.Symlink(metadata, alias); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{alias, metadata, dir} {
		for _, allowed := range []bool{false, true} {
			t.Run(filepath.Base(path)+map[bool]string{false: "/denied", true: "/allowed"}[allowed], func(t *testing.T) {
				req := Request{
					Workspace: fakeWorkspace{dir: dir, access: &vcs.Access{Mounts: []string{path, path}}},
					Profile:   Profile{VCSAccess: allowed},
				}
				// Without VCS the worktree, which holds .git, can only be
				// granted read-only.
				req.Grants = &Grants{Tools: []string{ToolsAll}, Mounts: []Mount{{Path: dir, Access: ReadOnly}}}
				if allowed {
					req.Grants = &Grants{Tools: []string{ToolsAll}, VCS: true, Mounts: []Mount{{Path: dir, Access: ReadWrite}, {Path: path, Access: ReadWrite}}}
				}
				c, err := verifiedContainer(t, &Runner{}, req, "")
				if err != nil {
					t.Fatal(err)
				}
				args := c.mounts()
				mounts := map[string]int{}
				for i := 0; i < len(args); i += 2 {
					mounts[args[i+1]]++
				}
				want := map[string]int{"type=bind,source=" + dir + ",destination=" + dir + ",readonly": 1}
				if allowed {
					want = map[string]int{"type=bind,source=" + dir + ",destination=" + dir: 1}
					// Every supplied path must resolve inside, even an alias whose
					// target is already reachable through the workspace mount.
					real, _ := filepath.EvalSymlinks(path)
					want["type=bind,source="+real+",destination="+path] = 1
					if path == alias {
						want["type=bind,source="+metadata+",destination="+metadata] = 1
					}
				}
				if len(mounts) != len(want) {
					t.Errorf("mounts=%v, want %v", mounts, want)
				}
				for spec, count := range want {
					if mounts[spec] != count {
						t.Errorf("mount %q appears %d times, want %d", spec, mounts[spec], count)
					}
				}
			})
		}
	}
}

func TestWorkspaceAccessContract(t *testing.T) {
	// If git discovery returns, fail on its side effect even if its error is ignored.
	gitDir := t.TempDir()
	marker := filepath.Join(t.TempDir(), "git-called")
	t.Setenv("GIT_DISCOVERY_MARKER", marker)
	if err := os.WriteFile(filepath.Join(gitDir, "git"), []byte("#!/bin/sh\ntouch \"$GIT_DISCOVERY_MARKER\"\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", gitDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	for _, allowed := range []bool{false, true} {
		for _, mode := range []string{SandboxNone, SandboxClaude, SandboxContainer} {
			t.Run(mode+map[bool]string{false: "/denied", true: "/allowed"}[allowed], func(t *testing.T) {
				dir, metadata, sessionDir := t.TempDir(), t.TempDir(), t.TempDir()
				var ws vcs.Workspace = deniedWorkspace{dir: dir}
				if allowed {
					ws = fakeWorkspace{dir: dir, access: &vcs.Access{Mounts: []string{metadata}}}
				}
				req := Request{Workspace: ws, SessionDir: sessionDir, Profile: Profile{Sandbox: mode, SandboxImage: "image", VCSAccess: allowed},
					VCSEnv:          map[string]string{"GIT_AUTHOR_NAME": "workspace-author", "GH_TOKEN": "workspace-token", "GIT_CONFIG_COUNT": "1", "GIT_CONFIG_KEY_0": "push.default", "GIT_CONFIG_VALUE_0": "current"},
					VCSContainerEnv: map[string]string{"GIT_CONFIG_VALUE_0": "container-value"},
					Env:             map[string]string{"DUMP": filepath.Join(sessionDir, "env"), "CWD_DUMP": filepath.Join(sessionDir, "cwd")}}
				r := Runner{ClaudeBin: agenttest.Script(t, "claude", `env > "$DUMP"
pwd > "$CWD_DUMP"
echo '{"type":"result","subtype":"success","result":"ok"}'`)}
				if mode == SandboxContainer {
					// Exercise the actual mount/command/environment construction, with no engine launch.
					granted := grantAll(req)
					if !allowed {
						// The fixture's directories hold no VCS metadata, so
						// they may be writable without VCS.
						granted.Grants.VCS = false
						granted.Grants.Env = slices.DeleteFunc(granted.Grants.Env, overlapsVCSEnv)
					}
					c, err := verifiedContainer(t, &r, granted, sessionDir)
					if err != nil {
						t.Fatal(err)
					}
					_, args, err := c.command(context.Background(), "claude", nil)
					if err != nil {
						t.Fatal(err)
					}
					mounted := strings.Contains(strings.Join(args, " ")+" ", ",destination="+metadata+" ")
					if mounted != allowed {
						t.Fatalf("VCS mount present=%v, allowed=%v: %v", mounted, allowed, args)
					}
					env := map[string]string{}
					for _, v := range dedupe(c.vars) {
						env[v.name] = v.value
					}
					if got := env["GH_TOKEN"]; (got == "workspace-token") != allowed {
						t.Fatalf("VCS credential=%q, allowed=%v", got, allowed)
					}
					want := ""
					if allowed {
						want = "container-value"
					}
					if env["GIT_CONFIG_VALUE_0"] != want {
						t.Fatalf("container config=%q, want=%q", env["GIT_CONFIG_VALUE_0"], want)
					}
				} else {
					mounts := []Mount{{Path: "/", Access: ReadWrite}}
					if mode == SandboxClaude {
						mounts = []Mount{{Path: "/", Access: ReadOnly}, {Path: dir, Access: ReadWrite}}
					}
					req.Grants = &Grants{Env: []string{"PATH", "DUMP", "CWD_DUMP", "GIT_*", "GH_TOKEN"}, Tools: []string{ToolsAll}, Mounts: mounts, VCS: allowed}
					if !allowed {
						req.Grants.Env = []string{"PATH", "DUMP", "CWD_DUMP"}
					}
					res, err := r.Run(context.Background(), req)
					if mode == SandboxNone && !allowed {
						// An unsandboxed host cannot keep VCS metadata unwritable.
						if !errors.Is(err, ErrUnsupported) {
							t.Fatalf("unsandboxed session without VCS: %v, want ErrUnsupported", err)
						}
						return
					}
					if err != nil || res.IsError {
						t.Fatalf("non-git workspace: %+v, %v", res, err)
					}
					data, err := os.ReadFile(req.Env["DUMP"])
					if err != nil {
						t.Fatal(err)
					}
					env := strings.Split(string(data), "\n")
					for _, value := range []string{"GIT_AUTHOR_NAME=workspace-author", "GH_TOKEN=workspace-token", "GIT_CONFIG_VALUE_0=current"} {
						if slices.Contains(env, value) != allowed {
							t.Errorf("%s exposure, allowed=%v", value, allowed)
						}
					}
					data, err = os.ReadFile(req.Env["CWD_DUMP"])
					if err != nil {
						t.Fatal(err)
					}
					real, _ := filepath.EvalSymlinks(dir)
					if strings.TrimSpace(string(data)) != real {
						t.Fatalf("ran in %q, want %q", data, real)
					}
				}
			})
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("core attempted git discovery: %v", err)
	}
}
