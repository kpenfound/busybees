package agent

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sync"

	"github.com/kpenfound/busybees/core/agent/agentbin"
)

// Enforcer prepares turns of one sandbox kind that are held to their grants
// by something other than the agent: the operating system for a host kind
// (the confined HostBoundary), the engine's binds for a container. There is
// one constructor per kind: NewHostNone, NewHostClaude and NewContainer.
//
//	session, err := enforcer.Prepare(ctx, grants)
//	// inspect session.Policy(), and refuse to go on when it is not what was meant
//	result, err := session.Run(ctx, request)
//	err = session.Release(ctx)
type Enforcer interface {
	// Prepare checks the grants on their own and asks the platform whether
	// it can enforce them, without starting an agent. A kind this platform
	// cannot enforce is refused with an error that wraps ErrUnsupported,
	// never prepared with less.
	Prepare(ctx context.Context, grants Grants) (Session, error)
}

// Session is one prepared set of grants.
type Session interface {
	// Policy is what the session enforces on every turn it runs.
	Policy() Policy
	// Run runs one turn under the session's grants: see Admit for what it
	// does to the request. A turn the policy does not describe, because
	// something the policy was read from has changed since Prepare, is
	// refused with ErrPolicyChanged before anything starts. Otherwise it is
	// Runner.Run.
	Run(ctx context.Context, req Request) (*Result, error)
	// Release frees what Prepare made. Run is refused with ErrReleased
	// afterwards. Call it once no turn of the session is running; calling it
	// again does nothing.
	Release(ctx context.Context) error
}

// ErrReleased is returned by Run on a session that was released.
var ErrReleased = errors.New("session was released")

// ErrPolicyChanged wraps the refusal of a turn that is not the one the
// session's policy describes.
var ErrPolicyChanged = errors.New("turn differs from the session's policy")

// NewHostNone returns the enforcer of host turns with no sandbox of the
// agent's own. The turn is a confined one (Profile.Confine): r.Confiner, or
// the platform's, holds it to its mounts and r.SystemPaths.
func NewHostNone(r Runner) Enforcer { return &hostEnforcer{kind: SandboxNone, r: r} }

// NewHostClaude returns the enforcer of host turns under Claude Code's
// sandbox, confined the same way. Seatbelt holds it on macOS. Landlock
// cannot hold Claude's box, which is built with mount(2), so on Linux
// Prepare refuses unless r.Confiner is one that can.
func NewHostClaude(r Runner) Enforcer { return &hostEnforcer{kind: SandboxClaude, r: r} }

// NewContainer returns the enforcer of turns in a container of image. The
// container is given the granted mounts and nothing else of the host. For
// grants without VCS, Prepare runs the image once, with no network and no
// mounts, to find its VCS executables: the files the names in VCSExecutables
// resolve to in the image's PATH and the usual system directories, and git's
// directory of subcommand programs. Each is bound over, read-only, by a
// stand-in that exits 126, so it is denied by absolute path and from a shell
// too. A VCS executable anywhere else in the image, a hard link to one, or
// one the turn fetches is not found. The stand-ins are files of the runner's
// under the temporary directory, which the engine must be able to bind;
// Release removes them.
func NewContainer(r Runner, image string) Enforcer { return &containerEnforcer{r: r, image: image} }

// Policy is what a session enforces. Every path of the host has its symbolic
// links resolved, a bind's destination apart, which is also the path a mount
// was granted by. It carries names and paths and no values: the
// environment's values are the turn's.
type Policy struct {
	// Sandbox is the kind: SandboxNone, SandboxClaude or SandboxContainer.
	Sandbox string
	// Env is the allowlist of variable names the turn's environment is
	// built from.
	Env []string
	// Tools are the built-in tools the agent is started with; nil is all
	// of them. MCPServers are the servers it may be given.
	Tools      []string
	MCPServers []string
	// Mounts are the granted paths. Where one lies inside another the inner
	// one decides.
	Mounts []Mount
	// System is what a host turn reaches beyond its mounts: the system
	// paths, and the executables of the agents the runner names, of which a
	// turn runs one. Nil for a container, which reaches its image and
	// nothing else of the host.
	System []Mount
	// VCS is whether version control is granted.
	VCS bool
	// DeniedExecutables are the names shadowed on the turn's PATH, and
	// Denied the paths that can be neither read nor executed: on the host
	// for a host turn, inside the image for a container.
	DeniedExecutables []string
	Denied            []string
	// Image is what a container turn runs, and Binds everything it is given
	// of the host: the mounts, and the stand-ins over Denied. A turn adds
	// to them only a granted mount again, under the path a symbolic link
	// names a directory of the request by.
	Image string
	Binds []Bind
}

