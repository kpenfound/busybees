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

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

// hostFlags are the engine flags that would give a container something of
// the host that is not one of its binds.
var hostFlags = []string{"-v", "--volume", "--volumes-from", "--privileged", "--device", "--pid", "--ipc", "--cap-add", "--security-opt"}

// mountArgs are the --mount values of a recorded engine command line.
func mountArgs(args []string) []string {
	var mounts []string
	for i, a := range args {
		if a == "--mount" && i+1 < len(args) {
			mounts = append(mounts, args[i+1])
		}
	}
	return mounts
}

func bindArg(b Bind) string {
	spec := "type=bind,source=" + b.Source + ",destination=" + b.Destination
	if b.Access == ReadOnly {
		spec += ",readonly"
	}
	return spec
}

// A container session is the same three calls. What it reports after Prepare
// is what the engine is told when a turn starts: the granted mounts with
// their access, the stand-ins over the image's VCS executables, and nothing
// else of the host.
func TestAContainerSessionEnforcesThePolicyItReports(t *testing.T) {
	l := newConfinedLayout(t)
	secret := filepath.Join(l.outside, "secret.txt")
	pinned := filepath.Join(l.work, "readme.txt")
	for _, p := range []string{secret, pinned} {
		if err := os.WriteFile(p, []byte("x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	docker := agenttest.Docker(t, "image", "ARGS_DIR")
	engine := filepath.Dir(docker)
	// The image has a git, its directory of subcommands, and a git inside it.
	image := "f /usr/bin/git\nd /usr/lib/git-core\nf /usr/lib/git-core/git\nf /usr/bin/git\n"
	if err := os.WriteFile(filepath.Join(engine, "image-vcs.txt"), []byte(image), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewContainer(Runner{ClaudeBin: okAgent(t), DockerBin: docker}, "image").Prepare(context.Background(), l.grants())
	if err != nil {
		t.Fatal(err)
	}

	// The look into the image is given nothing of the host and no network.
	probe := lines(t, filepath.Join(engine, "docker-probe.txt"))
	if got := strings.Join(probe[:8], " "); got != "run --rm --network none --entrypoint /bin/sh image -c" || len(mountArgs(probe)) != 0 {
		t.Fatalf("the image was looked into with: %v", probe)
	}

	p := s.Policy()
	if len(p.Binds) != 4 {
		t.Fatalf("binds = %v, want the two mounts and two stand-ins", p.Binds)
	}
	masks := p.Binds[2:]
	want := Policy{
		Sandbox:           SandboxContainer,
		Image:             "image",
		Env:               []string{"PATH", "ARGS_DIR"},
		Tools:             []string{"Read", "Grep"},
		MCPServers:        []string{"tools"},
		Mounts:            []Mount{{Path: l.work, Access: ReadOnly}, {Path: l.session, Access: ReadWrite}},
		DeniedExecutables: VCSExecutables,
		Denied:            []string{"/usr/bin/git", "/usr/lib/git-core"},
		Binds: []Bind{
			{Source: l.work, Destination: l.work, Access: ReadOnly},
			{Source: l.session, Destination: l.session, Access: ReadWrite},
			{Source: masks[0].Source, Destination: "/usr/bin/git", Access: ReadOnly},
			{Source: masks[1].Source, Destination: "/usr/lib/git-core", Access: ReadOnly},
		},
	}
	if got, want := jsonOf(t, p), jsonOf(t, want); got != want {
		t.Fatalf("policy after Prepare:\n got %s\nwant %s", got, want)
	}
	// The stand-ins: an executable that refuses, and a directory with
	// nothing in it, neither inside a mount.
	if out, err := exec.Command(masks[0].Source, "--version").CombinedOutput(); exitCode(err) != 126 || !strings.Contains(string(out), "not granted") {
		t.Errorf("the stand-in over git: %s, %v, want exit 126", out, err)
	}
	if entries, err := os.ReadDir(masks[1].Source); err != nil || len(entries) != 0 {
		t.Errorf("the stand-in over git's subcommands: %v, %v, want an empty directory", entries, err)
	}
	for name, tc := range map[string]struct{ got, want bool }{
		"reads its working directory":       {p.Reads(pinned), true},
		"reads outside its mounts":          {p.Reads(secret), false},
		"writes its read-only working dir":  {p.Writes(pinned), false},
		"creates in its read-only work dir": {p.Writes(filepath.Join(l.work, "new.txt")), false},
		"creates in its session directory":  {p.Writes(filepath.Join(l.session, "new.txt")), true},
		"runs git by its path":              {p.Runs("/usr/bin/git"), false},
		"runs git's subcommands":            {p.Runs("/usr/lib/git-core/git"), false},
		"runs the image's other programs":   {p.Runs("/usr/bin/env"), true},
		"has an ungranted tool":             {p.Allows("Bash"), false},
	} {
		if tc.got != tc.want {
			t.Errorf("the policy says the turn %s: %v, want %v", name, tc.got, tc.want)
		}
	}

	res, err := s.Run(context.Background(), l.turn())
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	args := lines(t, filepath.Join(l.session, "docker-args.txt"))
	var reported []string
	for _, b := range p.Binds {
		reported = append(reported, bindArg(b))
	}
	// The runner's own tmpfs home is the one mount that is not a bind.
	got := slices.DeleteFunc(mountArgs(args), func(m string) bool { return strings.HasPrefix(m, "type=tmpfs,") })
	slices.Sort(got)
	slices.Sort(reported)
	if !slices.Equal(got, reported) {
		t.Errorf("the engine was told to mount\n%v\nand the policy reports\n%v", got, reported)
	}
	if !slices.Contains(got, "type=bind,source="+l.work+",destination="+l.work+",readonly") {
		t.Errorf("the read-only working directory is not bound read-only: %v", got)
	}
	for _, flag := range hostFlags {
		if slices.ContainsFunc(args, func(a string) bool { return a == flag || strings.HasPrefix(a, flag+"=") }) {
			t.Errorf("the engine was given %s: %v", flag, args)
		}
	}
	if agent := strings.Join(lines(t, filepath.Join(l.session, "agent-args.txt")), " "); !strings.Contains(agent, "--tools Read,Grep ") {
		t.Errorf("the agent was not started with the granted tools alone: %s", agent)
	}

	// Release takes the stand-ins away, and the session runs nothing more.
	if err := s.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(masks[0].Source)); !os.IsNotExist(err) {
		t.Errorf("the stand-ins are still there after Release: %v", err)
	}
	if _, err := s.Run(context.Background(), l.turn()); !errors.Is(err, ErrReleased) {
		t.Errorf("run after release: %v, want ErrReleased", err)
	}
}

func exitCode(err error) int {
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode()
	}
	return 0
}

// With VCS granted the image is not looked into and nothing is masked; an
// image the engine cannot look into is not prepared at all.
func TestAContainerSessionLooksIntoItsImageOnlyToDeny(t *testing.T) {
	l := newConfinedLayout(t)
	docker := agenttest.Docker(t, "image", "ARGS_DIR")
	engine := filepath.Dir(docker)
	if err := os.WriteFile(filepath.Join(engine, "image-vcs.txt"), []byte("f /usr/bin/git\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	e := NewContainer(Runner{ClaudeBin: okAgent(t), DockerBin: docker}, "image")
	g := l.grants()
	g.VCS = true
	s, err := e.Prepare(context.Background(), g)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(engine, "docker-probe.txt")); err == nil {
		t.Error("the image was looked into for a session with VCS")
	}
	if p := s.Policy(); len(p.Denied)+len(p.DeniedExecutables) != 0 || len(p.Binds) != 2 || !p.Runs("/usr/bin/git") {
		t.Errorf("policy with VCS: %+v", p)
	}
	if res, err := s.Run(context.Background(), l.turn()); err != nil || res.IsError {
		t.Fatalf("run with VCS: %+v, %v", res, err)
	}
	if got := mountArgs(lines(t, filepath.Join(l.session, "docker-args.txt"))); slices.ContainsFunc(got, func(m string) bool { return strings.Contains(m, "/usr/bin/git") }) {
		t.Errorf("git masked with VCS granted: %v", got)
	}

	masksParent = t.TempDir()
	defer func() { masksParent = "" }()
	for name, arrange := range map[string]func(){
		"the engine fails":          func() { _ = os.WriteFile(filepath.Join(engine, "fail-probe"), nil, 0o644) },
		"the probe prints nonsense": func() { _ = os.WriteFile(filepath.Join(engine, "image-vcs.txt"), []byte("f usr/bin/git\n"), 0o644) },
		"a path --mount cannot take": func() {
			_ = os.WriteFile(filepath.Join(engine, "image-vcs.txt"), []byte("f /usr/bin/g,it\n"), 0o644)
		},
	} {
		arrange()
		if s, err := e.Prepare(context.Background(), l.grants()); err == nil || s != nil {
			t.Errorf("%s: Prepare = %v, %v, want a refusal", name, s, err)
		}
		if left, err := os.ReadDir(masksParent); err != nil || len(left) != 0 {
			t.Errorf("%s: stand-ins left behind: %v, %v", name, left, err)
		}
		_ = os.Remove(filepath.Join(engine, "fail-probe"))
	}
}

// The image is the one Prepare looked into: a request that names another,
// or an environment to build one from, is refused.
func TestAContainerSessionRunsTheImageItPrepared(t *testing.T) {
	l := newConfinedLayout(t)
	docker := agenttest.Docker(t, "image", "ARGS_DIR")
	s, err := NewContainer(Runner{ClaudeBin: okAgent(t), DockerBin: docker}, "image").Prepare(context.Background(), l.grants())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Release(context.Background()) }()
	for name, change := range map[string]func(*Profile){
		"another image":            func(p *Profile) { p.SandboxImage = "other" },
		"an environment to build":  func(p *Profile) { p.ContainerUseEnvironment = "env" },
		"a host sandbox":           func(p *Profile) { p.Sandbox = SandboxNone },
		"a confined host sandbox":  func(p *Profile) { p.Sandbox = SandboxClaude; p.Confine = true },
		"the image and more tools": func(p *Profile) { p.SandboxImage = "image"; p.AllowedTools = []string{"Bash"} },
	} {
		req := l.turn()
		change(&req.Profile)
		if res, err := s.Run(context.Background(), req); err == nil || (!errors.Is(err, ErrUnsupported) && !errors.Is(err, ErrNotGranted)) {
			t.Errorf("%s: run = %+v, %v, want a refusal", name, res, err)
		}
	}
	if _, err := os.Stat(filepath.Join(l.session, "docker-args.txt")); err == nil {
		t.Error("a refused request reached the engine")
	}
	req := l.turn()
	req.Profile.Sandbox, req.Profile.SandboxImage = SandboxContainer, "image"
	if res, err := s.Run(context.Background(), req); err != nil || res.IsError {
		t.Fatalf("the prepared image by name: %+v, %v", res, err)
	}
}

// The probe finds, inside an image, what executablePaths finds on the host:
// the file a name resolves to, and git's directory of subcommand programs.
func TestTheImageProbeFindsVCSExecutables(t *testing.T) {
	root := realTemp(t)
	bin, opt := filepath.Join(root, "bin"), filepath.Join(root, "opt", "git", "bin")
	core := filepath.Join(root, "opt", "git", "libexec", "git-core")
	for _, d := range []string{bin, opt, core} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeExecutable(t, filepath.Join(opt, "git"), "exit 0\n")
	writeExecutable(t, filepath.Join(bin, "hg"), "exit 0\n")
	writeExecutable(t, filepath.Join(bin, "other"), "exit 0\n")
	if err := os.Symlink(filepath.Join(opt, "git"), filepath.Join(bin, "git")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(bin, "svn"), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", vcsProbe)
	cmd.Env = []string{"PATH=relative:" + bin + "::" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	found, err := parseImageVCS(string(out))
	if err != nil {
		t.Fatalf("%v in:\n%s", err, out)
	}
	for _, want := range []imagePath{{path: filepath.Join(opt, "git")}, {path: filepath.Join(bin, "hg")}, {path: core, dir: true}} {
		if !slices.Contains(found, want) {
			t.Errorf("the probe did not find %+v in:\n%s", want, out)
		}
	}
	for _, f := range found {
		if strings.HasPrefix(f.path, bin) && f.path != filepath.Join(bin, "hg") {
			t.Errorf("the probe found %+v: not a VCS executable, or a link and not the file", f)
		}
	}
}

// What the probe prints is read strictly: a line that is not a kind and a
// clean absolute path below the root is an image not looked into, not a
// path left unmasked.
func TestTheImageProbeIsReadStrictly(t *testing.T) {
	found, err := parseImageVCS("f /usr/bin/git\n\nd /usr/lib/git-core\nf /usr/bin/git\n")
	if err != nil || !slices.Equal(found, []imagePath{{path: "/usr/bin/git"}, {path: "/usr/lib/git-core", dir: true}}) {
		t.Fatalf("parse = %v, %v", found, err)
	}
	if found, err := parseImageVCS(""); err != nil || len(found) != 0 {
		t.Errorf("an image with no VCS executables: %v, %v", found, err)
	}
	for name, out := range map[string]string{
		"an unknown kind":    "x /usr/bin/git\n",
		"a relative path":    "f usr/bin/git\n",
		"a path not clean":   "f /usr/bin/../bin/git\n",
		"a trailing slash":   "d /usr/lib/git-core/\n",
		"the root":           "d /\n",
		"a kind and no path": "f\n",
		"a good line first":  "f /usr/bin/git\nnonsense\n",
	} {
		if found, err := parseImageVCS(out); err == nil {
			t.Errorf("%s: parsed as %v, want a refusal", name, found)
		}
	}
}
