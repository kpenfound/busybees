package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// Confinement is what the operating system enforces for a confined host
// turn. Every path has its symbolic links resolved. Nothing outside Mounts
// and System can be read, written or executed.
type Confinement struct {
	// Sandbox is the profile's host sandbox, SandboxNone or SandboxClaude.
	Sandbox string
	// Mounts are the granted mounts. Where one lies inside another the
	// inner one decides, so a read-only mount inside a read-write one stays
	// read-only.
	Mounts []Mount
	// System are the system paths and, once the runner has resolved it, the
	// agent's executable. They add to what the mounts allow.
	System []Mount
	// Denied are the files and directories that stay unreadable and
	// unexecutable inside Mounts and System: the VCS executables of a turn
	// without VCS. A denied file is denied at its own path, whatever shell
	// or symbolic link leads there, and under a hard link in a directory
	// that holds a denied path. A hard link to it anywhere else in Mounts or
	// System is not found.
	Denied []string
}

// Confiner enforces a Confinement with the operating system.
type Confiner interface {
	// Check reports whether this platform can enforce c, without starting
	// anything. Its error wraps ErrUnsupported.
	Check(c Confinement) error
	// Start starts cmd under c. It never starts cmd with less than c.
	Start(cmd *exec.Cmd, c Confinement) error
}

// confinement checks the confined contract and builds what the confiner is
// handed. The grants alone decide what the turn reaches: no "/" is asked for
// and none is implied.
func (h HostBoundary) confinement(req Request, turn *Turn) (*Confinement, error) {
	c := &Confinement{Sandbox: req.Profile.Sandbox, Mounts: slices.Clone(turn.Mounts)}
	if c.Sandbox == "" {
		c.Sandbox = SandboxNone
	}
	system := h.SystemPaths
	if system == nil {
		system = DefaultSystemPaths()
	}
	for _, m := range system {
		if m.Access != ReadOnly && m.Access != ReadWrite {
			return nil, fmt.Errorf("system path %s: unknown access %q (want %s or %s)", m.Path, m.Access, ReadOnly, ReadWrite)
		}
		// A system path this machine does not have is not an error: the
		// list names what a distribution may have.
		if resolved, err := filepath.EvalSymlinks(m.Path); err == nil {
			c.System = append(c.System, Mount{Path: resolved, Access: m.Access})
		}
	}
	// The files the runner writes for the agent must be ones it can read.
	switch {
	case req.SessionDir != "":
		if err := covered(turn.Mounts, "session directory", req.SessionDir, resolve); err != nil {
			return nil, err
		}
	case h.SessionsDir != "":
		// Not created yet: the runner verifies again once it is.
		if err := covered(turn.Mounts, "sessions directory", h.SessionsDir, resolveCreatable); err != nil {
			return nil, err
		}
	}
	if len(req.Profile.Skills) > 0 {
		for _, dir := range h.SkillDirs {
			if err := covered(turn.Mounts, "skill directory", dir, resolve); err != nil {
				return nil, err
			}
		}
	}
	if !turn.VCS {
		c.Denied = executablePaths(VCSExecutables, envValue(turn.Env, "PATH"))
		for i, m := range turn.Mounts {
			info, err := os.Stat(m.Path)
			if err != nil {
				return nil, fmt.Errorf("mount %s: %w", req.Grants.Mounts[i].Path, err)
			}
			for _, d := range c.Denied {
				// By its path, or as a hard link to it under another name.
				denied, err := os.Stat(d)
				if inside(d, m.Path) || (err == nil && os.SameFile(info, denied)) {
					return nil, fmt.Errorf("%w: mount %s is the VCS executable %s and VCS is not granted", ErrNotGranted, req.Grants.Mounts[i].Path, d)
				}
			}
		}
	}
	confiner := h.Confiner
	if confiner == nil {
		confiner = platformConfiner()
	}
	if err := confiner.Check(*c); err != nil {
		return nil, err
	}
	return c, nil
}

// covered requires a path the runner owns to lie inside a mount.
func covered(mounts []Mount, what, path string, resolveFn func(string) (string, error)) error {
	real, err := resolveFn(path)
	if err != nil {
		return fmt.Errorf("%s: %w", what, err)
	}
	if findMount(mounts, real) == nil {
		return fmt.Errorf("%w: %s %s is outside every mount, and a confined turn reads nothing else", ErrNotGranted, what, path)
	}
	return nil
}

// withExecutable adds the agent's executable to what the turn may read and
// execute. Whatever else the executable loads from outside the system paths
// is the caller's to grant.
func (c Confinement) withExecutable(bin string) (Confinement, error) {
	real, err := filepath.EvalSymlinks(bin)
	if err != nil {
		return c, err
	}
	c.System = append(slices.Clone(c.System), Mount{Path: real, Access: ReadOnly})
	return c, nil
}

func envValue(env []string, name string) string {
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, name+"="); ok {
			return v
		}
	}
	return ""
}

