package agent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/vcs"
)

// okAgent is a fake claude that records its arguments in the directory
// ARGS_DIR names, when it is set, and succeeds.
func okAgent(t *testing.T) string {
	t.Helper()
	return agenttest.Script(t, "claude", `cat >/dev/null
[ -z "$ARGS_DIR" ] || printf '%s\n' "$@" > "$ARGS_DIR/agent-args.txt"
echo '{"type":"result","subtype":"success","result":"ok"}'
`)
}

// grants are the layout's grants: the working directory read-only, the
// session directory read-write, and no "/".
func (l confinedLayout) grants() Grants {
	return Grants{
		Env:    []string{"PATH", "ARGS_DIR"},
		Tools:  []string{"Read", "Grep", "mcp__tools"},
		Mounts: []Mount{{Path: l.work, Access: ReadOnly}, {Path: l.session, Access: ReadWrite}},
	}
}

// turn is a request that says nothing about its sandbox or its grants: the
// session decides both.
func (l confinedLayout) turn() Request {
	return Request{Name: "review", Workspace: vcs.Directory(l.work), SessionDir: l.session,
		Env: map[string]string{"ARGS_DIR": l.session}, Profile: Profile{Name: "reviewer"}}
}

func hostEnforcers(r Runner) map[string]Enforcer {
	return map[string]Enforcer{SandboxNone: NewHostNone(r), SandboxClaude: NewHostClaude(r)}
}

