package agent

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Access is how a granted path may be used.
type Access string

const (
	ReadOnly  Access = "ro"
	ReadWrite Access = "rw"
)

// ToolsAll grants every built-in tool of the agent. MCP servers are never
// covered by it: each one is granted as "mcp__<server>".
const ToolsAll = "*"

// Mount grants one path of the host, and everything below it, to a session.
type Mount struct {
	Path   string
	Access Access
}

// Grants are the complete capabilities of one session. Whatever they do not
// name, the session does not get: the runner verifies a request against them
// before any process starts and refuses what it cannot enforce.
type Grants struct {
	// Env is the complete allowlist of environment variable names. A name
	// ending in "*" grants every name with that prefix. Host variables are
	// inherited only when listed; a variable the profile or request sets
	// that is not listed is refused.
	Env []string
	// Tools are the tools the agent may use: built-in tool names (or
	// ToolsAll) and "mcp__<server>" for each MCP server. The profile's
	// allowed tools and MCP servers may narrow them, never widen them.
	Tools []string
	// Mounts are the paths the session may read or write. The working
	// directory must lie inside one of them.
	Mounts []Mount
	// Within, when set, confines every mount: a mount that resolves outside
	// it, through ".." or a symbolic link, is refused.
	Within string
	// VCS grants version control: VCS credentials and configuration in the
	// environment, VCS executables, and writable VCS metadata. Without it
	// all three are denied.
	VCS bool
	// DaggerEngine grants the Dagger engine at this address (Dagger.Engine)
	// to a session whose profile asks for it, and only in SandboxSbx. A
	// profile that asks for another engine, or none, is refused.
	DaggerEngine string
}

// ErrNoGrants is returned for a request that carries no grants.
var ErrNoGrants = errors.New("request has no grants")

// ErrNotGranted wraps a request that asks for more than its grants.
var ErrNotGranted = errors.New("not granted")

// ErrUnsupported wraps a grant the selected boundary cannot enforce.
var ErrUnsupported = errors.New("grant cannot be enforced")

// VCSExecutables are the executables denied to a session without VCS.
var VCSExecutables = []string{"gh", "git", "hg", "jj", "svn"}

// vcsEnv are the variables that carry VCS credentials or configuration; a
// trailing "*" is a prefix. They need Grants.VCS.
var vcsEnv = []string{"GH_*", "GITHUB_*", "GIT_*", "GCM_*", "SSH_AUTH_SOCK", "SSH_AGENT_PID", "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE"}

// Boundary is where a session's process runs and what enforces its grants.
// Verify inspects a request without starting anything and returns the turn
// the boundary would run, or an error when the request asks for more than
// its grants or for a grant the boundary cannot enforce.
type Boundary interface {
	Verify(req Request) (*Turn, error)
}

// Turn is a verified request: the effective capabilities its process gets.
type Turn struct {
	// Env is the complete process environment, as NAME=value pairs, before
	// the backend's own configuration variables are added.
	Env []string
	// Tools are the built-in tools passed to the agent; nil means all.
	Tools []string
	// Mounts are the granted paths with their symbolic links resolved.
	Mounts []Mount
	// WriteDirs are the writable directories beyond the working directory
	// the agent is told about.
	WriteDirs []string
	// VCS is whether version control is granted and requested.
	VCS bool
	// DeniedExecutables are shadowed on PATH for the session.
	DeniedExecutables []string
	// Binds are everything a container or Docker Sandbox session sees of
	// the host; nil for a host session.
	Binds []Bind
	// Confinement is what the operating system enforces for a confined host
	// session; nil for any other.
	Confinement *Confinement
	// DaggerEngine is the Dagger engine the session reaches; empty for
	// none.
	DaggerEngine string
}

// Bind is one host path a container sees, at Destination inside it.
type Bind struct {
	Source      string
	Destination string
	Access      Access
}

