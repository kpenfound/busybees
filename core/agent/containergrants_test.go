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

// realTempDir is a temporary directory by its real path.
func realTempDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// containerRequest is a container request for work and session with the
// given grants.
func containerRequest(work, session string, g *Grants) Request {
	return Request{Name: "c", Workspace: fakeWorkspace{dir: work}, SessionDir: session, Grants: g,
		Profile: Profile{Name: "builder", Sandbox: SandboxContainer, SandboxImage: "image"}}
}

// The container's environment is the allowlist's: a host variable is never
// inherited, not even a granted one; an agent credential is forwarded only
// when granted; a variable set for the container must be granted.
func TestContainerEnvironmentIsTheAllowlist(t *testing.T) {
	work, session := realTempDir(t), realTempDir(t)
	environ := func() []string {
		return []string{"PATH=/host/bin", "ANTHROPIC_API_KEY=sk-host", "CLAUDE_CODE_OAUTH_TOKEN=oauth-host", "GH_TOKEN=gh-host", "SSH_AUTH_SOCK=/agent"}
	}
	b := ContainerBoundary{Environ: environ, Home: "/home/box"}
	g := &Grants{Env: []string{"PATH", "ANTHROPIC_API_KEY", "CONTEXT"}, Tools: []string{ToolsAll},
		Mounts: []Mount{{Path: work, Access: ReadWrite}, {Path: session, Access: ReadWrite}}}
	req := containerRequest(work, session, g)
	req.ContainerEnv = map[string]string{"CONTEXT": "inside"}
	turn, err := b.Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ANTHROPIC_API_KEY=sk-host", "CONTEXT=inside", "HOME=/home/box"}
	if !slices.Equal(turn.Env, want) {
		t.Errorf("container env = %v, want %v", turn.Env, want)
	}

	req.ContainerEnv = map[string]string{"CONTEXT": "inside", "EXTRA": "x"}
	if _, err := b.Verify(req); !errors.Is(err, ErrNotGranted) || !strings.Contains(err.Error(), "EXTRA") {
		t.Errorf("ungranted container variable: %v, want ErrNotGranted naming it", err)
	}

	// VCS configuration reaches the container only with VCS granted and
	// asked for, and must be in the allowlist then too.
	req.ContainerEnv = nil
	req.VCSContainerEnv = map[string]string{"GIT_CONFIG_COUNT": "1"}
	turn, err = b.Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(turn.Env, func(kv string) bool { return strings.HasPrefix(kv, "GIT_") }) {
		t.Errorf("VCS configuration without VCS: %v", turn.Env)
	}
	req.Profile.VCSAccess = true
	req.Grants = &Grants{Env: g.Env, Tools: g.Tools, Mounts: g.Mounts, VCS: true}
	if _, err := b.Verify(req); !errors.Is(err, ErrNotGranted) {
		t.Errorf("ungranted VCS configuration: %v, want ErrNotGranted", err)
	}
	req.Grants.Env = append(slices.Clone(g.Env), "GIT_*")
	if turn, err = b.Verify(req); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(turn.Env, "GIT_CONFIG_COUNT=1") || slices.Contains(turn.Env, "GH_TOKEN=gh-host") {
		t.Errorf("VCS container env = %v", turn.Env)
	}
}

