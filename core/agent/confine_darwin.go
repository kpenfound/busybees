package agent

// DefaultSystemPaths are what a confined turn reaches beyond its mounts when
// the boundary names nothing else: the programs, libraries and frameworks of
// the system, the configuration a program needs to resolve a name, verify a
// certificate and know the time, and the devices every program expects. No
// home directory, no temporary directory, nothing of /private/var beyond the
// time zone data, and nothing of /opt or /Library: a turn that needs one is
// granted it. A path this machine does not have is skipped.
func DefaultSystemPaths() []Mount {
	var paths []Mount
	for _, p := range []string{
		"/usr", "/bin", "/sbin", "/System",
		"/etc/ssl", "/etc/hosts", "/etc/resolv.conf", "/etc/services", "/etc/protocols",
		"/etc/passwd", "/etc/group", "/etc/localtime", "/var/db/timezone",
	} {
		paths = append(paths, Mount{Path: p, Access: ReadOnly})
	}
	for _, p := range []string{"/dev/null", "/dev/zero", "/dev/random", "/dev/urandom", "/dev/tty", "/dev/dtracehelper"} {
		paths = append(paths, Mount{Path: p, Access: ReadWrite})
	}
	return paths
}

// platformConfiner is Seatbelt on macOS.
func platformConfiner() Confiner { return seatbeltConfiner{bin: sandboxExec} }

// sandboxExec is the program that applies a Seatbelt profile. It is named by
// its path and never searched for on PATH.
const sandboxExec = "/usr/bin/sandbox-exec"