// binDirs are searched for denied executables beside the turn's PATH: an
// absolute path reaches them whatever PATH says.
var binDirs = []string{
	"/usr/local/sbin", "/usr/local/bin", "/usr/sbin", "/usr/bin", "/sbin", "/bin",
	"/opt/homebrew/bin", "/home/linuxbrew/.linuxbrew/bin", "/snap/bin",
}

// gitExecDirs are where git keeps the programs behind its subcommands,
// relative to the directory above the one that holds git.
var gitExecDirs = []string{"libexec/git-core", "lib/git-core"}

// executablePaths finds the named executables in the directories of path and
// in binDirs, with their symbolic links resolved: the files a name runs, not
// the names. For git it adds the directory of its subcommand programs, each
// of which is git again.
func executablePaths(names []string, path string) []string {
	var found []string
	add := func(p string) string {
		real, err := filepath.EvalSymlinks(p)
		if err == nil && !slices.Contains(found, real) {
			found = append(found, real)
		}
		return real
	}
	dirs := append(filepath.SplitList(path), binDirs...)
	for _, dir := range dirs {
		if !filepath.IsAbs(dir) {
			continue
		}
		for _, name := range names {
			p := filepath.Join(dir, name)
			info, err := os.Stat(p)
			if err != nil || info.IsDir() {
				continue
			}
			real := add(p)
			if name != "git" || real == "" {
				continue
			}
			for _, from := range []string{p, real} {
				for _, rel := range gitExecDirs {
					dir := filepath.Join(filepath.Dir(filepath.Dir(from)), rel)
					if info, err := os.Stat(dir); err == nil && info.IsDir() {
						add(dir)
					}
				}
			}
		}
	}
	return found
}

// confineRule allows one path, and everything below it when it is a
// directory. A rule that is neither Write nor Read allows listing
// directories only.
type confineRule struct {
	Path  string
	Dir   bool
	Read  bool
	Write bool
}

// confineRules turns a confinement into rules for a mechanism that can only
// allow, never deny, below a path it has allowed (Landlock). A directory
// that holds something with less access than itself, a denied path or a
// read-only mount inside a read-write one, is not allowed as a whole: its
// entries are allowed one by one around the exception, and the directory
// itself can be listed and nothing more, so nothing can be created at its
// own level. Symbolic links are left out, since following one needs no
// access and its target is decided where the target lies. So is an entry
// that is a denied file under another name. Only a directory gone around is
// read entry by entry: a hard link to a denied file in a directory allowed
// as a whole is allowed with it.
func confineRules(c Confinement) ([]confineRule, error) {
	p := &rulePlan{denied: c.Denied, index: map[string]int{}}
	for _, d := range c.Denied {
		if info, err := os.Stat(d); err == nil {
			p.deniedInfo = append(p.deniedInfo, info)
		}
	}
	for _, m := range c.Mounts {
		var except []string
		for _, inner := range c.Mounts {
			if inner.Path != m.Path && inside(m.Path, inner.Path) && m.Access == ReadWrite && inner.Access != ReadWrite {
				except = append(except, inner.Path)
			}
		}
		if err := p.allow(m.Path, m.Access == ReadWrite, except); err != nil {
			return nil, err
		}
	}
	for _, m := range c.System {
		if err := p.allow(m.Path, m.Access == ReadWrite, nil); err != nil {
			return nil, err
		}
	}
	return p.rules, nil
}

type rulePlan struct {
	denied     []string
	deniedInfo []os.FileInfo
	rules      []confineRule
	index      map[string]int
}

// allow adds the rules for path. The paths in except are other mounts, which
// get rules of their own.
func (p *rulePlan) allow(path string, write bool, except []string) error {
	for _, d := range p.denied {
		if inside(d, path) {
			return nil
		}
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	for _, d := range p.deniedInfo {
		if os.SameFile(info, d) {
			return nil
		}
	}
	holes := slices.ContainsFunc(append(slices.Clone(except), p.denied...), func(e string) bool {
		return e != path && inside(path, e)
	})
	if !info.IsDir() || !holes {
		p.add(confineRule{Path: path, Dir: info.IsDir(), Read: true, Write: write})
		return nil
	}
	p.add(confineRule{Path: path, Dir: true})
	entries, err := os.ReadDir(path)
	if err != nil {
		return err
	}
	for _, e := range entries {
		child := filepath.Join(path, e.Name())
		if slices.Contains(except, child) {
			continue
		}
		// An entry gone since the directory was read has nothing to allow.
		if err := p.allow(child, write, except); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func (p *rulePlan) add(r confineRule) {
	if i, ok := p.index[r.Path]; ok {
		p.rules[i].Read = p.rules[i].Read || r.Read
		p.rules[i].Write = p.rules[i].Write || r.Write
		return
	}
	p.index[r.Path] = len(p.rules)
	p.rules = append(p.rules, r)
}