// The container is given the granted mounts with their access, each at its
// real path and the path it was granted by, and nothing else.
func TestContainerBindsAreTheGrants(t *testing.T) {
	base := realTempDir(t)
	work, session, cache, state := filepath.Join(base, "work"), filepath.Join(base, "session"), filepath.Join(base, "cache"), filepath.Join(base, "state")
	for _, d := range []string{work, session, cache, state} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(realTempDir(t), "state-link")
	if err := os.Symlink(state, link); err != nil {
		t.Fatal(err)
	}
	b := ContainerBoundary{MountDirs: []string{state}, SkillMountDirs: []string{cache}}
	g := &Grants{Tools: []string{ToolsAll}, Mounts: []Mount{
		{Path: work, Access: ReadWrite}, {Path: session, Access: ReadWrite},
		{Path: cache, Access: ReadOnly}, {Path: link, Access: ReadWrite},
	}}
	req := containerRequest(work, session, g)
	req.Profile.Skills = []string{"skill"}
	req.Profile.VCSAccess = false
	turn, err := b.Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	c := &container{r: &Runner{}, req: req, sessionDir: session, turn: turn}
	got := strings.Join(c.mounts(), " ")
	want := strings.Join([]string{
		"--mount type=bind,source=" + cache + ",destination=" + cache + ",readonly",
		"--mount type=bind,source=" + session + ",destination=" + session,
		"--mount type=bind,source=" + state + ",destination=" + state,
		"--mount type=bind,source=" + work + ",destination=" + work,
		"--mount type=bind,source=" + state + ",destination=" + link,
	}, " ")
	if got != want {
		t.Errorf("mounts:\n got %s\nwant %s", got, want)
	}

	for name, tc := range map[string]struct {
		b    ContainerBoundary
		g    []Mount
		want error
	}{
		"mount dir not granted":    {ContainerBoundary{MountDirs: []string{state}}, []Mount{{Path: work, Access: ReadWrite}, {Path: session, Access: ReadWrite}}, ErrNotGranted},
		"mount dir read-only":      {ContainerBoundary{MountDirs: []string{state}}, []Mount{{Path: base, Access: ReadOnly}, {Path: work, Access: ReadWrite}}, ErrNotGranted},
		"skill dir not granted":    {ContainerBoundary{SkillMountDirs: []string{cache}}, []Mount{{Path: work, Access: ReadWrite}, {Path: session, Access: ReadWrite}}, ErrNotGranted},
		"session dir not granted":  {ContainerBoundary{}, []Mount{{Path: work, Access: ReadWrite}}, ErrNotGranted},
		"host root":                {ContainerBoundary{}, []Mount{{Path: "/", Access: ReadWrite}}, ErrUnsupported},
		"sessions dir not granted": {ContainerBoundary{SessionsDir: filepath.Join(state, "new", "sessions")}, []Mount{{Path: work, Access: ReadWrite}}, ErrNotGranted},
	} {
		req := containerRequest(work, session, &Grants{Tools: []string{ToolsAll}, Mounts: tc.g})
		req.Profile.Skills = []string{"skill"}
		if name == "sessions dir not granted" {
			req.SessionDir = ""
		}
		if _, err := tc.b.Verify(req); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}

	// A working directory granted read-only is bound read-only: the engine
	// holds a reviewer's pinned revision the way it holds any other mount.
	req = containerRequest(work, session, &Grants{Tools: []string{ToolsAll}, Mounts: []Mount{{Path: work, Access: ReadOnly}, {Path: session, Access: ReadWrite}}})
	turn, err = (ContainerBoundary{}).Verify(req)
	if err != nil {
		t.Fatalf("read-only working directory: %v", err)
	}
	if !slices.Contains(turn.Binds, Bind{Source: work, Destination: work, Access: ReadOnly}) {
		t.Errorf("read-only working directory: binds %v", turn.Binds)
	}

	// The skills cache need not be granted to a profile without skills.
	req = containerRequest(work, session, &Grants{Tools: []string{ToolsAll}, Mounts: []Mount{{Path: work, Access: ReadWrite}, {Path: session, Access: ReadWrite}}})
	if _, err := (ContainerBoundary{SkillMountDirs: []string{cache}}).Verify(req); err != nil {
		t.Errorf("skills cache for a profile without skills: %v", err)
	}

	// A sessions directory that does not exist yet is checked by where it
	// would be created.
	req = containerRequest(work, "", &Grants{Tools: []string{ToolsAll}, Mounts: []Mount{{Path: work, Access: ReadWrite}, {Path: link, Access: ReadWrite}}})
	if _, err := (ContainerBoundary{SessionsDir: filepath.Join(link, "new", "sessions")}).Verify(req); err != nil {
		t.Errorf("sessions directory to be created inside a grant: %v", err)
	}
}

// A request with no session directory is verified again once the runner
// has created one, so the container is given that directory under the path
// the runner uses for it, even when a symbolic link leads there.
func TestContainerSessionDirIsVerifiedOnceCreated(t *testing.T) {
	// The fake engine records its arguments in the session directory, found
	// from the container id file the runner puts there.
	docker := agenttest.Docker(t, "image", "RUN_DIR")
	engine := agenttest.Script(t, "docker", `prev=""
for arg in "$@"; do
  [ "$prev" = --cidfile ] && RUN_DIR="$(dirname "$arg")" && export RUN_DIR
  prev="$arg"
done
exec `+docker+` "$@"`)
	claude := agenttest.Script(t, "claude", `cat >/dev/null
echo '{"type":"result","subtype":"success","result":"ok"}'`)
	for _, linked := range []bool{false, true} {
		root, work := realTempDir(t), realTempDir(t)
		sessions := filepath.Join(root, "sessions")
		if linked {
			sessions = filepath.Join(realTempDir(t), "state-link", "sessions")
			if err := os.Symlink(root, filepath.Dir(sessions)); err != nil {
				t.Fatal(err)
			}
		}
		r := Runner{ClaudeBin: claude, DockerBin: engine, SessionsDir: sessions}
		req := containerRequest(work, "", &Grants{Tools: []string{ToolsAll},
			Mounts: []Mount{{Path: work, Access: ReadWrite}, {Path: root, Access: ReadWrite}}})
		res, err := r.Run(context.Background(), req)
		if err != nil || res.IsError {
			t.Fatalf("linked=%v: %+v, %v", linked, res, err)
		}
		if filepath.Dir(res.SessionDir) != sessions {
			t.Fatalf("linked=%v: session directory %s is not in %s", linked, res.SessionDir, sessions)
		}
		real := filepath.Join(root, "sessions", filepath.Base(res.SessionDir))
		args := strings.Join(lines(t, filepath.Join(real, "docker-args.txt")), " ")
		bind := "--mount type=bind,source=" + real + ",destination=" + res.SessionDir
		if linked && !strings.Contains(args, bind) {
			t.Errorf("session directory behind a link is not bound at its alias (%s): %s", bind, args)
		}
		if !strings.Contains(args, "--mount type=bind,source="+root+",destination="+root) {
			t.Errorf("linked=%v: granted state directory not bound: %s", linked, args)
		}
	}
}

