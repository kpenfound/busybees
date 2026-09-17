package agent

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
)

// The operations a Seatbelt profile confines. Everything else (network,
// Mach services, signals) is left as the process would have it: a
// Confinement is about the filesystem.
const (
	seatbeltRead  = "file-read*"
	seatbeltExec  = "process-exec*"
	seatbeltWrite = "file-write* file-link file-clone"
)

// seatbeltConfiner holds a process to its confinement with Seatbelt:
// sandbox-exec applies the profile to itself and then executes the command,
// so the command and everything it starts run under the profile, with no
// way to lift it. When the profile cannot be applied, sandbox-exec exits
// with an error and the command never runs.
type seatbeltConfiner struct {
	// bin is sandbox-exec.
	bin string
}

func (s seatbeltConfiner) Check(Confinement) error {
	info, err := os.Stat(s.bin)
	if err != nil {
		return fmt.Errorf("%w: a confined host session needs %s to apply its Seatbelt profile: %v", ErrUnsupported, s.bin, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%w: %s, which applies a confined host session's Seatbelt profile, is not an executable file", ErrUnsupported, s.bin)
	}
	return nil
}

// Start starts cmd as sandbox-exec's command, under the profile c becomes.
func (s seatbeltConfiner) Start(cmd *exec.Cmd, c Confinement) error {
	if err := s.Check(c); err != nil {
		return err
	}
	profile, err := seatbeltProfile(c)
	if err != nil {
		return err
	}
	args := append([]string{s.bin, "-p", profile, cmd.Path}, cmd.Args[1:]...)
	cmd.Path, cmd.Args, cmd.Err = s.bin, args, nil
	return cmd.Start()
}

// seatbeltProfile turns a confinement into a Seatbelt (SBPL) profile, the
// text sandbox-exec applies on macOS. In a Seatbelt profile the last rule
// that matches decides, so the profile allows everything, refuses the
// filesystem, and then allows it back path by path:
//
//   - a file's metadata everywhere, as Landlock leaves stat(2) alone;
//   - the entries of the root directory, which dyld reads before any
//     program starts (without it every program aborts), and nothing below;
//   - read and execute below every mount and system path;
//   - write below the read-write mounts and system paths, a mount inside
//     another deciding for itself, outermost first;
//   - nothing at all on a denied path, or on a hard link to a denied file
//     in a directory that holds a denied path, whatever allowed it before.
//
// Seatbelt matches the path a symbolic link resolves to, and a
// Confinement's paths are resolved already.
func seatbeltProfile(c Confinement) (string, error) {
	var b strings.Builder
	rule := func(verb, ops string, paths []string) {
		if len(paths) == 0 {
			return
		}
		fmt.Fprintf(&b, "(%s %s", verb, ops)
		for _, p := range paths {
			fmt.Fprintf(&b, " (subpath %s)", sbplString(p))
		}
		b.WriteString(")\n")
	}
	b.WriteString("(version 1)\n(allow default)\n")
	fmt.Fprintf(&b, "(deny %s %s %s)\n", seatbeltRead, seatbeltWrite, seatbeltExec)
	b.WriteString("(allow file-read-metadata)\n(allow file-read-data (literal \"/\"))\n")

	var readable []string
	for _, m := range append(slices.Clone(c.Mounts), c.System...) {
		if !slices.Contains(readable, m.Path) {
			readable = append(readable, m.Path)
		}
	}
	rule("allow", seatbeltRead+" "+seatbeltExec, readable)

	// Outermost first, so the inner mount's rule comes later and decides.
	mounts := slices.Clone(c.Mounts)
	slices.SortStableFunc(mounts, func(a, b Mount) int {
		return strings.Count(a.Path, string(filepath.Separator)) - strings.Count(b.Path, string(filepath.Separator))
	})
	for _, m := range mounts {
		verb := "deny"
		if m.Access == ReadWrite {
			verb = "allow"
		}
		rule(verb, seatbeltWrite, []string{m.Path})
	}
	var writable []string
	for _, m := range c.System {
		if m.Access == ReadWrite {
			writable = append(writable, m.Path)
		}
	}
	rule("allow", seatbeltWrite, writable)

	denied, err := deniedLinks(c)
	if err != nil {
		return "", err
	}
	rule("deny", seatbeltRead+" "+seatbeltWrite+" "+seatbeltExec, denied)
	return b.String(), nil
}

// deniedLinks returns the denied paths, followed by every other name a
// denied file has in a directory that holds a denied path, up to the mount
// or system path that allows the directory. Seatbelt matches names, and a
// hard link is a name of its own.
func deniedLinks(c Confinement) ([]string, error) {
	denied := slices.Clone(c.Denied)
	var files []os.FileInfo
	for _, d := range c.Denied {
		if info, err := os.Stat(d); err == nil && info.Mode().IsRegular() {
			files = append(files, info)
		}
	}
	if len(files) == 0 {
		return denied, nil
	}
	allowed := append(slices.Clone(c.Mounts), c.System...)
	var dirs []string
	for _, d := range c.Denied {
		for dir := filepath.Dir(d); ; dir = filepath.Dir(dir) {
			if findMount(allowed, dir) != nil && !slices.Contains(dirs, dir) {
				dirs = append(dirs, dir)
			}
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	for _, dir := range dirs {
		entries, err := os.ReadDir(dir)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("confine: %w", err)
		}
		for _, e := range entries {
			if !e.Type().IsRegular() {
				continue
			}
			p := filepath.Join(dir, e.Name())
			info, err := os.Lstat(p)
			if err != nil {
				continue // gone since the directory was read
			}
			if slices.ContainsFunc(files, func(f os.FileInfo) bool { return os.SameFile(info, f) }) && !slices.Contains(denied, p) {
				denied = append(denied, p)
			}
		}
	}
	return denied, nil
}

// sbplString quotes a path as an SBPL string literal.
func sbplString(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
