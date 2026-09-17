package agent

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// DefaultSystemPaths are what a confined turn reaches beyond its mounts when
// the boundary names nothing else: the programs and libraries of the system,
// the configuration a program needs to resolve a name, verify a certificate
// and find its libraries, the process and CPU information a runtime reads,
// and the devices every program expects. No home directory, no temporary
// directory and nothing of /var or /run: a turn that needs one is granted it.
// A path this machine does not have is skipped.
func DefaultSystemPaths() []Mount {
	var paths []Mount
	for _, p := range []string{
		"/usr", "/bin", "/sbin", "/lib", "/lib32", "/lib64", "/libx32",
		"/etc/ld.so.cache", "/etc/ld.so.conf", "/etc/ld.so.conf.d",
		"/etc/ssl", "/etc/ca-certificates", "/etc/pki",
		"/etc/resolv.conf", "/etc/hosts", "/etc/nsswitch.conf", "/etc/host.conf",
		"/etc/passwd", "/etc/group", "/etc/localtime",
		"/proc", "/sys/devices/system/cpu", "/sys/fs/cgroup",
	} {
		paths = append(paths, Mount{Path: p, Access: ReadOnly})
	}
	for _, p := range []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/random", "/dev/urandom", "/dev/tty"} {
		paths = append(paths, Mount{Path: p, Access: ReadWrite})
	}
	return paths
}

// platformConfiner is Landlock on Linux.
func platformConfiner() Confiner { return landlockConfiner{abi: landlockABI} }

// landlockMinABI is the first Landlock version that can refuse truncate(2):
// before it a file granted read-only could still be emptied.
const landlockMinABI = 3

const (
	landlockFile = unix.LANDLOCK_ACCESS_FS_EXECUTE | unix.LANDLOCK_ACCESS_FS_READ_FILE
	landlockRead = landlockFile | unix.LANDLOCK_ACCESS_FS_READ_DIR
	// A writable path may hold anything but a device node.
	landlockWriteFile = unix.LANDLOCK_ACCESS_FS_WRITE_FILE | unix.LANDLOCK_ACCESS_FS_TRUNCATE
	landlockWrite     = landlockWriteFile | unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_SYM | unix.LANDLOCK_ACCESS_FS_REFER
	// Everything version 3 can refuse. What a rule does not allow of it is
	// refused everywhere.
	landlockHandled = landlockRead | landlockWrite | unix.LANDLOCK_ACCESS_FS_MAKE_CHAR | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK
)

// landlockConfiner holds a process to its confinement with Landlock: the
// rules name what the process may reach, and the kernel refuses the rest for
// the process and everything it starts, with no way to lift it.
type landlockConfiner struct {
	// abi reports the kernel's Landlock version.
	abi func() (int, error)
}

// landlockABI asks the kernel for its Landlock version.
func landlockABI() (int, error) {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, unix.LANDLOCK_CREATE_RULESET_VERSION)
	if errno != 0 {
		return 0, errno
	}
	return int(v), nil
}

func (l landlockConfiner) Check(c Confinement) error {
	v, err := l.abi()
	if err != nil {
		return fmt.Errorf("%w: this kernel has no Landlock to confine a host session with (landlock_create_ruleset: %v)", ErrUnsupported, err)
	}
	if v < landlockMinABI {
		return fmt.Errorf("%w: this kernel's Landlock is version %d, and before version %d it cannot keep a read-only file from being truncated", ErrUnsupported, v, landlockMinABI)
	}
	if c.Sandbox == SandboxClaude {
		// Claude's box is bubblewrap on Linux, which builds itself with
		// mount(2), and a Landlock domain refuses every mount.
		return fmt.Errorf("%w: sandbox %q cannot start under Landlock, which refuses the mounts its box is built with; confine sandbox %q", ErrUnsupported, SandboxClaude, SandboxNone)
	}
	return nil
}

// Start starts cmd from a thread that has restricted itself. Landlock
// restricts the calling thread and what it forks, so the thread is locked to
// one goroutine, restricted, used for the fork and never unlocked: the
// runtime destroys it when the goroutine ends, and no other goroutine ever
// runs under the restriction.
func (l landlockConfiner) Start(cmd *exec.Cmd, c Confinement) error {
	if err := l.Check(c); err != nil {
		return err
	}
	rules, err := confineRules(c)
	if err != nil {
		return fmt.Errorf("confine: %w", err)
	}
	v, _ := l.abi()
	ruleset, err := landlockRuleset(rules, v)
	if err != nil {
		return fmt.Errorf("confine: %w", err)
	}
	defer func() { _ = unix.Close(ruleset) }()

	started := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
			started <- fmt.Errorf("confine: no_new_privs: %w", err)
			return
		}
		if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, uintptr(ruleset), 0, 0); errno != 0 {
			started <- fmt.Errorf("confine: landlock_restrict_self: %w", errno)
			return
		}
		started <- cmd.Start()
	}()
	return <-started
}

// landlockRuleset builds the ruleset the rules describe and returns its
// descriptor.
func landlockRuleset(rules []confineRule, abi int) (int, error) {
	handled := uint64(landlockHandled)
	device := uint64(0)
	if abi >= 5 {
		// Version 5 can refuse ioctl(2) on a device; a granted device
		// (/dev/tty) keeps it.
		device = unix.LANDLOCK_ACCESS_FS_IOCTL_DEV
		handled |= device
	}
	attr := unix.LandlockRulesetAttr{Access_fs: handled}
	// Only the first field is passed: every kernel with Landlock knows it.
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr.Access_fs), 0)
	if errno != 0 {
		return -1, fmt.Errorf("landlock_create_ruleset: %w", errno)
	}
	ruleset := int(fd)
	for _, r := range rules {
		if err := landlockAllow(ruleset, r, device); err != nil {
			_ = unix.Close(ruleset)
			return -1, err
		}
	}
	return ruleset, nil
}

func landlockAllow(ruleset int, r confineRule, device uint64) error {
	var access uint64
	switch {
	case r.Dir && r.Write:
		access = landlockRead | landlockWrite | device
	case r.Dir && r.Read:
		access = landlockRead
	case r.Dir:
		access = unix.LANDLOCK_ACCESS_FS_READ_DIR
	case r.Write:
		// A rule on a file takes only what applies to a file.
		access = landlockFile | landlockWriteFile | device
	case r.Read:
		access = landlockFile
	default:
		return nil
	}
	// The plan names no symbolic link, and none is followed here.
	fd, err := unix.Open(r.Path, unix.O_PATH|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil // gone since the plan was made: nothing to allow
	}
	if err != nil {
		return fmt.Errorf("open %s: %w", r.Path, err)
	}
	defer func() { _ = unix.Close(fd) }()
	rule := unix.LandlockPathBeneathAttr{Allowed_access: access, Parent_fd: int32(fd)}
	if _, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, uintptr(ruleset), unix.LANDLOCK_RULE_PATH_BENEATH, uintptr(unsafe.Pointer(&rule)), 0, 0, 0); errno != 0 {
		return fmt.Errorf("landlock_add_rule %s: %w", r.Path, errno)
	}
	return nil
}