// NewPolicy checks grants on their own, the way every Enforcer does before
// it asks its platform anything, and returns the policy they describe for a
// sandbox kind: no system paths and no denied paths, which a platform adds,
// and for a container the binds of its mounts and no image.
func NewPolicy(sandbox string, g Grants) (Policy, error) {
	kind, err := sandboxKind(sandbox)
	if err != nil {
		return Policy{}, err
	}
	if err := checkEnvGrants(&g); err != nil {
		return Policy{}, err
	}
	tools, servers, err := splitTools(g.Tools)
	if err != nil {
		return Policy{}, err
	}
	mounts, err := resolveMounts(&g, g.VCS)
	if err != nil {
		return Policy{}, err
	}
	p := Policy{Sandbox: kind, Env: slices.Clone(g.Env), MCPServers: slices.Sorted(maps.Keys(servers)), Mounts: mounts, VCS: g.VCS}
	if !slices.Contains(tools, ToolsAll) {
		p.Tools = append([]string{}, tools...)
	}
	if !g.VCS {
		p.DeniedExecutables = slices.Clone(VCSExecutables)
	}
	if kind == SandboxContainer {
		set := &bindSet{}
		if err := set.mounts(g.Mounts, mounts); err != nil {
			return Policy{}, err
		}
		p.Binds = set.binds
	}
	return p, nil
}

func sandboxKind(sandbox string) (string, error) {
	switch sandbox {
	case "":
		return SandboxNone, nil
	case SandboxNone, SandboxClaude, SandboxContainer:
		return sandbox, nil
	}
	return "", fmt.Errorf("%w: sandbox %q is not a kind an enforcer holds (%s, %s, %s)", ErrUnsupported, sandbox, SandboxNone, SandboxClaude, SandboxContainer)
}

// Admit returns req as a session with policy p, prepared with g, runs it, or
// the reason it does not: the grants are the session's, and a request that
// carries others is refused; the profile's sandbox and image are the
// session's, a host session's image being none, and a profile that names
// another, or a container-use environment to build one from, is refused
// whatever the kind; a host turn is confined; granted VCS is given to the
// turn, and a profile that asks for VCS that was not granted is refused. The
// request is then checked against the grants the way every boundary checks
// it.
func Admit(p Policy, g Grants, req Request) (Request, error) {
	req, err := admit(p, g, req)
	if err != nil {
		return Request{}, err
	}
	if err := req.Profile.Validate(); err != nil {
		return Request{}, err
	}
	if _, err := verifyCommon(req); err != nil {
		return Request{}, err
	}
	return req, nil
}

func admit(p Policy, g Grants, req Request) (Request, error) {
	kind, err := sandboxKind(p.Sandbox)
	if err != nil {
		return Request{}, err
	}
	if req.Grants != nil && !equalGrants(*req.Grants, g) {
		return Request{}, fmt.Errorf("%w: the request carries grants of its own, and the session runs under the ones it was prepared with", ErrNotGranted)
	}
	grants := cloneGrants(g)
	req.Grants = &grants
	if asked, _ := sandboxKind(req.Profile.Sandbox); req.Profile.Sandbox != "" && asked != kind {
		return Request{}, fmt.Errorf("%w: the profile asks for sandbox %q and the session was prepared for %q", ErrUnsupported, req.Profile.Sandbox, kind)
	}
	req.Profile.Sandbox = kind
	req.Profile.Confine = kind != SandboxContainer
	// The image is the one Prepare looked into, and a host session has none:
	// a profile that names another is not run with the name dropped.
	if req.Profile.ContainerUseEnvironment != "" || (req.Profile.SandboxImage != "" && req.Profile.SandboxImage != p.Image) {
		return Request{}, fmt.Errorf("%w: the session was prepared for sandbox %q and image %q, and the profile names another image or an environment to build one from", ErrUnsupported, kind, p.Image)
	}
	if kind == SandboxContainer {
		req.Profile.SandboxImage = p.Image
	}
	if g.VCS {
		req.Profile.VCSAccess = true
	}
	return req, nil
}

