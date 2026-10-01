package agent

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/kpenfound/busybees/core/agent/agentbin"
)

// A SandboxSbx session whose profile asks for Dagger (Profile.Dagger) is
// given the Dagger CLI and the host's Dagger engine. Once the sandbox is
// created, the runner asks the CLI already in it for its release (`dagger
// version`): a template that has the profile's release keeps it, and any
// other sandbox, one without the CLI, with another release or whose probe
// failed, is given it by the Dagger install script, as root, into
// /usr/local/bin. The engine cannot be
// mounted: a sandbox is a virtual machine and sbx shares directories with
// it, not sockets. The session reaches it over TCP at host.docker.internal
// instead, which the sandbox's proxy turns into the host's localhost: an
// engine on a socket, or in a container that publishes no port
// (docker-container://<name>, reached as the Dagger CLI reaches it, by
// `docker exec -i <name> buildctl dial-stdio` per connection), is
// forwarded from a port on the host's loopback that the runner listens on
// for this session alone, and an engine on a loopback TCP address is
// reached at that port. Either port is allowed for
// this sandbox alone, as the caller-supplied server's is (allowHost). The
// Dagger CLI inside is pointed at it with EnvDaggerRunnerHost.

// EnvDaggerRunnerHost is the variable the Dagger CLI reads the address of
// an engine it does not start itself from.
const EnvDaggerRunnerHost = "_EXPERIMENTAL_DAGGER_RUNNER_HOST"

// DaggerInstallScript is the script that installs the Dagger CLI in the
// sandbox. The sandbox's network policy must allow its host.
const DaggerInstallScript = "https://dl.dagger.io/dagger/install.sh"

// daggerProbeTimeout bounds the probe for the Dagger CLI a sandbox already
// has; a probe that takes longer is a failed one, and the CLI is installed.
const daggerProbeTimeout = time.Minute

// startDagger installs the Dagger CLI in the sandbox, unless it has the
// profile's release already, and gives the session the engine: the address
// it reaches the engine at, in EnvDaggerRunnerHost, and, for an engine on
// the host's loopback, the network policy rule that lets the sandbox reach
// it.
func (s *sandbox) startDagger(ctx context.Context) error {
	d := s.req.Profile.Dagger
	if !s.hasDagger(ctx, d.Version) {
		if err := s.installDagger(ctx, d.Version); err != nil {
			return err
		}
	}
	addr, err := s.daggerAddress(s.turn.DaggerEngine)
	if err != nil {
		return err
	}
	if host, port, _ := net.SplitHostPort(strings.TrimPrefix(addr, "tcp://")); host == containerHostAlias {
		if err := s.allowHost(ctx, port); err != nil {
			return err
		}
	}
	s.vars = append(s.vars, envVar{EnvDaggerRunnerHost, addr})
	return nil
}

// hasDagger reports whether the sandbox's template already has the Dagger
// CLI at a release, "v0.20.5" and "0.20.5" being the same one. A probe that
// fails, a CLI that is missing included, is logged and reports false.
func (s *sandbox) hasDagger(ctx context.Context, version string) bool {
	ctx, cancel := context.WithTimeout(ctx, daggerProbeTimeout)
	defer cancel()
	out, err := s.r.sbxCommand(ctx, "exec", s.name, "dagger", "version").CombinedOutput()
	if err != nil {
		if msg := bytes.TrimSpace(out); len(msg) > 0 {
			err = fmt.Errorf("%w: %s", err, msg)
		}
		s.r.Logger.Info("no Dagger CLI in the sandbox's template", "sandbox", s.name, "err", err)
		return false
	}
	have := daggerCLIRelease(string(out))
	if strings.TrimPrefix(have, "v") != strings.TrimPrefix(version, "v") {
		s.r.Logger.Info("the sandbox's template has another Dagger CLI release", "sandbox", s.name, "have", have, "want", version)
		return false
	}
	return true
}