// HostBoundary runs a session as a process of this host. It has two modes.
// By default the host enforces nothing of the mounts, so Verify asks for the
// grants that say so: "/" read-write and VCS without a sandbox, "/" read-only
// and a writable working directory under SandboxClaude. A profile with Confine
// set is held to its grants by the operating system instead: it reads the
// granted mounts and SystemPaths and nothing else, its working directory may
// be read-only, and without VCS the VCS executables cannot be read or run
// under any path. Neither host sandbox needs "/" for it, and where nothing
// can enforce it the request is refused with ErrUnsupported.
type HostBoundary struct {
	// Environ is the host environment; nil reads os.Environ.
	Environ func() []string
	// StripPrefix removes host variables with this prefix even when granted.
	StripPrefix string
	// AddDirs are directories the agent is told it may write.
	AddDirs []string
	// Confiner enforces a confined turn; nil selects this platform's, which
	// is Landlock on Linux, Seatbelt on macOS and none anywhere else.
	Confiner Confiner
	// SystemPaths are what a confined turn reaches beyond its mounts; nil
	// selects DefaultSystemPaths.
	SystemPaths []Mount
	// SessionsDir is where a session directory is created for a request
	// that names none. A confined turn must be granted it.
	SessionsDir string
	// SkillDirs hold the prepared skills; a confined turn with skills must
	// be granted them.
	SkillDirs []string
}

// Verify checks the request and builds the host turn. It fails closed: a
// request without grants, with an unknown access mode, or with a grant the
// host cannot enforce is refused.
func (h HostBoundary) Verify(req Request) (*Turn, error) {
	turn, err := verifyCommon(req)
	if err != nil {
		return nil, err
	}
	p := req.Profile
	root := findMount(turn.Mounts, string(filepath.Separator))
	switch {
	case p.Sandbox != "" && p.Sandbox != SandboxNone && p.Sandbox != SandboxClaude:
		return nil, fmt.Errorf("%w: sandbox %q is not a host sandbox", ErrUnsupported, p.Sandbox)
	case p.Confine:
		// The operating system holds the turn to its mounts: checked below,
		// once the environment the executables are searched in is known.
	case p.Sandbox != SandboxClaude:
		// An unsandboxed process reaches everything its user can.
		if root == nil || root.Access != ReadWrite {
			return nil, fmt.Errorf("%w: a host session without a sandbox reaches the whole filesystem; grant %q %s, or confine it", ErrUnsupported, "/", ReadWrite)
		}
		if !turn.VCS {
			return nil, fmt.Errorf("%w: a host session without a sandbox can write VCS metadata; grant VCS or use sandbox %q", ErrUnsupported, SandboxClaude)
		}
	default:
		// Claude's box reads everywhere and writes the working directory
		// and the directories it is told about.
		if root == nil || root.Access != ReadOnly {
			return nil, fmt.Errorf("%w: sandbox %q reads the whole filesystem and writes only named directories; grant %q %s, or confine it", ErrUnsupported, SandboxClaude, "/", ReadOnly)
		}
		if !writable(turn.Mounts, workDir(req)) {
			return nil, fmt.Errorf("%w: sandbox %q makes the working directory writable; grant it %s", ErrUnsupported, SandboxClaude, ReadWrite)
		}
	}
	var addDirs []string
	for _, d := range h.AddDirs {
		resolved, err := resolve(d)
		if err != nil {
			return nil, fmt.Errorf("add dir %s: %w", d, err)
		}
		if !writable(turn.Mounts, resolved) {
			return nil, fmt.Errorf("%w: directory %s is writable to the agent but not granted %s", ErrNotGranted, d, ReadWrite)
		}
		addDirs = append(addDirs, d)
	}
	if p.Sandbox == SandboxClaude {
		// Every writable grant is one the box is told about.
		for _, m := range turn.Mounts {
			if m.Access == ReadWrite && m.Path != workDir(req) && !slices.Contains(addDirs, m.Path) {
				addDirs = append(addDirs, m.Path)
			}
		}
	}
	turn.WriteDirs = addDirs
	turn.Env = h.env(req, turn)
	if !turn.VCS {
		turn.DeniedExecutables = slices.Clone(VCSExecutables)
	}
	if p.Confine {
		if turn.Confinement, err = h.confinement(req, turn); err != nil {
			return nil, err
		}
	}
	return turn, nil
}

// env builds the host environment from the allowlist alone.
func (h HostBoundary) env(req Request, turn *Turn) []string {
	environ := h.Environ
	if environ == nil {
		environ = os.Environ
	}
	allow := req.Grants.Env
	set := map[string]string{}
	var order []string
	add := func(k, v string) {
		if _, ok := set[k]; !ok {
			order = append(order, k)
		}
		set[k] = v
	}
	for _, kv := range environ() {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || (h.StripPrefix != "" && strings.HasPrefix(k, h.StripPrefix)) {
			continue
		}
		if envGranted(allow, k) && (turn.VCS || !isVCSEnv(k)) {
			add(k, v)
		}
	}
	for _, v := range sessionVars(req, turn.VCS) {
		add(v.name, v.value)
	}
	env := make([]string, 0, len(order))
	for _, k := range order {
		env = append(env, k+"="+set[k])
	}
	return env
}