func equalGrants(a, b Grants) bool {
	return slices.Equal(a.Env, b.Env) && slices.Equal(a.Tools, b.Tools) && slices.Equal(a.Mounts, b.Mounts) && a.Within == b.Within && a.VCS == b.VCS
}

func cloneGrants(g Grants) Grants {
	g.Env, g.Tools, g.Mounts = slices.Clone(g.Env), slices.Clone(g.Tools), slices.Clone(g.Mounts)
	return g
}

func (p Policy) clone() Policy {
	p.Env, p.Tools, p.MCPServers = slices.Clone(p.Env), slices.Clone(p.Tools), slices.Clone(p.MCPServers)
	p.Mounts, p.System = slices.Clone(p.Mounts), slices.Clone(p.System)
	p.DeniedExecutables, p.Denied, p.Binds = slices.Clone(p.DeniedExecutables), slices.Clone(p.Denied), slices.Clone(p.Binds)
	return p
}

// Allows reports whether the turn may be given a tool: a built-in one the
// agent is started with, or a tool of an MCP server ("mcp__<server>" or
// "mcp__<server>__<tool>") the turn may have.
func (p Policy) Allows(tool string) bool {
	if server, ok := mcpServer(tool); ok {
		return slices.Contains(p.MCPServers, server)
	}
	return tool != "" && (p.Tools == nil || slices.Contains(p.Tools, tool))
}

// Reads reports whether the turn can read the host's file or directory at
// path, and Writes whether it can change or create it. Both follow the
// contract every confiner and the engine's binds are held to: the innermost
// mount that covers the path decides, a host turn's system paths add to it,
// and nothing is reached under a denied path, or on the host through a hard
// link to a denied file in a directory that holds a denied path. A path
// that is not absolute, or whose links cannot be resolved, is neither read
// nor written.
//
// A turn is never given more than they say. It can be given less: Landlock
// lets nothing be created, removed or renamed at the level of a directory
// that holds a denied path or a read-only mount inside a read-write one.
// They do not judge a file's metadata, which a host confiner leaves
// readable, or the entries of "/", which Seatbelt does.
func (p Policy) Reads(path string) bool {
	read, _ := p.access(path)
	return read
}

// Writes: see Reads.
func (p Policy) Writes(path string) bool {
	_, write := p.access(path)
	return write
}

// Runs reports whether the turn can execute the file at path, a path as the
// turn sees it. A host turn executes what it can read. A container turn sees
// its image, whose links the host knows nothing of, so its path is judged as
// it is written, cleaned and with no link followed on the host or in the
// image: it runs unless it lies under Denied, whether it is a path of the
// image or one a mount is bound at. A name the image links to a denied path
// ("/bin/git" where "/bin" is a link to "usr/bin") is therefore said to run,
// and the stand-in over the path it leads to still refuses it. A path that
// is not absolute does not run.
func (p Policy) Runs(path string) bool {
	if p.Sandbox != SandboxContainer {
		return p.Reads(path)
	}
	// denied compares cleaned paths: "/usr/bin/../bin/git" is "/usr/bin/git".
	return filepath.IsAbs(path) && !p.denied(path)
}

func (p Policy) denied(real string) bool {
	return slices.ContainsFunc(p.Denied, func(d string) bool { return inside(d, real) })
}

func (p Policy) access(path string) (read, write bool) {
	real, err := realPath(path)
	if err != nil || p.denied(real) {
		return false, false
	}
	// A mask hides one name: a container reaches a hard link to it.
	if p.Sandbox != SandboxContainer && p.deniedLink(real) {
		return false, false
	}
	if m := findMount(p.Mounts, real); m != nil {
		read, write = true, m.Access == ReadWrite
	}
	for _, m := range p.System {
		if inside(m.Path, real) {
			read, write = true, write || m.Access == ReadWrite
		}
	}
	return read, write
}

