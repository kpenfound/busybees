package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/vcs"
)

// fakeConfiner stands in for the operating system: it records what it is
// asked to enforce and starts the command the way an enforcer would, or
// refuses the way a platform without one does.
type fakeConfiner struct {
	check   error
	start   error
	checked []Confinement
	started []Confinement
}

func (f *fakeConfiner) Check(c Confinement) error {
	f.checked = append(f.checked, c)
	return f.check
}

func (f *fakeConfiner) Start(cmd *exec.Cmd, c Confinement) error {
	if f.start != nil {
		return f.start
	}
	f.started = append(f.started, c)
	return cmd.Start()
}

// confinedLayout is a machine in miniature: a working directory granted
// read-only, a session directory granted read-write, a directory on PATH
// that holds a git, and a system path.
type confinedLayout struct {
	work, session, tools, system, outside string
}

func newConfinedLayout(t *testing.T) confinedLayout {
	t.Helper()
	base := realTemp(t)
	l := confinedLayout{
		work:    filepath.Join(base, "work"),
		session: filepath.Join(base, "session"),
		tools:   filepath.Join(base, "tools"),
		system:  filepath.Join(base, "system"),
		outside: filepath.Join(base, "outside"),
	}
	for _, dir := range []string{l.work, l.session, l.tools, l.system, l.outside} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(l.tools, "git"), "echo a git\n")
	return l
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// request is a confined request with no "/" anywhere in its grants.
func (l confinedLayout) request(sandbox string) Request {
	return Request{
		Workspace:  vcs.Directory(l.work),
		SessionDir: l.session,
		Profile:    Profile{Name: "reviewer", Sandbox: sandbox, Confine: true},
		Grants: &Grants{
			Env:    []string{"PATH"},
			Tools:  []string{ToolsAll},
			Mounts: []Mount{{Path: l.work, Access: ReadOnly}, {Path: l.session, Access: ReadWrite}},
		},
	}
}

func (l confinedLayout) boundary(c Confiner) HostBoundary {
	return HostBoundary{
		Environ:     func() []string { return []string{"PATH=" + l.tools, "HOME=/home/someone"} },
		Confiner:    c,
		SystemPaths: []Mount{{Path: l.system, Access: ReadOnly}, {Path: filepath.Join(l.system, "missing"), Access: ReadOnly}},
	}
}

func TestAConfinedTurnIsHeldToItsGrants(t *testing.T) {
	for _, sandbox := range []string{"", SandboxNone, SandboxClaude} {
		t.Run("sandbox "+sandbox, func(t *testing.T) {
			l := newConfinedLayout(t)
			confiner := &fakeConfiner{}
			req := l.request(sandbox)
			turn, err := l.boundary(confiner).Verify(req)
			if err != nil {
				t.Fatalf("a confined turn with a read-only working directory and no %q: %v", "/", err)
			}
			c := turn.Confinement
			if c == nil {
				t.Fatal("the turn carries no confinement")
			}
			want := []Mount{{Path: l.work, Access: ReadOnly}, {Path: l.session, Access: ReadWrite}}
			if !slices.Equal(c.Mounts, want) {
				t.Errorf("mounts = %v, want the grants alone %v", c.Mounts, want)
			}
			// The system path the machine has, and not the one it lacks.
			if !slices.Equal(c.System, []Mount{{Path: l.system, Access: ReadOnly}}) {
				t.Errorf("system = %v", c.System)
			}
			for _, m := range append(slices.Clone(c.Mounts), c.System...) {
				if m.Path == string(filepath.Separator) {
					t.Errorf("the confinement reaches %q: %v", "/", m)
				}
			}
			if !slices.Contains(c.Denied, filepath.Join(l.tools, "git")) {
				t.Errorf("denied = %v, want the git on PATH", c.Denied)
			}
			if !slices.Equal(turn.DeniedExecutables, VCSExecutables) {
				t.Errorf("denied executables = %v", turn.DeniedExecutables)
			}
			if slices.Contains(turn.WriteDirs, l.work) {
				t.Errorf("the read-only working directory is named writable: %v", turn.WriteDirs)
			}
			wantSandbox := sandbox
			if wantSandbox == "" {
				wantSandbox = SandboxNone
			}
			if len(confiner.checked) != 1 || confiner.checked[0].Sandbox != wantSandbox {
				t.Errorf("the confiner was asked %+v", confiner.checked)
			}

			// The same grants without Confine are what they always were:
			// nothing enforces them, so they are refused.
			req.Profile.Confine = false
			if _, err := l.boundary(confiner).Verify(req); !errors.Is(err, ErrUnsupported) {
				t.Errorf("unconfined with the same grants: %v, want ErrUnsupported", err)
			}
		})
	}
}