// Paths the engine could not be given exactly, a symbolic link out of the
// confinement, writable VCS metadata and an ungranted VCS mount are refused.
func TestContainerRefusesWhatItCannotConfine(t *testing.T) {
	base := realTempDir(t)
	work, session := filepath.Join(base, "work"), filepath.Join(base, "session")
	comma := filepath.Join(base, "a,b")
	outside := realTempDir(t)
	escape := filepath.Join(base, "escape")
	repo := filepath.Join(base, "repo")
	metadata := realTempDir(t)
	for _, d := range []string{work, session, comma, filepath.Join(repo, ".git")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, escape); err != nil {
		t.Fatal(err)
	}
	granted := func(extra ...Mount) *Grants {
		return &Grants{Tools: []string{ToolsAll}, Within: base, Mounts: append([]Mount{{Path: work, Access: ReadWrite}, {Path: session, Access: ReadWrite}}, extra...)}
	}
	for name, tc := range map[string]struct {
		req  Request
		want error
	}{
		"comma in a path":      {containerRequest(work, session, granted(Mount{Path: comma, Access: ReadOnly})), ErrUnsupported},
		"symlink escape":       {containerRequest(work, session, granted(Mount{Path: escape, Access: ReadOnly})), ErrNotGranted},
		"dot-dot escape":       {containerRequest(work, session, granted(Mount{Path: base + "/work/../../x", Access: ReadOnly})), ErrNotGranted},
		"writable .git":        {containerRequest(work, session, granted(Mount{Path: repo, Access: ReadWrite})), ErrNotGranted},
		"writable .git itself": {containerRequest(work, session, granted(Mount{Path: filepath.Join(repo, ".git"), Access: ReadWrite})), ErrNotGranted},
		"relative mount":       {containerRequest(work, session, granted(Mount{Path: "work", Access: ReadOnly})), ErrNotGranted},
		"conflicting modes":    {containerRequest(work, session, granted(Mount{Path: work, Access: ReadOnly})), ErrUnsupported},
		"narrowed codex tools": {func() Request {
			r := containerRequest(work, session, granted())
			r.Profile.Agent = AgentCodex
			r.Grants.Tools = []string{"Read"}
			return r
		}(), ErrUnsupported},
		"VCS mount not granted": {func() Request {
			r := containerRequest(work, session, granted())
			r.Grants.Within = ""
			r.Grants.VCS = true
			r.Profile.VCSAccess = true
			r.Workspace = fakeWorkspace{dir: work, access: &vcs.Access{Mounts: []string{metadata}}}
			return r
		}(), ErrNotGranted},
	} {
		if _, err := (ContainerBoundary{}).Verify(tc.req); !errors.Is(err, tc.want) {
			t.Errorf("%s: %v, want %v", name, err, tc.want)
		}
	}
	// A read-only repository is fine without VCS.
	if _, err := (ContainerBoundary{}).Verify(containerRequest(work, session, granted(Mount{Path: repo, Access: ReadOnly}))); err != nil {
		t.Errorf("read-only repository: %v", err)
	}
}

// Without VCS the session runs behind stand-ins for the VCS executables,
// put in front of the image's PATH inside the container; with VCS it runs
// the command directly.
func TestContainerDeniesVCSExecutables(t *testing.T) {
	for _, vcsAccess := range []bool{false, true} {
		dir := realTempDir(t)
		work := realTempDir(t)
		claude := agenttest.Script(t, "claude", `code=0
git --version >/dev/null 2>&1 || code=$?
echo $code > "$RUN_DIR/git-exit"
cat >/dev/null
echo '{"type":"result","subtype":"success","result":"ok"}'`)
		r := Runner{ClaudeBin: claude, DockerBin: agenttest.Docker(t, "image", "RUN_DIR")}
		req := Request{Name: "c", SessionDir: dir, Workspace: fakeWorkspace{dir: work}, Env: map[string]string{"RUN_DIR": dir},
			Profile: Profile{Sandbox: SandboxContainer, SandboxImage: "image", VCSAccess: vcsAccess}}
		req = grantAll(req)
		req.Grants.VCS = vcsAccess
		res, err := r.Run(context.Background(), req)
		if err != nil || res.IsError {
			t.Fatalf("VCS=%v: %+v, %v", vcsAccess, res, err)
		}
		args := strings.Join(lines(t, filepath.Join(dir, "docker-args.txt")), " ")
		wrapped := strings.Contains(args, " image /bin/sh -c ")
		if wrapped == vcsAccess {
			t.Errorf("VCS=%v: command wrapped=%v: %s", vcsAccess, wrapped, args)
		}
		got := strings.TrimSpace(lines(t, filepath.Join(dir, "git-exit"))[0])
		if !vcsAccess && got != "126" {
			t.Errorf("git inside a container without VCS exited %s, want the stand-in's 126", got)
		}
		if vcsAccess && got == "126" {
			t.Error("git denied to a container with VCS")
		}
	}
}