// deniedLink reports whether real is another name of a denied file, in a
// directory that holds a denied path: where every confiner looks for one.
func (p Policy) deniedLink(real string) bool {
	info, err := os.Lstat(real)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	dir := filepath.Dir(real)
	return slices.ContainsFunc(p.Denied, func(d string) bool {
		denied, err := os.Stat(d)
		return err == nil && inside(dir, d) && os.SameFile(info, denied)
	})
}

// realPath resolves the links of a path that may not exist yet.
func realPath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("path %q is not absolute", path)
	}
	return resolveCreatable(filepath.Clean(path))
}

// held is what a session's runner checks every turn against.
type held struct {
	policy Policy
	// masks are the container's stand-ins over policy.Denied.
	masks []Bind
}

// ofTurn is the policy a verified turn would report.
func (h *held) ofTurn(turn *Turn) Policy {
	p := h.policy.clone()
	p.Tools, p.Mounts, p.VCS = turn.Tools, turn.Mounts, turn.VCS
	p.DeniedExecutables = turn.DeniedExecutables
	p.System, p.Denied, p.Binds = nil, nil, nil
	if c := turn.Confinement; c != nil {
		p.Sandbox, p.Mounts, p.System, p.Denied = c.Sandbox, c.Mounts, c.System, c.Denied
		return p
	}
	for _, b := range turn.Binds {
		// A granted mount again, under another name for it.
		m := findMount(h.policy.Mounts, b.Source)
		if alias := m != nil && m.Access == b.Access; alias && !slices.Contains(h.policy.Binds, b) {
			continue
		}
		p.Binds = append(p.Binds, b)
		if slices.Contains(h.masks, b) {
			p.Denied = append(p.Denied, b.Destination)
		}
	}
	return p
}

// admit refuses a turn the policy does not describe.
func (h *held) admit(turn *Turn) error {
	if confined := turn.Confinement != nil; confined == (h.policy.Sandbox == SandboxContainer) {
		return fmt.Errorf("%w: the session holds a %s turn, and this one is confined=%v", ErrPolicyChanged, h.policy.Sandbox, confined)
	}
	want, got := h.policy, h.ofTurn(turn)
	for _, f := range []struct {
		name      string
		same      bool
		want, got any
	}{
		{"sandbox", want.Sandbox == got.Sandbox, want.Sandbox, got.Sandbox},
		{"tools", (want.Tools == nil) == (got.Tools == nil) && slices.Equal(want.Tools, got.Tools), want.Tools, got.Tools},
		{"mounts", slices.Equal(want.Mounts, got.Mounts), want.Mounts, got.Mounts},
		{"system paths", slices.Equal(want.System, got.System), want.System, got.System},
		{"VCS", want.VCS == got.VCS, want.VCS, got.VCS},
		{"denied executables", slices.Equal(want.DeniedExecutables, got.DeniedExecutables), want.DeniedExecutables, got.DeniedExecutables},
		{"denied paths", slices.Equal(want.Denied, got.Denied), want.Denied, got.Denied},
		{"binds", slices.Equal(want.Binds, got.Binds), want.Binds, got.Binds},
	} {
		if !f.same {
			return fmt.Errorf("%w: %s were %v when the session was prepared and are %v now", ErrPolicyChanged, f.name, f.want, f.got)
		}
	}
	return nil
}

// admitExecutable refuses an agent executable the policy does not reach:
// the runner adds the executable to what a confined turn reads.
func (h *held) admitExecutable(bin string) error {
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return err
	}
	if findMount(h.policy.System, real) == nil && findMount(h.policy.Mounts, real) == nil {
		return fmt.Errorf("%w: the agent's executable %s was not there when the session was prepared, and its policy does not reach it", ErrPolicyChanged, real)
	}
	return nil
}

// session is the Session of every kind.
type session struct {
	grants Grants
	// r runs the turns: the enforcer's runner, holding the policy.
	r Runner

	mu       sync.Mutex
	released bool
	release  func() error
}