func TestAConfinedTurnWithVCSKeepsItsExecutables(t *testing.T) {
	l := newConfinedLayout(t)
	req := l.request(SandboxNone)
	req.Grants.VCS = true
	turn, err := l.boundary(&fakeConfiner{}).Verify(req)
	if err != nil {
		t.Fatal(err)
	}
	// Granted but not asked for by the profile: still denied.
	if len(turn.Confinement.Denied) == 0 || len(turn.DeniedExecutables) == 0 {
		t.Errorf("VCS narrowed by the profile: denied %v, %v", turn.Confinement.Denied, turn.DeniedExecutables)
	}
	req.Profile.VCSAccess = true
	turn, err = l.boundary(&fakeConfiner{}).Verify(req)
	if err != nil {
		t.Fatalf("VCS granted to a confined turn with no %q: %v", "/", err)
	}
	if len(turn.Confinement.Denied) != 0 || len(turn.DeniedExecutables) != 0 {
		t.Errorf("VCS granted: denied %v, %v", turn.Confinement.Denied, turn.DeniedExecutables)
	}
}

func TestAConfinedTurnIsRefusedWhereNothingEnforcesIt(t *testing.T) {
	l := newConfinedLayout(t)
	refusal := fmt.Errorf("%w: no confiner on this platform", ErrUnsupported)
	if _, err := l.boundary(&fakeConfiner{check: refusal}).Verify(l.request(SandboxNone)); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("verify = %v, want ErrUnsupported", err)
	}

	marker := filepath.Join(l.session, "ran")
	bin := agenttest.Script(t, "claude", "touch "+marker+"\necho '{\"type\":\"result\",\"subtype\":\"success\"}'\n")
	for name, confiner := range map[string]*fakeConfiner{
		"refused when verified": {check: refusal},
		"refused when started":  {start: refusal},
	} {
		t.Run(name, func(t *testing.T) {
			r := &Runner{ClaudeBin: bin, Confiner: confiner, SystemPaths: []Mount{{Path: l.system, Access: ReadOnly}}}
			res, err := r.Run(context.Background(), l.request(SandboxNone))
			if !errors.Is(err, ErrUnsupported) {
				t.Fatalf("run = %+v, %v, want ErrUnsupported", res, err)
			}
			if _, err := os.Stat(marker); err == nil {
				t.Fatal("the agent ran without its confinement")
			}
			if len(confiner.started) != 0 {
				t.Fatalf("started %v", confiner.started)
			}
		})
	}
}

func TestRunStartsAConfinedTurnThroughItsConfiner(t *testing.T) {
	l := newConfinedLayout(t)
	t.Setenv("PATH", l.tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	bin := agenttest.Script(t, "claude", "cat >/dev/null\necho '{\"type\":\"result\",\"subtype\":\"success\"}'\n")
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		t.Fatal(err)
	}
	confiner := &fakeConfiner{}
	r := &Runner{ClaudeBin: bin, Confiner: confiner, SystemPaths: []Mount{{Path: l.system, Access: ReadOnly}}}
	res, err := r.Run(context.Background(), l.request(SandboxNone))
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	if len(confiner.started) != 1 {
		t.Fatalf("started %d turns through the confiner, want 1", len(confiner.started))
	}
	c := confiner.started[0]
	if !slices.Equal(c.Mounts, []Mount{{Path: l.work, Access: ReadOnly}, {Path: l.session, Access: ReadWrite}}) {
		t.Errorf("mounts = %v", c.Mounts)
	}
	// The agent's own executable is the one thing the runner adds.
	if !slices.Equal(c.System, []Mount{{Path: l.system, Access: ReadOnly}, {Path: real, Access: ReadOnly}}) {
		t.Errorf("system = %v", c.System)
	}
	if !slices.Contains(c.Denied, filepath.Join(l.tools, "git")) {
		t.Errorf("denied = %v", c.Denied)
	}

	// A turn that is not confined does not go near it.
	plain := &fakeConfiner{}
	r = &Runner{ClaudeBin: bin, Confiner: plain}
	req := Request{Workspace: vcs.Directory(l.work), SessionDir: l.session, Profile: Profile{Name: "worker"}}
	if res, err := r.Run(context.Background(), grantAll(req)); err != nil || res.IsError {
		t.Fatalf("unconfined run: %+v, %v", res, err)
	}
	if len(plain.checked)+len(plain.started) != 0 {
		t.Errorf("an unconfined turn reached the confiner: %+v", plain)
	}
}

