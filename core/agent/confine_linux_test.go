package agent

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/vcs"
)

func TestLandlockRefusesWhatItCannotEnforce(t *testing.T) {
	version := func(v int, err error) func() (int, error) {
		return func() (int, error) { return v, err }
	}
	for name, tc := range map[string]struct {
		abi     func() (int, error)
		sandbox string
		refused bool
	}{
		"no landlock in the kernel":   {abi: version(0, syscall.ENOSYS), sandbox: SandboxNone, refused: true},
		"landlock switched off":       {abi: version(0, syscall.EOPNOTSUPP), sandbox: SandboxNone, refused: true},
		"no truncate right":           {abi: version(landlockMinABI-1, nil), sandbox: SandboxNone, refused: true},
		"enough":                      {abi: version(landlockMinABI, nil), sandbox: SandboxNone},
		"claude's box needs to mount": {abi: version(landlockMinABI, nil), sandbox: SandboxClaude, refused: true},
	} {
		t.Run(name, func(t *testing.T) {
			l := landlockConfiner{abi: tc.abi}
			c := Confinement{Sandbox: tc.sandbox}
			err := l.Check(c)
			if tc.refused != errors.Is(err, ErrUnsupported) || (!tc.refused && err != nil) {
				t.Fatalf("check = %v, refused want %v", err, tc.refused)
			}
			if !tc.refused {
				return
			}
			// Asked to start it anyway, it starts nothing.
			cmd := exec.Command(agenttest.Script(t, "claude", "exit 0\n"))
			if err := l.Start(cmd, c); !errors.Is(err, ErrUnsupported) || cmd.Process != nil {
				t.Fatalf("start = %v, process %v: want ErrUnsupported and nothing started", err, cmd.Process)
			}
		})
	}
}

// needLandlock skips a test of the kernel's enforcement where the kernel has
// none to test.
func needLandlock(t *testing.T) {
	t.Helper()
	v, err := landlockABI()
	if err != nil {
		t.Skipf("this kernel has no Landlock (landlock_create_ruleset: %v): enforcement is not exercised here", err)
	}
	if v < landlockMinABI {
		t.Skipf("this kernel's Landlock is version %d, below the %d a confined turn needs: enforcement is not exercised here", v, landlockMinABI)
	}
}

// probeScript is a fake agent that tries what a confined turn must not be
// able to do, and a few things it must, and writes what happened into out.
func probeScript(t *testing.T, l confinedLayout, out string) string {
	t.Helper()
	body := `cat >/dev/null
probe() {
  name="$1"; shift
  if "$@" >/dev/null 2>&1; then echo "$name allowed"; else echo "$name denied"; fi >> {out}/probes.txt
}
probe read-mount cat {work}/readme.txt
probe list-mount ls {work}
probe read-outside cat {outside}/secret.txt
probe list-outside ls {outside}
probe create-in-workdir sh -c 'echo x > {work}/new.txt'
probe overwrite-in-workdir sh -c 'echo x > {work}/readme.txt'
probe truncate-in-workdir truncate -s 0 {work}/readme.txt
probe remove-in-workdir rm {work}/readme.txt
probe mkdir-in-workdir mkdir {work}/sub
probe create-in-writable sh -c 'echo x > {out}/new.txt'
probe create-outside sh -c 'echo x > {outside}/new.txt'
probe tool-by-path {tools}/other
probe git-by-name git --version
probe git-by-path {tools}/git --version
probe git-by-shell sh -c 'exec {tools}/git --version'
probe git-by-env env {tools}/git --version
probe git-by-hard-link {tools}/git-receive-pack --version
probe git-by-symlink {tools}/git-link --version
probe git-read cat {tools}/git
probe git-copy cp {tools}/git {out}/git
echo '{"type":"result","subtype":"success"}'
`
	for name, dir := range map[string]string{"{outside}": l.outside, "{work}": l.work, "{tools}": l.tools, "{out}": out} {
		body = strings.ReplaceAll(body, name, dir)
	}
	return agenttest.Script(t, "claude", body)
}

func probes(t *testing.T, out string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, line := range lines(t, filepath.Join(out, "probes.txt")) {
		if name, verdict, ok := strings.Cut(line, " "); ok {
			got[name] = verdict
		}
	}
	return got
}