// verifyCommon checks grants that every boundary enforces the same way.
func verifyCommon(req Request) (*Turn, error) {
	g := req.Grants
	if g == nil {
		return nil, ErrNoGrants
	}
	p := req.Profile
	if p.VCSAccess && !g.VCS {
		return nil, fmt.Errorf("%w: profile asks for VCS access", ErrNotGranted)
	}
	turn := &Turn{VCS: p.VCSAccess && g.VCS}
	if err := verifyDagger(p, g); err != nil {
		return nil, err
	}
	if p.Dagger != nil {
		turn.DaggerEngine = g.DaggerEngine
	}

	if err := checkEnvGrants(g); err != nil {
		return nil, err
	}
	for _, v := range sessionVars(req, turn.VCS) {
		if !envGranted(g.Env, v.name) {
			return nil, fmt.Errorf("%w: variable %s is set for the session but not in the environment allowlist", ErrNotGranted, v.name)
		}
	}

	tools, servers, err := splitTools(g.Tools)
	if err != nil {
		return nil, err
	}
	for name := range p.MCP {
		if !servers[name] {
			return nil, fmt.Errorf("%w: MCP server %q", ErrNotGranted, name)
		}
		for _, e := range p.MCP[name].EnvVars {
			if !envGranted(g.Env, e) {
				return nil, fmt.Errorf("%w: MCP server %q forwards variable %s, which is not in the environment allowlist", ErrNotGranted, name, e)
			}
		}
	}
	if req.HostMCP != nil && !servers[req.HostMCP.Name] {
		return nil, fmt.Errorf("%w: MCP server %q", ErrNotGranted, req.HostMCP.Name)
	}
	all := slices.Contains(tools, ToolsAll)
	for _, t := range p.AllowedTools {
		base, _, _ := strings.Cut(t, "(")
		if server, ok := mcpServer(base); ok {
			if !servers[server] {
				return nil, fmt.Errorf("%w: allowed tool %q names MCP server %q", ErrNotGranted, t, server)
			}
			continue
		}
		if !all && !slices.Contains(tools, base) {
			return nil, fmt.Errorf("%w: allowed tool %q", ErrNotGranted, t)
		}
	}
	if !all {
		if p.Agent != "" && p.Agent != AgentClaude {
			return nil, fmt.Errorf("%w: agent %q cannot restrict its built-in tools; grant %q", ErrUnsupported, p.Agent, ToolsAll)
		}
		turn.Tools = tools
		if turn.Tools == nil {
			turn.Tools = []string{}
		}
	}

	if turn.Mounts, err = resolveMounts(g, turn.VCS); err != nil {
		return nil, err
	}
	dir := req.workDir()
	if dir == "" {
		return nil, fmt.Errorf("%w: request has no working directory", ErrNotGranted)
	}
	resolved, err := resolve(dir)
	if err != nil {
		return nil, fmt.Errorf("working directory: %w", err)
	}
	if findMount(turn.Mounts, resolved) == nil {
		return nil, fmt.Errorf("%w: working directory %s is outside every mount", ErrNotGranted, dir)
	}
	return turn, nil
}

// verifyDagger checks the Dagger engine against its grant: given only in
// SandboxSbx, only to a profile that asks for it, and only the engine
// granted. The engine widens what a session reaches, so it is refused
// rather than handed to a session that did not ask.
func verifyDagger(p Profile, g *Grants) error {
	switch {
	case p.Dagger == nil && g.DaggerEngine != "":
		return fmt.Errorf("%w: the Dagger engine %s is granted and the profile does not ask for it", ErrUnsupported, g.DaggerEngine)
	case p.Dagger == nil:
		return nil
	case p.Sandbox != SandboxSbx:
		return fmt.Errorf("%w: sandbox %q cannot give a session the Dagger engine; only %q can", ErrUnsupported, p.Sandbox, SandboxSbx)
	case g.DaggerEngine != p.Dagger.Engine:
		return fmt.Errorf("%w: profile asks for the Dagger engine %s", ErrNotGranted, p.Dagger.Engine)
	}
	return nil
}

// checkEnvGrants checks the environment allowlist on its own.
func checkEnvGrants(g *Grants) error {
	for _, name := range g.Env {
		if err := checkEnvPattern(name); err != nil {
			return err
		}
		if !g.VCS && overlapsVCSEnv(name) {
			return fmt.Errorf("%w: variable %s carries VCS credentials or configuration and VCS is not granted", ErrNotGranted, name)
		}
	}
	return nil
}