// What a host session reports after Prepare is what its confiner is handed
// when a turn starts, and it says the three things an embedder asks of it:
// nothing outside the mounts is read, a read-only working directory is not
// written, and git is not run.
func TestAHostSessionEnforcesThePolicyItReports(t *testing.T) {
	for _, kind := range []string{SandboxNone, SandboxClaude} {
		t.Run(kind, func(t *testing.T) {
			l := newConfinedLayout(t)
			t.Setenv("PATH", l.tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			secret := filepath.Join(l.outside, "secret.txt")
			pinned := filepath.Join(l.work, "readme.txt")
			for _, p := range []string{secret, pinned} {
				if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			bin := okAgent(t)
			real, err := filepath.EvalSymlinks(bin)
			if err != nil {
				t.Fatal(err)
			}
			confiner := &fakeConfiner{}
			r := Runner{ClaudeBin: bin, CodexBin: filepath.Join(l.outside, "no-codex"), OpenCodeBin: filepath.Join(l.outside, "no-opencode"),
				Confiner: confiner, SystemPaths: []Mount{{Path: l.system, Access: ReadOnly}}}
			s, err := hostEnforcers(r)[kind].Prepare(context.Background(), l.grants())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = s.Release(context.Background()) }()

			p := s.Policy()
			// The machine's own git is denied too, where it has one.
			if !slices.Contains(p.Denied, filepath.Join(l.tools, "git")) {
				t.Fatalf("denied = %v, want the git on PATH", p.Denied)
			}
			want := Policy{
				Sandbox:           kind,
				Env:               []string{"PATH", "ARGS_DIR"},
				Tools:             []string{"Read", "Grep"},
				MCPServers:        []string{"tools"},
				Mounts:            []Mount{{Path: l.work, Access: ReadOnly}, {Path: l.session, Access: ReadWrite}},
				System:            []Mount{{Path: l.system, Access: ReadOnly}, {Path: real, Access: ReadOnly}},
				DeniedExecutables: VCSExecutables,
				Denied:            p.Denied,
			}
			if got, want := jsonOf(t, p), jsonOf(t, want); got != want {
				t.Fatalf("policy after Prepare:\n got %s\nwant %s", got, want)
			}
			if len(confiner.checked) != 1 || len(confiner.started) != 0 {
				t.Fatalf("Prepare asked the confiner %d times and started %d turns, want 1 and 0", len(confiner.checked), len(confiner.started))
			}
			for name, tc := range map[string]struct{ got, want bool }{
				"reads its working directory":       {p.Reads(pinned), true},
				"reads outside its mounts":          {p.Reads(secret), false},
				"lists outside its mounts":          {p.Reads(l.outside), false},
				"writes its read-only working dir":  {p.Writes(pinned), false},
				"creates in its read-only work dir": {p.Writes(filepath.Join(l.work, "new.txt")), false},
				"creates in its session directory":  {p.Writes(filepath.Join(l.session, "new.txt")), true},
				"creates outside its mounts":        {p.Writes(filepath.Join(l.outside, "new.txt")), false},
				"runs git by its path":              {p.Runs(filepath.Join(l.tools, "git")), false},
				"reads git":                         {p.Reads(filepath.Join(l.tools, "git")), false},
				"runs its agent":                    {p.Runs(real), true},
				"has a granted tool":                {p.Allows("Read"), true},
				"has an ungranted tool":             {p.Allows("Bash"), false},
				"has a granted server's tool":       {p.Allows("mcp__tools__done"), true},
				"has an ungranted server's tool":    {p.Allows("mcp__other__done"), false},
			} {
				if tc.got != tc.want {
					t.Errorf("the policy says the turn %s: %v, want %v", name, tc.got, tc.want)
				}
			}

			res, err := s.Run(context.Background(), l.turn())
			if err != nil || res.IsError {
				t.Fatalf("run: %+v, %v", res, err)
			}
			if len(confiner.started) != 1 {
				t.Fatalf("started %d turns through the confiner, want 1", len(confiner.started))
			}
			// The rules the platform's confiner would hand the kernel are
			// the ones the policy is judged by.
			enforced, err := confineRules(confiner.started[0])
			if err != nil {
				t.Fatal(err)
			}
			reported, err := confineRules(Confinement{Sandbox: p.Sandbox, Mounts: p.Mounts, System: p.System, Denied: p.Denied})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(enforced, reported) || confiner.started[0].Sandbox != kind {
				t.Errorf("the confiner was handed\n%+v\nand the policy reports\n%+v", confiner.started[0], p)
			}
			// The tools are the runner's to pass, not the prompt's to ask for.
			args := strings.Join(lines(t, filepath.Join(l.session, "agent-args.txt")), " ")
			if !strings.Contains(args, "--tools Read,Grep ") {
				t.Errorf("the agent was not started with the granted tools alone: %s", args)
			}
			if boxed := strings.Contains(args, `"sandbox":{"enabled":true`); boxed != (kind == SandboxClaude) {
				t.Errorf("claude's box on = %v for kind %s: %s", boxed, kind, args)
			}
			if kind == SandboxClaude && (!strings.Contains(args, "Bash(git:*)") || !strings.Contains(args, "--add-dir "+l.session) || strings.Contains(args, "--add-dir "+l.work)) {
				t.Errorf("claude's box: git not denied, or the writable directories are not the read-write mounts: %s", args)
			}
		})
	}
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	data, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// A kind nothing can enforce is refused when it is prepared, and nothing of
// it runs.
func TestPrepareRefusesWhatNothingEnforces(t *testing.T) {
	l := newConfinedLayout(t)
	refusal := errors.New("no confiner here")
	for kind, e := range hostEnforcers(Runner{Confiner: &fakeConfiner{check: errors.Join(ErrUnsupported, refusal)}, SystemPaths: []Mount{}}) {
		if s, err := e.Prepare(context.Background(), l.grants()); !errors.Is(err, ErrUnsupported) || !errors.Is(err, refusal) || s != nil {
			t.Errorf("%s: Prepare = %v, %v, want the confiner's refusal and no session", kind, s, err)
		}
	}
	// No platform's own confiner holds Claude's box: Landlock refuses the
	// mounts it is built with, and nothing else confines at all.
	if s, err := NewHostClaude(Runner{}).Prepare(context.Background(), l.grants()); !errors.Is(err, ErrUnsupported) || s != nil {
		t.Errorf("host/claude under the platform's confiner: %v, %v, want ErrUnsupported", s, err)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	for kind, e := range map[string]Enforcer{SandboxNone: NewHostNone(Runner{Confiner: &fakeConfiner{}}), SandboxContainer: NewContainer(Runner{}, "image")} {
		if _, err := e.Prepare(cancelled, l.grants()); !errors.Is(err, context.Canceled) {
			t.Errorf("%s: Prepare with a cancelled context: %v", kind, err)
		}
	}
}

// Grants are checked when they are prepared, before any request exists.
func TestPrepareChecksTheGrantsOnTheirOwn(t *testing.T) {
	l := newConfinedLayout(t)
	if err := os.Mkdir(filepath.Join(l.outside, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, tc := range map[string]struct {
		grants func(*Grants)
		want   error
	}{
		"VCS variable without VCS":   {func(g *Grants) { g.Env = append(g.Env, "GH_TOKEN") }, ErrNotGranted},
		"relative mount":             {func(g *Grants) { g.Mounts = append(g.Mounts, Mount{Path: "work", Access: ReadOnly}) }, ErrNotGranted},
		"unknown access":             {func(g *Grants) { g.Mounts[0].Access = "rx" }, nil},
		"one tool of a server":       {func(g *Grants) { g.Tools = append(g.Tools, "mcp__tools__done") }, nil},
		"writable VCS metadata":      {func(g *Grants) { g.Mounts = append(g.Mounts, Mount{Path: l.outside, Access: ReadWrite}) }, ErrNotGranted},
		"mount outside its confines": {func(g *Grants) { g.Within = l.work }, ErrNotGranted},
		"a mount that is git": {func(g *Grants) {
			g.Mounts = append(g.Mounts, Mount{Path: filepath.Join(l.tools, "git"), Access: ReadOnly})
		}, ErrNotGranted},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv("PATH", l.tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			g := l.grants()
			tc.grants(&g)
			for kind, e := range map[string]Enforcer{
				SandboxNone:      NewHostNone(Runner{Confiner: &fakeConfiner{}, SystemPaths: []Mount{}}),
				SandboxContainer: NewContainer(Runner{DockerBin: agenttest.Docker(t, "image", "RUN_DIR")}, "image"),
			} {
				if kind == SandboxContainer && name == "a mount that is git" {
					continue // the host's git is nothing to a container
				}
				s, err := e.Prepare(context.Background(), g)
				if err == nil || s != nil || (tc.want != nil && !errors.Is(err, tc.want)) {
					t.Errorf("%s: Prepare = %v, %v, want a refusal (%v)", kind, s, err, tc.want)
				}
			}
		})
	}
	if _, err := NewPolicy("firejail", l.grants()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a policy for a kind there is none of: %v", err)
	}
	if _, err := NewContainer(Runner{}, "").Prepare(context.Background(), l.grants()); !errors.Is(err, ErrUnsupported) {
		t.Errorf("a container enforcer with no image: %v", err)
	}
}

// The session decides the grants, the sandbox and the confinement of every
// request, and refuses a request that names others.
func TestASessionRunsEveryRequestUnderItsOwnGrants(t *testing.T) {
	l := newConfinedLayout(t)
	t.Setenv("PATH", l.tools+string(os.PathListSeparator)+os.Getenv("PATH"))
	marker := filepath.Join(l.session, "agent-args.txt")
	newSession := func(t *testing.T, g Grants) (Session, *fakeConfiner) {
		confiner := &fakeConfiner{}
		s, err := NewHostNone(Runner{ClaudeBin: okAgent(t), Confiner: confiner, SystemPaths: []Mount{}}).Prepare(context.Background(), g)
		if err != nil {
			t.Fatal(err)
		}
		return s, confiner
	}
	for name, tc := range map[string]struct {
		request func(*Request)
		want    error
	}{
		"grants of its own": {func(r *Request) {
			g := l.grants()
			g.Mounts = append(g.Mounts, Mount{Path: l.outside, Access: ReadWrite})
			r.Grants = &g
		}, ErrNotGranted},
		"another sandbox":          {func(r *Request) { r.Profile.Sandbox = SandboxContainer; r.Profile.SandboxImage = "image" }, ErrUnsupported},
		"VCS that was not granted": {func(r *Request) { r.Profile.VCSAccess = true }, ErrNotGranted},
		"a server not granted":     {func(r *Request) { r.Profile.MCP = map[string]MCPEntry{"other": {Command: "x"}} }, ErrNotGranted},
		"a variable not granted":   {func(r *Request) { r.Env["EXTRA"] = "x" }, ErrNotGranted},
		"a directory not granted":  {func(r *Request) { r.Workspace = vcs.Directory(l.outside) }, ErrNotGranted},
	} {
		t.Run(name, func(t *testing.T) {
			s, confiner := newSession(t, l.grants())
			req := l.turn()
			tc.request(&req)
			if res, err := s.Run(context.Background(), req); !errors.Is(err, tc.want) {
				t.Fatalf("run = %+v, %v, want %v", res, err, tc.want)
			}
			if _, err := os.Stat(marker); err == nil || len(confiner.started) != 0 {
				t.Fatal("the refused request ran")
			}
		})
	}

	// What the session accepts: its own grants again, its own sandbox by
	// name, and a request unconfined and unsandboxed by its profile, which
	// runs confined all the same.
	s, confiner := newSession(t, l.grants())
	req := l.turn()
	g := l.grants()
	req.Grants, req.Profile.Sandbox, req.Profile.Confine = &g, SandboxNone, false
	if res, err := s.Run(context.Background(), req); err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	if len(confiner.started) != 1 {
		t.Fatalf("a request whose profile is not confined ran through the confiner %d times, want 1", len(confiner.started))
	}

	// Granted VCS is the turn's, whatever its profile asked for.
	g = l.grants()
	g.VCS = true
	g.Mounts = append(g.Mounts, Mount{Path: l.tools, Access: ReadOnly})
	s, confiner = newSession(t, g)
	if p := s.Policy(); !p.VCS || len(p.Denied)+len(p.DeniedExecutables) != 0 || !p.Runs(filepath.Join(l.tools, "git")) {
		t.Errorf("policy with VCS granted: %+v", p)
	}
	if res, err := s.Run(context.Background(), l.turn()); err != nil || res.IsError {
		t.Fatalf("run with VCS: %+v, %v", res, err)
	}
	if c := confiner.started[0]; len(c.Denied) != 0 {
		t.Errorf("a turn with VCS granted was denied %v", c.Denied)
	}

	// Released, it runs nothing more, and releasing again is nothing.
	for range 2 {
		if err := s.Release(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if res, err := s.Run(context.Background(), l.turn()); !errors.Is(err, ErrReleased) || len(confiner.started) != 1 {
		t.Fatalf("run after release = %+v, %v (started %d)", res, err, len(confiner.started))
	}
}

// A turn that would be enforced differently from what Prepare reported is
// refused, not run under a policy nobody saw.
func TestATurnThePolicyDoesNotDescribeIsRefused(t *testing.T) {
	for name, change := range map[string]func(t *testing.T, l confinedLayout, r *Request){
		"another PATH, with another git on it": func(t *testing.T, l confinedLayout, r *Request) {
			writeExecutable(t, filepath.Join(l.system, "git"), "echo another git\n")
			r.Env["PATH"] = l.system
		},
		"a mount's link pointed elsewhere": func(t *testing.T, l confinedLayout, _ *Request) {
			link := filepath.Join(filepath.Dir(l.work), "link")
			if err := os.Remove(link); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(l.outside, link); err != nil {
				t.Fatal(err)
			}
		},
		"an agent installed since": func(t *testing.T, l confinedLayout, r *Request) {
			writeExecutable(t, filepath.Join(l.outside, "codex"), "cat >/dev/null\n")
			r.Profile.Agent = AgentCodex
		},
	} {
		t.Run(name, func(t *testing.T) {
			l := newConfinedLayout(t)
			t.Setenv("PATH", l.tools+string(os.PathListSeparator)+os.Getenv("PATH"))
			link := filepath.Join(filepath.Dir(l.work), "link")
			if err := os.Symlink(l.system, link); err != nil {
				t.Fatal(err)
			}
			g := l.grants()
			g.Tools = []string{ToolsAll}
			g.Mounts = append(g.Mounts, Mount{Path: link, Access: ReadOnly})
			confiner := &fakeConfiner{}
			r := Runner{ClaudeBin: okAgent(t), CodexBin: filepath.Join(l.outside, "codex"), Confiner: confiner, SystemPaths: []Mount{}}
			s, err := NewHostNone(r).Prepare(context.Background(), g)
			if err != nil {
				t.Fatal(err)
			}
			req := l.turn()
			if res, err := s.Run(context.Background(), req); err != nil || res.IsError {
				t.Fatalf("unchanged: %+v, %v", res, err)
			}
			change(t, l, &req)
			res, err := s.Run(context.Background(), req)
			if !errors.Is(err, ErrPolicyChanged) {
				t.Fatalf("run = %+v, %v, want ErrPolicyChanged", res, err)
			}
			if len(confiner.started) != 1 {
				t.Fatalf("the changed turn was started (%d starts)", len(confiner.started))
			}
		})
	}
}

// Every part of the policy a turn could differ in is compared.
func TestAdmitComparesEveryPartOfThePolicy(t *testing.T) {
	host := Policy{Sandbox: SandboxNone, Tools: []string{"Read"}, Mounts: []Mount{{Path: "/work", Access: ReadOnly}},
		System: []Mount{{Path: "/usr", Access: ReadOnly}}, DeniedExecutables: []string{"git"}, Denied: []string{"/usr/bin/git"}}
	hostTurn := func() *Turn {
		return &Turn{Tools: []string{"Read"}, Mounts: slices.Clone(host.Mounts), DeniedExecutables: []string{"git"},
			Confinement: &Confinement{Sandbox: SandboxNone, Mounts: slices.Clone(host.Mounts), System: slices.Clone(host.System), Denied: []string{"/usr/bin/git"}}}
	}
	mask := Bind{Source: "/tmp/masks/denied", Destination: "/usr/bin/git", Access: ReadOnly}
	box := Policy{Sandbox: SandboxContainer, Mounts: []Mount{{Path: "/work", Access: ReadOnly}}, DeniedExecutables: []string{"git"},
		Denied: []string{"/usr/bin/git"}, Binds: []Bind{{Source: "/work", Destination: "/work", Access: ReadOnly}, mask}}
	boxTurn := func() *Turn {
		return &Turn{Mounts: slices.Clone(box.Mounts), DeniedExecutables: []string{"git"}, Binds: slices.Clone(box.Binds)}
	}
	if err := (&held{policy: host}).admit(hostTurn()); err != nil {
		t.Fatalf("the turn the host policy describes: %v", err)
	}
	if err := (&held{policy: box, masks: []Bind{mask}}).admit(boxTurn()); err != nil {
		t.Fatalf("the turn the container policy describes: %v", err)
	}
	// A granted mount again under a link's name is the one thing a turn adds.
	aliased := boxTurn()
	aliased.Binds = append(aliased.Binds, Bind{Source: "/work", Destination: "/link-to-work", Access: ReadOnly})
	if err := (&held{policy: box, masks: []Bind{mask}}).admit(aliased); err != nil {
		t.Fatalf("a granted mount bound again under an alias: %v", err)
	}
	for name, tc := range map[string]struct {
		policy Policy
		turn   func() *Turn
		change func(*Turn)
	}{
		"not confined": {host, hostTurn, func(u *Turn) { u.Confinement = nil }},
		// With no system path and nothing denied, only its confinement tells
		// a host turn from one that is not held at all.
		"not confined, and nothing else to tell by": {Policy{Sandbox: SandboxNone, Mounts: host.Mounts}, func() *Turn { return &Turn{Mounts: slices.Clone(host.Mounts)} }, func(*Turn) {}},
		"another sandbox":    {host, hostTurn, func(u *Turn) { u.Confinement.Sandbox = SandboxClaude }},
		"every tool":         {host, hostTurn, func(u *Turn) { u.Tools = nil }},
		"another tool":       {host, hostTurn, func(u *Turn) { u.Tools = []string{"Read", "Bash"} }},
		"a writable mount":   {host, hostTurn, func(u *Turn) { u.Confinement.Mounts[0].Access = ReadWrite }},
		"more of the system": {host, hostTurn, func(u *Turn) { u.Confinement.System = append(u.Confinement.System, Mount{Path: "/", Access: ReadOnly}) }},
		"VCS":                {host, hostTurn, func(u *Turn) { u.VCS = true }},
		"git not shadowed":   {host, hostTurn, func(u *Turn) { u.DeniedExecutables = nil }},
		"git not denied":     {host, hostTurn, func(u *Turn) { u.Confinement.Denied = nil }},
		"a confined box":     {box, boxTurn, func(u *Turn) { u.Confinement = &Confinement{} }},
		"no mask":            {box, boxTurn, func(u *Turn) { u.Binds = u.Binds[:1] }},
		"a bind of its own": {box, boxTurn, func(u *Turn) {
			u.Binds = append(u.Binds, Bind{Source: "/etc", Destination: "/host-etc", Access: ReadOnly})
		}},
		"a writable alias":    {box, boxTurn, func(u *Turn) { u.Binds = append(u.Binds, Bind{Source: "/work", Destination: "/rw", Access: ReadWrite}) }},
		"a mount bound wider": {box, boxTurn, func(u *Turn) { u.Binds[0].Access = ReadWrite }},
	} {
		turn := tc.turn()
		tc.change(turn)
		if err := (&held{policy: tc.policy, masks: []Bind{mask}}).admit(turn); !errors.Is(err, ErrPolicyChanged) {
			t.Errorf("%s: %v, want ErrPolicyChanged", name, err)
		}
	}
}

// Reads and Writes follow the rules a confiner is handed, where an inner
// mount decides and a directory gone around takes nothing new.
func TestThePolicyJudgesPathsTheWayTheyAreEnforced(t *testing.T) {
	base := realTemp(t)
	rw, ro := filepath.Join(base, "rw"), filepath.Join(base, "rw", "ro")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{ro, outside} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []string{filepath.Join(rw, "file"), filepath.Join(ro, "file"), filepath.Join(outside, "file")} {
		if err := os.WriteFile(f, nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink(outside, filepath.Join(rw, "escape")); err != nil {
		t.Fatal(err)
	}
	mounts := []Mount{{Path: rw, Access: ReadWrite}, {Path: ro, Access: ReadOnly}}
	for _, kind := range []string{SandboxNone, SandboxContainer} {
		p := Policy{Sandbox: kind, Mounts: mounts}
		for path, want := range map[string][2]bool{
			filepath.Join(rw, "file"):           {true, true},
			filepath.Join(ro, "file"):           {true, false},
			filepath.Join(ro, "new"):            {true, false},
			filepath.Join(rw, "escape", "file"): {false, false},
			filepath.Join(outside, "file"):      {false, false},
			"rw/file":                           {false, false},
			// Landlock goes around the read-only mount entry by entry, and
			// the directory that holds it takes nothing new; a container's
			// read-write bind does.
			filepath.Join(rw, "new"): {kind == SandboxContainer, kind == SandboxContainer},
		} {
			if got := [2]bool{p.Reads(path), p.Writes(path)}; got != want {
				t.Errorf("%s %s: reads, writes = %v, want %v", kind, path, got, want)
			}
		}
	}
	// A container runs what its image holds, the masked paths apart, and a
	// mask over a path of a mount hides the host's file there.
	box := Policy{Sandbox: SandboxContainer, Mounts: mounts, Denied: []string{"/usr/bin/git", "/usr/lib/git-core", filepath.Join(rw, "file")}}
	if box.Reads(filepath.Join(rw, "file")) || box.Writes(filepath.Join(rw, "file")) || !box.Reads(filepath.Join(ro, "file")) {
		t.Errorf("a masked path of a mount is read or written, or another is not")
	}
	for path, want := range map[string]bool{"/usr/bin/env": true, "/usr/bin/git": false, "/usr/lib/git-core/git-upload-pack": false, "usr/bin/env": false} {
		if got := box.Runs(path); got != want {
			t.Errorf("container runs %s = %v, want %v", path, got, want)
		}
	}
}