// A confined turn under the real Landlock confiner: a fake agent that tries
// to leave its grants. It reads the default system paths, so the shell and
// the tools it probes with are the machine's own.
func TestLandlockHoldsATurnToItsGrants(t *testing.T) {
	needLandlock(t)
	for name, vcsGranted := range map[string]bool{"without VCS": false, "with VCS": true} {
		t.Run(name, func(t *testing.T) {
			l := newConfinedLayout(t)
			out := filepath.Join(filepath.Dir(l.work), "out")
			if err := os.Mkdir(out, 0o755); err != nil {
				t.Fatal(err)
			}
			for path, text := range map[string]string{
				filepath.Join(l.work, "readme.txt"):    "pinned\n",
				filepath.Join(l.outside, "secret.txt"): "secret\n",
			} {
				if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			writeExecutable(t, filepath.Join(l.tools, "other"), "exit 0\n")
			if err := os.Link(filepath.Join(l.tools, "git"), filepath.Join(l.tools, "git-receive-pack")); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink("git", filepath.Join(l.tools, "git-link")); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", l.tools+string(os.PathListSeparator)+os.Getenv("PATH"))

			r := &Runner{ClaudeBin: probeScript(t, l, out)}
			req := Request{
				Workspace:  vcs.Directory(l.work),
				SessionDir: l.session,
				Profile:    Profile{Name: "reviewer", Sandbox: SandboxNone, Confine: true, VCSAccess: vcsGranted},
				Grants: &Grants{
					Env:   []string{"PATH"},
					Tools: []string{ToolsAll},
					VCS:   vcsGranted,
					Mounts: []Mount{
						{Path: l.work, Access: ReadOnly},
						{Path: l.tools, Access: ReadOnly},
						{Path: l.session, Access: ReadWrite},
						{Path: out, Access: ReadWrite},
					},
				},
			}
			res, err := r.Run(context.Background(), req)
			if err != nil || res.IsError {
				t.Fatalf("run: %+v, %v", res, err)
			}

			git := "denied"
			if vcsGranted {
				git = "allowed"
			}
			want := map[string]string{
				"read-mount":           "allowed",
				"list-mount":           "allowed",
				"read-outside":         "denied",
				"list-outside":         "denied",
				"create-in-workdir":    "denied",
				"overwrite-in-workdir": "denied",
				"truncate-in-workdir":  "denied",
				"remove-in-workdir":    "denied",
				"mkdir-in-workdir":     "denied",
				"create-in-writable":   "allowed",
				"create-outside":       "denied",
				"tool-by-path":         "allowed",
				"git-by-name":          git,
				"git-by-path":          git,
				"git-by-shell":         git,
				"git-by-env":           git,
				"git-by-hard-link":     git,
				"git-by-symlink":       git,
				"git-read":             git,
				"git-copy":             git,
			}
			got := probes(t, out)
			for name, verdict := range want {
				if got[name] != verdict {
					t.Errorf("%s: %s, want %s", name, got[name], verdict)
				}
			}
			if data, err := os.ReadFile(filepath.Join(l.work, "readme.txt")); err != nil || string(data) != "pinned\n" {
				t.Errorf("the read-only working directory changed: %q, %v", data, err)
			}
			for _, p := range []string{filepath.Join(l.work, "new.txt"), filepath.Join(l.work, "sub"), filepath.Join(l.outside, "new.txt")} {
				if _, err := os.Lstat(p); err == nil {
					t.Errorf("%s was created", p)
				}
			}
		})
	}
}

// The restriction is the agent's alone: the thread that took it on is gone,
// and the test binary still reads what the agent could not.
func TestLandlockLeavesTheRunnerUnconfined(t *testing.T) {
	needLandlock(t)
	l := newConfinedLayout(t)
	secret := filepath.Join(l.outside, "secret.txt")
	if err := os.WriteFile(secret, []byte("secret\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := agenttest.Script(t, "claude", "cat >/dev/null\necho '{\"type\":\"result\",\"subtype\":\"success\"}'\n")
	for range 20 {
		res, err := (&Runner{ClaudeBin: bin}).Run(context.Background(), l.request(SandboxNone))
		if err != nil || res.IsError {
			t.Fatalf("run: %+v, %v", res, err)
		}
		done := make(chan error)
		for range 8 {
			go func() {
				_, err := os.ReadFile(secret)
				done <- err
			}()
		}
		for range 8 {
			if err := <-done; err != nil {
				t.Fatalf("the runner's own process lost access: %v", err)
			}
		}
	}
}