// daggerCLIRelease is the release `dagger version` reports, the field after
// "dagger" in "dagger v0.20.5 (registry.dagger.io/engine:v0.20.5)
// linux/amd64", or "" when the output is not that.
func daggerCLIRelease(out string) string {
	f := strings.Fields(out)
	if len(f) < 2 || f[0] != "dagger" {
		return ""
	}
	return f[1]
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
// socket or a container through a forward this session alone has, a TCP
// engine on the host's loopback at host.docker.internal, and any other TCP
// engine as it is.
func (s *sandbox) daggerAddress(engine string) (string, error) {
	scheme, rest, _ := strings.Cut(engine, "://")
	if scheme == "unix" || scheme == "docker-container" {
		// Always the loopback, whatever ContainerListen says: the forward
		// has no token, unlike the caller's server, and whoever connects
		// to it drives the engine, which is root on the host. The
		// sandbox's proxy reaches it there as host.docker.internal.
		var f *forward
		var err error
		if scheme == "unix" {
			f, err = forwardUnix("127.0.0.1:0", rest, s.r.Logger)
		} else {
			f, err = forwardContainer("127.0.0.1:0", s.r.dockerBin(), rest, s.r.Logger)
		}
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
// to the engine, until it is closed.
type forward struct {
	ln     net.Listener
	engine string
	dial   func() (io.ReadWriteCloser, error)
	mu     sync.Mutex
	conns  map[io.Closer]bool
	closed bool
	wg     sync.WaitGroup
	log    *slog.Logger
}

// forwardUnix listens on listen and forwards every connection to socket,
// logging a connection the socket refuses to log (nil: slog.Default).
func forwardUnix(listen, socket string, log *slog.Logger) (*forward, error) {
	return listenForward(listen, socket, func() (io.ReadWriteCloser, error) {
		return net.Dial("unix", socket)
	}, log)
}

// forwardContainer listens on listen and forwards every connection to the
// engine in the container name, through a `<docker> exec -i <name>
// buildctl dial-stdio` of its own, as the Dagger CLI reaches a
// docker-container engine: the engine publishes no port and its socket
// may be in a virtual machine the host cannot connect into. A process that
// cannot start is logged to log (nil: slog.Default).
func forwardContainer(listen, docker, name string, log *slog.Logger) (*forward, error) {
	if _, err := agentbin.Resolve(docker); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	return listenForward(listen, "docker-container://"+name, func() (io.ReadWriteCloser, error) {
		return dialProcess(log, docker, "exec", "-i", name, "buildctl", "dial-stdio")
	}, log)
}

func listenForward(listen, engine string, dial func() (io.ReadWriteCloser, error), log *slog.Logger) (*forward, error) {
	if log == nil {
		log = slog.Default()
	}
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		return nil, err
	}
	f := &forward{ln: ln, engine: engine, dial: dial, conns: map[io.Closer]bool{}, log: log}
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
		out, err := f.dial()
		if err != nil {
			f.log.Warn("forward to the Dagger engine", "engine", f.engine, "err", err)
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
func (f *forward) track(conns ...io.Closer) bool {
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

// pipe copies one direction, then closes both ends and forgets them:
// either side hanging up ends the connection.
func (f *forward) pipe(dst io.WriteCloser, src io.ReadCloser) {
	defer f.wg.Done()
	_, _ = io.Copy(dst, src)
	_ = dst.Close()
	_ = src.Close()
	f.mu.Lock()
	delete(f.conns, dst)
	delete(f.conns, src)
	f.mu.Unlock()
}

// open is how many connections the forward holds, both ends counted.
func (f *forward) open() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.conns)
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

// processConn is a connection carried by a process's stdin and stdout.
// Closing it ends the process.
type processConn struct {
	cmd    *exec.Cmd
	stdin  *os.File
	stdout *os.File
	stderr *bytes.Buffer
	log    *slog.Logger
	once   sync.Once
}

// processWaitDelay bounds how long closing a processConn waits for the
// process's output once it is killed.
const processWaitDelay = 5 * time.Second

// dialProcess starts bin with args, its stdin and stdout the connection.
// Its own pipes, not exec's, so that a read and Wait can be concurrent.
// What the process says on stderr is logged to log when it ends.
func dialProcess(log *slog.Logger, bin string, args ...string) (*processConn, error) {
	inR, inW, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = inR.Close()
		_ = inW.Close()
		return nil, err
	}
	var stderr bytes.Buffer
	cmd := agentbin.CommandContext(context.Background(), bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = inR, outW, &stderr
	cmd.WaitDelay = processWaitDelay
	err = cmd.Start()
	_ = inR.Close()
	_ = outW.Close()
	if err != nil {
		_ = inW.Close()
		_ = outR.Close()
		return nil, fmt.Errorf("%s: %w", strings.Join(cmd.Args, " "), err)
	}
	return &processConn{cmd: cmd, stdin: inW, stdout: outR, stderr: &stderr, log: log}, nil
}

func (c *processConn) Read(p []byte) (int, error)  { return c.stdout.Read(p) }
func (c *processConn) Write(p []byte) (int, error) { return c.stdin.Write(p) }

// Close ends the process's input, kills it and waits for it.
func (c *processConn) Close() error {
	c.once.Do(func() {
		_ = c.stdin.Close()
		_ = c.cmd.Process.Kill()
		_ = c.stdout.Close()
		_ = c.cmd.Wait()
		if msg := bytes.TrimSpace(c.stderr.Bytes()); len(msg) > 0 {
			c.log.Warn("forward to the Dagger engine", "command", strings.Join(c.cmd.Args, " "), "stderr", string(msg))
		}
	})
	return nil
}