// resolveMounts checks the mounts on their own and returns them with their
// symbolic links resolved, in the order they were granted. vcs is whether
// the turn has version control.
func resolveMounts(g *Grants, vcs bool) ([]Mount, error) {
	var within string
	var err error
	if g.Within != "" {
		if within, err = resolve(g.Within); err != nil {
			return nil, fmt.Errorf("within: %w", err)
		}
	}
	var mounts []Mount
	for _, m := range g.Mounts {
		if m.Access != ReadOnly && m.Access != ReadWrite {
			return nil, fmt.Errorf("mount %s: unknown access %q (want %s or %s)", m.Path, m.Access, ReadOnly, ReadWrite)
		}
		resolved, err := resolve(m.Path)
		if err != nil {
			return nil, fmt.Errorf("mount %s: %w", m.Path, err)
		}
		if within != "" && !inside(within, resolved) {
			return nil, fmt.Errorf("%w: mount %s resolves to %s, outside %s", ErrNotGranted, m.Path, resolved, g.Within)
		}
		if m.Access == ReadWrite && !vcs {
			if meta, ok := vcsMetadata(resolved); ok {
				return nil, fmt.Errorf("%w: mount %s is writable and holds VCS metadata %s", ErrNotGranted, m.Path, meta)
			}
		}
		mounts = append(mounts, Mount{Path: resolved, Access: m.Access})
	}
	return mounts, nil
}

// sessionVars are the variables the request sets, in the order they are set.
func sessionVars(req Request, vcs bool) []envVar {
	var vars []envVar
	for _, k := range slices.Sorted(maps.Keys(req.Profile.Env)) {
		vars = append(vars, envVar{k, os.ExpandEnv(req.Profile.Env[k])})
	}
	if req.Profile.Shell != "" {
		vars = append(vars, envVar{"SHELL", req.Profile.Shell})
	}
	for _, k := range slices.Sorted(maps.Keys(req.Env)) {
		vars = append(vars, envVar{k, req.Env[k]})
	}
	if vcs {
		for _, k := range slices.Sorted(maps.Keys(req.VCSEnv)) {
			vars = append(vars, envVar{k, req.VCSEnv[k]})
		}
	}
	return vars
}

func checkEnvPattern(name string) error {
	stem := strings.TrimSuffix(name, "*")
	if stem == "" {
		return fmt.Errorf("%w: environment allowlist entry %q is not a name or a prefix", ErrNotGranted, name)
	}
	for _, r := range stem {
		if r != '_' && (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			return fmt.Errorf("environment allowlist entry %q: invalid character %q", name, r)
		}
	}
	return nil
}

func envGranted(allow []string, name string) bool {
	for _, a := range allow {
		if a == name || (strings.HasSuffix(a, "*") && strings.HasPrefix(name, strings.TrimSuffix(a, "*"))) {
			return true
		}
	}
	return false
}

func isVCSEnv(name string) bool { return envGranted(vcsEnv, name) }

// overlapsVCSEnv reports whether an allowlist entry could grant a VCS variable.
func overlapsVCSEnv(entry string) bool {
	stem, pattern := strings.CutSuffix(entry, "*")
	for _, d := range vcsEnv {
		dstem, dpattern := strings.CutSuffix(d, "*")
		switch {
		case pattern && dpattern:
			if strings.HasPrefix(stem, dstem) || strings.HasPrefix(dstem, stem) {
				return true
			}
		case pattern:
			if strings.HasPrefix(dstem, stem) {
				return true
			}
		case dpattern:
			if strings.HasPrefix(stem, dstem) {
				return true
			}
		default:
			if stem == dstem {
				return true
			}
		}
	}
	return false
}

// splitTools separates built-in tool grants from MCP server grants.
func splitTools(grants []string) ([]string, map[string]bool, error) {
	var tools []string
	servers := map[string]bool{}
	for _, t := range grants {
		if t == "" || strings.ContainsAny(t, "(), ") {
			return nil, nil, fmt.Errorf("tool grant %q is not a tool name", t)
		}
		if server, ok := mcpServer(t); ok {
			if strings.Contains(strings.TrimPrefix(t, "mcp__"), "__") {
				return nil, nil, fmt.Errorf("tool grant %q: grant the MCP server %q, not one of its tools", t, "mcp__"+server)
			}
			servers[server] = true
			continue
		}
		tools = append(tools, t)
	}
	return tools, servers, nil
}