func (s *session) Policy() Policy { return s.r.held.policy.clone() }

func (s *session) Run(ctx context.Context, req Request) (*Result, error) {
	s.mu.Lock()
	released := s.released
	s.mu.Unlock()
	if released {
		return nil, ErrReleased
	}
	admitted, err := admit(s.r.held.policy, s.grants, req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.Profile.Name, err)
	}
	return s.r.Run(ctx, admitted)
}

func (s *session) Release(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.released {
		return nil
	}
	s.released = true
	if s.release != nil {
		return s.release()
	}
	return nil
}

type hostEnforcer struct {
	kind string
	r    Runner
}

func (e *hostEnforcer) Prepare(ctx context.Context, g Grants) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	g = cloneGrants(g)
	policy, err := NewPolicy(e.kind, g)
	if err != nil {
		return nil, err
	}
	r := e.r
	system := r.SystemPaths
	if system == nil {
		system = DefaultSystemPaths()
	}
	// The executables are named now, so that the policy names everything a
	// turn reads: the runner adds the one a turn runs to its confinement.
	r.SystemPaths = append(slices.Clone(system), r.agentExecutables()...)
	b := HostBoundary{StripPrefix: r.EnvironmentPrefix, Confiner: r.Confiner, SystemPaths: r.SystemPaths}
	turn := &Turn{Mounts: policy.Mounts, VCS: g.VCS}
	// The environment a turn that sets nothing of its own would have: its
	// PATH is where the denied executables are looked for.
	turn.Env = b.env(Request{Grants: &g}, turn)
	c, err := b.confine(e.kind, g.Mounts, turn)
	if err != nil {
		return nil, err
	}
	policy.System, policy.Denied = c.System, c.Denied
	r.held = &held{policy: policy}
	return &session{grants: g, r: r}, nil
}

// agentExecutables are the executables of the agents the runner names, where
// this machine has them.
func (r *Runner) agentExecutables() []Mount {
	var found []Mount
	for _, bin := range []struct{ named, fallback string }{{r.ClaudeBin, "claude"}, {r.CodexBin, "codex"}, {r.OpenCodeBin, "opencode"}} {
		name := bin.named
		if name == "" {
			name = bin.fallback
		}
		path, err := agentbin.Resolve(name)
		if err != nil {
			continue // a real agent in a test binary: Run refuses it by name
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil {
			continue
		}
		if m := (Mount{Path: real, Access: ReadOnly}); !slices.Contains(found, m) {
			found = append(found, m)
		}
	}
	return found
}

// masksParent is where a container session's stand-ins are made; empty is
// the temporary directory.
var masksParent = ""

type containerEnforcer struct {
	r     Runner
	image string
}

func (e *containerEnforcer) Prepare(ctx context.Context, g Grants) (Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.image == "" {
		return nil, fmt.Errorf("%w: a container enforcer needs the image its turns run", ErrUnsupported)
	}
	g = cloneGrants(g)
	policy, err := NewPolicy(SandboxContainer, g)
	if err != nil {
		return nil, err
	}
	policy.Image = e.image
	set := &bindSet{binds: policy.Binds, index: map[string]int{}}
	for i, b := range set.binds {
		set.index[b.Destination] = i
	}
	s := &session{grants: g, r: e.r}
	h := &held{}
	if !g.VCS {
		found, err := e.r.imageVCS(ctx, e.image)
		if err != nil {
			return nil, err
		}
		dir, err := os.MkdirTemp(masksParent, "agent-masks-")
		if err != nil {
			return nil, err
		}
		s.release = func() error { return os.RemoveAll(dir) }
		if h.masks, err = writeMasks(dir, found); err == nil {
			for _, m := range h.masks {
				if err = set.add(m.Source, m.Destination, ReadOnly); err != nil {
					break
				}
				policy.Denied = append(policy.Denied, m.Destination)
			}
		}
		if err != nil {
			_ = s.release()
			return nil, err
		}
	}
	policy.Binds = set.binds
	h.policy = policy
	s.r.held = h
	return s, nil
}