func TestAConfinedTurnMustBeGrantedWhatTheRunnerWritesForIt(t *testing.T) {
	l := newConfinedLayout(t)
	for name, tc := range map[string]struct {
		boundary func(*HostBoundary)
		request  func(*Request)
	}{
		"session directory": {request: func(r *Request) { r.SessionDir = l.outside }},
		"sessions directory": {
			boundary: func(h *HostBoundary) { h.SessionsDir = filepath.Join(l.outside, "sessions") },
			request:  func(r *Request) { r.SessionDir = "" },
		},
		"skill directory": {
			boundary: func(h *HostBoundary) { h.SkillDirs = []string{l.outside} },
			request:  func(r *Request) { r.Profile.Skills = []string{"a-skill"} },
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := l.boundary(&fakeConfiner{})
			if tc.boundary != nil {
				tc.boundary(&h)
			}
			req := l.request(SandboxNone)
			tc.request(&req)
			if _, err := h.Verify(req); !errors.Is(err, ErrNotGranted) {
				t.Fatalf("ungranted %s: %v, want ErrNotGranted", name, err)
			}
			req.Grants.Mounts = append(req.Grants.Mounts, Mount{Path: l.outside, Access: ReadOnly})
			if _, err := h.Verify(req); err != nil {
				t.Fatalf("granted %s: %v", name, err)
			}
		})
	}
}

func TestAMountCannotGrantADeniedExecutable(t *testing.T) {
	l := newConfinedLayout(t)
	req := l.request(SandboxNone)
	req.Grants.Mounts = append(req.Grants.Mounts, Mount{Path: filepath.Join(l.tools, "git"), Access: ReadOnly})
	if _, err := l.boundary(&fakeConfiner{}).Verify(req); !errors.Is(err, ErrNotGranted) {
		t.Fatalf("a mount that is git, without VCS: %v, want ErrNotGranted", err)
	}
}

func TestConfineIsAHostSetting(t *testing.T) {
	p := Profile{Sandbox: SandboxContainer, SandboxImage: "image", Confine: true}
	if err := p.Validate(); err == nil {
		t.Fatal("confine with a container sandbox validated")
	}
}

