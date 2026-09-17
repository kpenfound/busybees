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
probe git-by-new-hard-link sh -c 'ln {tools}/git {out}/git-hl && {out}/git-hl --version'
probe system-git-by-path /usr/bin/git --version
probe system-git-exec-path /usr/lib/git-core/git --version
probe system-tool-by-path /usr/bin/env true
mkdir {out}/mnt
probe mount-in-writable mount -t tmpfs none {out}/mnt
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
				// A link the agent makes itself, out of a directory it may
				// only read into one it may write, would widen what the file
				// allows: refused with VCS too.
				"git-by-new-hard-link": "denied",
				// What claude's box on Linux is built with: see Check.
				"mount-in-writable": "denied",
			}
			// The machine's own git, gone around inside the system paths,
			// where the machine has one.
			want["system-tool-by-path"] = "allowed"
			for probe, path := range map[string]string{"system-git-by-path": "/usr/bin/git", "system-git-exec-path": "/usr/lib/git-core/git"} {
				if _, err := os.Stat(path); err == nil {
					want[probe] = git
				}
			}
			got := probes(t, out)
			t.Logf("probes: %v", got)
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

// The same turn through the exported API: what the session's policy says of
// a path after Prepare is what the kernel does to the agent that tries it.
func TestLandlockEnforcesWhatASessionReports(t *testing.T) {
	needLandlock(t)
	l := newConfinedLayout(t)
	out := filepath.Join(filepath.Dir(l.work), "out")
	if err := os.Mkdir(out, 0o755); err != nil {
		t.Fatal(err)
	}
	readme, secret := filepath.Join(l.work, "readme.txt"), filepath.Join(l.outside, "secret.txt")
	for path, text := range map[string]string{readme: "pinned\n", secret: "secret\n"} {
		if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	git := filepath.Join(l.tools, "git")
	writeExecutable(t, filepath.Join(l.tools, "other"), "exit 0\n")
	if err := os.Link(git, filepath.Join(l.tools, "git-receive-pack")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("git", filepath.Join(l.tools, "git-link")); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", l.tools+string(os.PathListSeparator)+os.Getenv("PATH"))

	s, err := NewHostNone(Runner{ClaudeBin: probeScript(t, l, out)}).Prepare(context.Background(), Grants{
		Env:   []string{"PATH"},
		Tools: []string{ToolsAll},
		Mounts: []Mount{
			{Path: l.work, Access: ReadOnly},
			{Path: l.tools, Access: ReadOnly},
			{Path: l.session, Access: ReadWrite},
			{Path: out, Access: ReadWrite},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Release(context.Background()) }()
	p := s.Policy()
	// What the policy says before anything runs, by the probe that tries it.
	said := map[string]bool{
		"read-mount":           p.Reads(readme),
		"list-mount":           p.Reads(l.work),
		"read-outside":         p.Reads(secret),
		"list-outside":         p.Reads(l.outside),
		"create-in-workdir":    p.Writes(filepath.Join(l.work, "new.txt")),
		"overwrite-in-workdir": p.Writes(readme),
		"truncate-in-workdir":  p.Writes(readme),
		"remove-in-workdir":    p.Writes(readme),
		"mkdir-in-workdir":     p.Writes(filepath.Join(l.work, "sub")),
		"create-in-writable":   p.Writes(filepath.Join(out, "new.txt")),
		"create-outside":       p.Writes(filepath.Join(l.outside, "new.txt")),
		"tool-by-path":         p.Runs(filepath.Join(l.tools, "other")),
		"git-by-path":          p.Runs(git),
		"git-by-shell":         p.Runs(git),
		"git-by-env":           p.Runs(git),
		"git-by-hard-link":     p.Runs(filepath.Join(l.tools, "git-receive-pack")),
		"git-by-symlink":       p.Runs(filepath.Join(l.tools, "git-link")),
		"git-read":             p.Reads(git),
		"system-tool-by-path":  p.Runs("/usr/bin/env"),
	}
	for probe, path := range map[string]string{"system-git-by-path": "/usr/bin/git", "system-git-exec-path": "/usr/lib/git-core/git"} {
		if _, err := os.Stat(path); err == nil {
			said[probe] = p.Runs(path)
		}
	}
	// The three an embedder refuses to run without.
	for _, probe := range []string{"read-outside", "create-in-workdir", "git-by-path", "git-by-shell"} {
		if said[probe] {
			t.Errorf("the policy allows %s", probe)
		}
	}

	res, err := s.Run(context.Background(), Request{Workspace: vcs.Directory(l.work), SessionDir: l.session, Profile: Profile{Name: "reviewer"}})
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	got := probes(t, out)
	t.Logf("probes: %v", got)
	for probe, allowed := range said {
		want := "denied"
		if allowed {
			want = "allowed"
		}
		if got[probe] != want {
			t.Errorf("%s: the kernel %s it, and the policy said %s", probe, got[probe], want)
		}
	}
}
