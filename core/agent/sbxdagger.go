package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
)

// A SandboxSbx session whose profile asks for Dagger (Profile.Dagger) is
// given the Dagger CLI and the host's Dagger engine. The CLI is installed
// into the sandbox once it is created, by the Dagger install script at the
// profile's release, as root, into /usr/local/bin. The engine cannot be
// mounted: a sandbox is a virtual machine and sbx shares directories with
// it, not sockets. The session reaches it over TCP at host.docker.internal
// instead, which the sandbox's proxy turns into the host's localhost: an
// engine on a socket is forwarded from a port on the host's loopback that
// the runner listens on for this session alone, and an engine on a
// loopback TCP address is reached at that port. The Dagger CLI inside is
// pointed at it with EnvDaggerRunnerHost.

// EnvDaggerRunnerHost is the variable the Dagger CLI reads the address of
// an engine it does not start itself from.
const EnvDaggerRunnerHost = "_EXPERIMENTAL_DAGGER_RUNNER_HOST"

// DaggerInstallScript is the script that installs the Dagger CLI in the
// sandbox. The sandbox's network policy must allow its host.
const DaggerInstallScript = "https://dl.dagger.io/dagger/install.sh"

// startDagger installs the Dagger CLI in the sandbox and gives the session
// the engine: the address it reaches the engine at, in EnvDaggerRunnerHost.
func (s *sandbox) startDagger(ctx context.Context) error {
	d := s.req.Profile.Dagger
	if err := s.installDagger(ctx, d.Version); err != nil {
		return err
	}
	addr, err := s.daggerAddress(ctx, s.turn.DaggerEngine)
	if err != nil {
		return err
	}
	s.vars = append(s.vars, envVar{EnvDaggerRunnerHost, addr})
	return nil
}

// installDagger runs the install script inside the sandbox at one release.
// The release has passed CheckDaggerVersion, so it is safe on a command
// line.
func (s *sandbox) installDagger(ctx context.Context, version string) error {
	args := []string{"exec", s.name, "sudo", "env", "BIN_DIR=/usr/local/bin", "DAGGER_VERSION=" + strings.TrimPrefix(version, "v"),
		"sh", "-c", "curl -fsSL " + DaggerInstallScript + " | sh"}
	cmd := s.r.sbxCommand(ctx, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		if msg := bytes.TrimSpace(out); len(msg) > 0 {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		return fmt.Errorf("install the Dagger CLI %s in sandbox %s: %w", version, s.name, err)
	}
	return nil
}

// daggerAddress is the address the session reaches the engine at: a
// socket through a forward this session alone has, a TCP engine on the
// host's loopback at host.docker.internal, and any other TCP engine as it
// is.
func (s *sandbox) daggerAddress(ctx context.Context, engine string) (string, error) {
	scheme, rest, _ := strings.Cut(engine, "://")
	if scheme == "unix" {
		listen, err := s.r.sandboxListen(ctx)
		if err != nil {
			return "", err
		}
		host, _, err := net.SplitHostPort(listen)
		if err != nil {
			return "", fmt.Errorf("forward the Dagger engine: listen address %q: %w", listen, err)
		}
		f, err := forwardUnix(net.JoinHostPort(host, "0"), rest)
		if err != nil {
			return "", fmt.Errorf("forward the Dagger engine %s: %w", engine, err)
		}
		s.forward = f
		_, port, _ := net.SplitHostPort(f.addr())
		return "tcp://" + net.JoinHostPort(containerHostAlias, port), nil
	}
	host, port, err := net.SplitHostPort(rest)
	if err != nil {
		return "", fmt.Errorf("the Dagger engine %s: %w", engine, err)
	}
	if isLoopback(host) {
		host = containerHostAlias
	}
	return "tcp://" + net.JoinHostPort(host, port), nil
}

// isLoopback reports whether a host names this machine's loopback, which
// the sandbox reaches as host.docker.internal.
func isLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// forward accepts TCP connections and joins each one to a new connection
// to a Unix socket, until it is closed.
type forward struct {
	ln     net.Listener
	socket string
	mu     sync.Mutex
	conns  map[net.Conn]bool
	closed bool
	wg     sync.WaitGroup
}

// forwardUnix listens on listen and forwards every connection to socket.
func forwardUnix(listen, socket string) (*forward, error) {
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	f := &forward{ln: ln, socket: socket, conns: map[net.Conn]bool{}}
	f.wg.Add(1)
	go f.serve()
	return f, nil
}

func (f *forward) addr() string { return f.ln.Addr().String() }

func (f *forward) serve() {
	defer f.wg.Done()
	for {
		in, err := f.ln.Accept()
		if err != nil {
			return
		}
		out, err := net.Dial("unix", f.socket)
		if err != nil {
			_ = in.Close()
			continue
		}
		if !f.track(in, out) {
			return
		}
		f.wg.Add(2)
		go f.pipe(out, in)
		go f.pipe(in, out)
	}
}

// track records a pair of connections for close, or closes them when the
// forward has been closed already.
func (f *forward) track(conns ...net.Conn) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		for _, c := range conns {
			_ = c.Close()
		}
		return false
	}
	for _, c := range conns {
		f.conns[c] = true
	}
	return true
}

// pipe copies one direction, then closes both ends: either side hanging up
// ends the connection.
func (f *forward) pipe(dst, src net.Conn) {
	defer f.wg.Done()
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
}

// close stops accepting, closes every connection and waits for the
// goroutines.
func (f *forward) close() {
	f.mu.Lock()
	f.closed = true
	for c := range f.conns {
		_ = c.Close()
	}
	f.mu.Unlock()
	_ = f.ln.Close()
	f.wg.Wait()
}