// mcpServer returns the server an "mcp__<server>[__<tool>]" name refers to.
func mcpServer(tool string) (string, bool) {
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok || rest == "" {
		return "", false
	}
	server, _, _ := strings.Cut(rest, "__")
	return server, server != ""
}

// resolve requires an absolute, clean, existing path and resolves its links.
func resolve(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("%w: path %q is not absolute", ErrNotGranted, path)
	}
	if filepath.Clean(path) != path && filepath.Clean(path)+string(filepath.Separator) != path {
		return "", fmt.Errorf("%w: path %q is not clean", ErrNotGranted, path)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func inside(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// findMount returns the innermost mount that covers path.
func findMount(mounts []Mount, path string) *Mount {
	var best *Mount
	for i, m := range mounts {
		if inside(m.Path, path) && (best == nil || len(m.Path) > len(best.Path)) {
			best = &mounts[i]
		}
	}
	return best
}

func writable(mounts []Mount, path string) bool {
	if path == "" {
		return false
	}
	m := findMount(mounts, path)
	return m != nil && m.Access == ReadWrite
}

// vcsMetadata reports VCS metadata a writable path is inside of or holds at
// its top: a ".git" directory or file, or another VCS's metadata directory.
func vcsMetadata(path string) (string, bool) {
	for p := path; ; p = filepath.Dir(p) {
		if slices.Contains(vcsDirs, filepath.Base(p)) {
			return p, true
		}
		if filepath.Dir(p) == p {
			break
		}
	}
	for _, d := range vcsDirs {
		meta := filepath.Join(path, d)
		if _, err := os.Lstat(meta); err == nil {
			return meta, true
		}
	}
	return "", false
}

var vcsDirs = []string{".git", ".hg", ".jj", ".svn"}

func workDir(req Request) string {
	dir, err := resolve(req.workDir())
	if err != nil {
		return ""
	}
	return dir
}

// boundary selects what enforces the request's grants.
func (r *Runner) boundary(req Request) Boundary {
	if req.Profile.Sandbox == SandboxSbx {
		b := SandboxBoundary{SessionsDir: r.SessionsDir, MountDirs: r.MountDirs}
		if r.Skills != nil {
			b.SkillMountDirs = r.SkillMountDirs
		}
		return b
	}
	if req.Profile.Sandbox == SandboxContainer {
		b := ContainerBoundary{Home: r.containerHome(), SessionsDir: r.SessionsDir, MountDirs: r.MountDirs}
		if r.held != nil {
			b.Masks = r.held.masks
		}
		if r.Skills != nil {
			b.SkillMountDirs = r.SkillMountDirs
		}
		return b
	}
	b := HostBoundary{StripPrefix: r.EnvironmentPrefix, AddDirs: r.AddDirs, Confiner: r.Confiner, SystemPaths: r.SystemPaths, SessionsDir: r.SessionsDir}
	if r.Skills != nil {
		b.SkillDirs = r.SkillMountDirs
	}
	return b
}

// Verify checks a request against its grants without starting anything. The
// runner of a Session also refuses a turn that is not the one the session's
// policy describes.
func (r *Runner) Verify(req Request) (*Turn, error) {
	turn, err := r.boundary(req).Verify(req)
	if err != nil {
		return nil, err
	}
	if r.held != nil {
		if err := r.held.admit(turn); err != nil {
			return nil, err
		}
	}
	return turn, nil
}

// deniedBinDir is the session subdirectory holding denied executables' stand-ins.
const deniedBinDir = "denied-bin"

// denyExecutables writes stand-ins for the denied executables into dir and
// returns the PATH that puts them first. They keep a session from using a
// VCS executable by name; the box keeps VCS metadata unwritable.
func denyExecutables(dir string, names []string, env []string) ([]string, error) {
	if len(names) == 0 {
		return env, nil
	}
	if err := writeDenied(dir, names); err != nil {
		return nil, err
	}
	path := dir
	out := make([]string, 0, len(env)+1)
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			if v != "" {
				path += string(os.PathListSeparator) + v
			}
			continue
		}
		out = append(out, kv)
	}
	return append(out, "PATH="+path), nil
}

// writeDenied writes a stand-in for each denied executable into dir.
func writeDenied(dir string, names []string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, n := range names {
		script := "#!/bin/sh\necho \"" + n + ": not granted to this session\" >&2\nexit 126\n"
		if err := os.WriteFile(filepath.Join(dir, n), []byte(script), 0o755); err != nil {
			return err
		}
	}
	return nil
}