func TestExecutablePathsAreTheFilesNotTheNames(t *testing.T) {
	base := realTemp(t)
	prefix := filepath.Join(base, "opt", "vcs")
	for _, dir := range []string{filepath.Join(prefix, "bin"), filepath.Join(prefix, "libexec", "git-core"), filepath.Join(base, "bin")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(prefix, "bin", "git"), "echo git\n")
	writeExecutable(t, filepath.Join(base, "bin", "hg"), "echo hg\n")
	if err := os.Symlink(filepath.Join(prefix, "bin", "git"), filepath.Join(base, "bin", "git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(base, "bin", "svn"), 0o755); err != nil {
		t.Fatal(err)
	}
	got := executablePaths(VCSExecutables, "relative"+string(os.PathListSeparator)+filepath.Join(base, "bin"))
	for _, want := range []string{
		filepath.Join(prefix, "bin", "git"), // what the link on PATH runs
		filepath.Join(prefix, "libexec", "git-core"),
		filepath.Join(base, "bin", "hg"),
	} {
		if !slices.Contains(got, want) {
			t.Errorf("%s missing from %v", want, got)
		}
	}
	for _, p := range got {
		if p == filepath.Join(base, "bin", "git") || p == filepath.Join(base, "bin", "svn") {
			t.Errorf("%s is a link or a directory, not an executable: %v", p, got)
		}
	}
}

// reach reports what the rules allow of path: the access of every rule that
// covers it, the way a mechanism that only allows adds them up.
func reach(rules []confineRule, path string) (read, write bool) {
	for _, r := range rules {
		if r.Path == path || (r.Dir && inside(r.Path, path)) {
			read = read || r.Read
			write = write || r.Write
		}
	}
	return read, write
}

func TestRulesGoAroundADeniedExecutable(t *testing.T) {
	base := realTemp(t)
	bin := filepath.Join(base, "usr", "bin")
	core := filepath.Join(base, "usr", "lib", "git-core")
	for _, dir := range []string{bin, core, filepath.Join(base, "usr", "share")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	git := filepath.Join(bin, "git")
	for _, p := range []string{git, filepath.Join(bin, "cat"), filepath.Join(core, "git-fetch"), filepath.Join(base, "usr", "share", "doc")} {
		writeExecutable(t, p, "true\n")
	}
	if err := os.Link(git, filepath.Join(bin, "git-upload-pack")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("git", filepath.Join(bin, "git-link")); err != nil {
		t.Fatal(err)
	}
	c := Confinement{
		System: []Mount{{Path: filepath.Join(base, "usr"), Access: ReadOnly}, {Path: git, Access: ReadOnly}, {Path: filepath.Join(core, "git-fetch"), Access: ReadOnly}},
		Denied: []string{git, core},
	}
	rules, err := confineRules(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, denied := range []string{git, filepath.Join(bin, "git-upload-pack"), core, filepath.Join(core, "git-fetch")} {
		if read, write := reach(rules, denied); read || write {
			t.Errorf("%s is reachable (read %v, write %v): %+v", denied, read, write, rules)
		}
	}
	for _, allowed := range []string{filepath.Join(bin, "cat"), filepath.Join(base, "usr", "share", "doc"), filepath.Join(base, "usr", "share")} {
		if read, _ := reach(rules, allowed); !read {
			t.Errorf("%s is not readable: %+v", allowed, rules)
		}
	}
	for _, r := range rules {
		if r.Path == filepath.Join(bin, "git-link") {
			t.Errorf("a rule names a symbolic link, which a rule would follow to its target: %+v", r)
		}
		// A directory gone around can be listed, and that is all.
		if (r.Path == bin || r.Path == filepath.Join(base, "usr")) && (!r.Dir || r.Read || r.Write) {
			t.Errorf("rule for %s = %+v, want listing only", r.Path, r)
		}
	}
}

func TestRulesKeepAReadOnlyMountInsideAWritableOneReadOnly(t *testing.T) {
	base := realTemp(t)
	state := filepath.Join(base, "state")
	pinned := filepath.Join(state, "checkouts", "pinned")
	scratch := filepath.Join(pinned, "scratch")
	for _, dir := range []string{scratch, filepath.Join(state, "notes"), filepath.Join(state, "checkouts", "other")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	c := Confinement{Mounts: []Mount{
		{Path: state, Access: ReadWrite},
		{Path: pinned, Access: ReadOnly},
		{Path: scratch, Access: ReadWrite},
	}}
	rules, err := confineRules(c)
	if err != nil {
		t.Fatal(err)
	}
	for path, wantWrite := range map[string]bool{
		pinned:                           false,
		filepath.Join(pinned, "file.go"): false,
		scratch:                          true,
		filepath.Join(state, "notes"):    true,
		filepath.Join(state, "checkouts", "other"):   true,
		filepath.Join(state, "checkouts", "new-dir"): false, // nothing is created beside the exception
	} {
		read, write := reach(rules, path)
		if write != wantWrite {
			t.Errorf("%s writable = %v, want %v: %+v", path, write, wantWrite, rules)
		}
		if !read && path != filepath.Join(state, "checkouts", "new-dir") {
			t.Errorf("%s is not readable: %+v", path, rules)
		}
	}

	// A writable mount inside a read-only one needs nothing gone around.
	rules, err = confineRules(Confinement{Mounts: []Mount{{Path: pinned, Access: ReadOnly}, {Path: scratch, Access: ReadWrite}}})
	if err != nil {
		t.Fatal(err)
	}
	want := []confineRule{{Path: pinned, Dir: true, Read: true}, {Path: scratch, Dir: true, Read: true, Write: true}}
	if !slices.Equal(rules, want) {
		t.Errorf("rules = %+v, want %+v", rules, want)
	}
}
