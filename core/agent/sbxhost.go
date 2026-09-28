package agent

import (
	"fmt"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// A SandboxSbx session reaches a server on the host's loopback only through
// a network policy rule the runner adds for its sandbox alone (allowHost).
// The runner adds one for the server it starts itself (Request.HostMCP) and
// for a Dagger engine it is granted (sbxdagger.go). A caller that runs its
// own MCP server, already listening before the turn starts, grants it by
// name and port (Grants.HostServers) and gives the session an HTTP entry
// for it in Profile.MCP. The granted entry's URL names the host as the host
// sees it, a loopback address or "localhost", or as the sandbox reaches it,
// host.docker.internal; the session is given it at host.docker.internal
// either way. The listener stays the caller's: the runner neither starts
// nor stops it, and allows its port for as long as the sandbox exists.

// HostServer grants a SandboxSbx session one MCP server the caller runs on
// the host's loopback: the sandbox, and no other, is allowed to reach Port
// while it exists (`sbx policy allow network --sandbox <name>
// localhost:<port>`). The profile's MCP entry of that name must be an HTTP
// entry whose URL names the host and Port, or the request is refused. The
// server must also be granted as a tool ("mcp__<name>"). A grant widens
// nothing else: a granted server the profile has no entry for gets no
// rule, and an entry that is not granted is passed as it is and gets none
// either, so it reaches the host only if the machine's own policy lets
// every sandbox do so.
type HostServer struct {
	// Name is the server's name, the key of its Profile.MCP entry.
	Name string
	// Port is the port it listens on, on the host's loopback.
	Port int
}

// checkHostServers checks the host server grants on their own: each names
// a granted MCP server once, and a port. servers are the granted MCP
// servers (splitTools).
func checkHostServers(g *Grants, servers map[string]bool) error {
	seen := map[string]bool{}
	for _, h := range g.HostServers {
		switch {
		case h.Name == "":
			return fmt.Errorf("host server on port %d has no name", h.Port)
		case seen[h.Name]:
			return fmt.Errorf("host server %q is granted twice", h.Name)
		case !servers[h.Name]:
			return fmt.Errorf("%w: host server %q is granted and its MCP server is not; grant %q", ErrNotGranted, h.Name, "mcp__"+h.Name)
		case h.Port < 1 || h.Port > 65535:
			return fmt.Errorf("host server %q: port %d is not a port", h.Name, h.Port)
		}
		seen[h.Name] = true
	}
	return nil
}

// hostServers are the granted host servers the profile's MCP entries
// reach, in name order. A granted server the profile has no entry for is
// left out. One whose entry is not an HTTP entry on the host at the granted
// port is refused: the grant says where the server is, and the session is
// given no other port for it. An entry that is not granted is the
// profile's own, passed as it is and given no rule.
func hostServers(req Request) ([]HostServer, error) {
	var out []HostServer
	for _, h := range req.Grants.HostServers {
		e, ok := req.Profile.MCP[h.Name]
		if !ok {
			continue
		}
		if e.Command != "" || e.URL == "" {
			return nil, fmt.Errorf("%w: host server %q is granted and its MCP entry is not an HTTP entry", ErrUnsupported, h.Name)
		}
		u, port, err := hostURL(e.URL)
		switch {
		case err != nil:
			return nil, fmt.Errorf("host server %q: %w", h.Name, err)
		case u == nil:
			return nil, fmt.Errorf("%w: host server %q is granted and its MCP entry %s is not on the host's loopback", ErrNotGranted, h.Name, e.URL)
		case port != h.Port:
			return nil, fmt.Errorf("%w: host server %q is granted port %d and its MCP entry names port %d", ErrNotGranted, h.Name, h.Port, port)
		}
		out = append(out, h)
	}
	slices.SortFunc(out, func(a, b HostServer) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// hostURL parses an MCP entry's URL and, when it names the host, returns
// it and its port; a URL that names any other host is nil. A URL on the
// host must be HTTP or HTTPS and name its port, which is what the network
// rule is scoped to.
func hostURL(raw string) (*url.URL, int, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, 0, err
	}
	if host := u.Hostname(); host != containerHostAlias && !isLoopback(host) {
		return nil, 0, nil
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, 0, fmt.Errorf("%w: %s is on the host and is not an HTTP URL", ErrUnsupported, raw)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil || port < 1 || port > 65535 {
		return nil, 0, fmt.Errorf("%w: %s is on the host and names no port", ErrUnsupported, raw)
	}
	return u, port, nil
}

// sandboxURL is an MCP entry's URL on the host as the sandbox reaches it:
// at host.docker.internal, which the sandbox's proxy turns into the host's
// localhost. Verify has checked it (hostURL).
func sandboxURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.Host = net.JoinHostPort(containerHostAlias, u.Port())
	return u.String()
}
