package doctor

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/session"
	"github.com/kpenfound/busybees/internal/text"
)

// probeVersion is the client version doctor introduces itself with; a server
// that logs its clients then shows where the connection came from.
const probeVersion = "1"

// MCPTimeout is how long a configured MCP server has to start and answer an
// initialize request before doctor calls it unreachable.
const MCPTimeout = 15 * time.Second

// roleChecks returns the per-role checks: one result per role per thing that
// role configures, so the table names the role that is broken rather than
// reporting "some MCP server is down".
//
// They are all marked expensive: a skill is cloned and an MCP server is
// started, which is exactly what `bees run` must not do on every start.
func (d *Deps) roleChecks() []Check {
	var checks []Check
	for _, name := range config.Roles {
		role, err := d.Config.Role(name)
		if err != nil {
			checks = append(checks, Check{Expensive: true, Run: func(context.Context) Result {
				return fail(name, GroupRoles, oneLine(err.Error()),
					fmt.Sprintf("fix the [roles.%s] section of %s", name, d.Config.Path))
			}})
			continue
		}
		// A disabled role is skipped rather than dropped: "developer disabled"
		// is the answer to "why is nothing being built?".
		if !role.Enabled {
			checks = append(checks, Check{Expensive: true, Run: func(context.Context) Result {
				return pass(name, GroupRoles, fmt.Sprintf("disabled (roles.%s.enabled = false)", name))
			}})
			continue
		}
		var roleChecks []Check
		if len(role.Skills) > 0 {
			roleChecks = append(roleChecks, Check{Expensive: true, Run: d.checkRoleSkills(role)})
		}
		if len(role.MCP) > 0 {
			roleChecks = append(roleChecks, Check{
				Expensive: true,
				// Every server gets the full budget, one after the other.
				Timeout: time.Duration(len(role.MCP))*MCPTimeout + Timeout,
				Run:     d.checkRoleMCP(role),
			})
		}
		if role.Shell != "" {
			roleChecks = append(roleChecks, Check{Expensive: true, Run: d.checkRoleShell(role)})
		}
		if len(roleChecks) == 0 {
			// Still one row, so the group never looks like it did not run.
			roleChecks = append(roleChecks, Check{Expensive: true, Run: func(context.Context) Result {
				return pass(name, GroupRoles, "enabled, no skills, MCP servers or shell configured")
			}})
		}
		checks = append(checks, roleChecks...)
	}
	return checks
}

// checkRoleSkills clones every skill the role configures, through the same
// manager (and the same cache) a session uses, so a run afterwards finds them
// warm.
func (d *Deps) checkRoleSkills(role config.ResolvedRole) func(context.Context) Result {
	return func(ctx context.Context) Result {
		name := role.Name + " skills"
		if d.Skills == nil {
			return fail(name, GroupRoles, "no skills cache is available",
				"set $BEES_CACHE_DIR to a writable directory")
		}
		var ready, broken []string
		for _, ref := range role.Skills {
			// One at a time: a broken reference must not hide the ones after it.
			// Prepare puts the reference itself into the error.
			if _, err := d.Skills.Prepare(ctx, []string{ref}); err != nil {
				broken = append(broken, oneLine(err.Error()))
				continue
			}
			ready = append(ready, ref)
		}
		if len(broken) > 0 {
			return fail(name, GroupRoles, strings.Join(broken, "; "),
				fmt.Sprintf("fix the roles.%s.skills entries in %s, or run `bees skills list` to inspect the cache",
					role.Name, d.Config.Path))
		}
		return pass(name, GroupRoles, fmt.Sprintf("%s ready: %s",
			text.Count(len(ready), "skill"), strings.Join(ready, ", ")))
	}
}

// checkRoleMCP starts every MCP server the role configures and waits for it to
// answer an initialize request. The servers are described by the very file a
// session would be given (session.WriteMCPConfig), so doctor probes the thing
// that actually runs, $VAR expansion included.
func (d *Deps) checkRoleMCP(role config.ResolvedRole) func(context.Context) Result {
	return func(ctx context.Context) Result {
		name := role.Name + " mcp"
		entries := session.MCPEntries(role.MCP)
		dir, err := os.MkdirTemp("", "bees-doctor-mcp-")
		if err != nil {
			return fail(name, GroupRoles, "no temporary directory: "+oneLine(err.Error()),
				"make $TMPDIR writable: every session writes its mcp.json there")
		}
		defer func() { _ = os.RemoveAll(dir) }()
		if err := session.WriteMCPConfig(filepath.Join(dir, "mcp.json"), entries); err != nil {
			return fail(name, GroupRoles, "cannot write the mcp config: "+oneLine(err.Error()),
				fmt.Sprintf("check the [roles.%s.mcp] servers in %s", role.Name, d.Config.Path))
		}
		var ok, broken []string
		for _, server := range slices.Sorted(maps.Keys(entries)) {
			if err := probeMCP(ctx, entries[server]); err != nil {
				broken = append(broken, fmt.Sprintf("%s: %s", server, oneLine(err.Error())))
				continue
			}
			ok = append(ok, server)
		}
		if len(broken) > 0 {
			return fail(name, GroupRoles, strings.Join(broken, "; "),
				fmt.Sprintf("start the server by hand or fix [roles.%s.mcp] in %s: a session that cannot reach it loses those tools",
					role.Name, d.Config.Path))
		}
		return pass(name, GroupRoles, fmt.Sprintf("%s answered: %s",
			text.Count(len(ok), "server"), strings.Join(ok, ", ")))
	}
}

// probeMCP connects to one configured server and completes the initialize
// handshake, then disconnects. A stdio server is spawned and killed; an http
// or sse server is contacted once.
func probeMCP(ctx context.Context, e session.MCPEntry) error {
	ctx, cancel := context.WithTimeout(ctx, MCPTimeout)
	defer cancel()

	var t mcp.Transport
	switch {
	case e.Type == "http":
		t = &mcp.StreamableClientTransport{
			Endpoint:   e.URL,
			HTTPClient: httpClient(e.Headers),
			MaxRetries: -1, // doctor asks once; retrying only hides the failure
			// doctor wants one round trip, not a persistent event stream.
			DisableStandaloneSSE: true,
		}
	case e.Type == "sse":
		t = &mcp.SSEClientTransport{Endpoint: e.URL, HTTPClient: httpClient(e.Headers)}
	case e.Command == "":
		return fmt.Errorf("neither a command nor a url is configured")
	default:
		cmd := exec.CommandContext(ctx, e.Command, e.Args...)
		cmd.Env = append(os.Environ(), envPairs(e.Env)...)
		// The probe is over as soon as initialize is answered: do not wait the
		// SDK's default five seconds for a server that ignores a closed stdin.
		t = &mcp.CommandTransport{Command: cmd, TerminateDuration: time.Second}
	}
	client := mcp.NewClient(&mcp.Implementation{Name: "bees-doctor", Version: probeVersion}, nil)
	cs, err := client.Connect(ctx, t, nil)
	if err != nil {
		return err
	}
	return cs.Close()
}

// envPairs renders an entry's environment as KEY=VALUE, in a stable order.
func envPairs(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for _, k := range slices.Sorted(maps.Keys(env)) {
		out = append(out, k+"="+env[k])
	}
	return out
}

// httpClient returns a client that sends the entry's configured headers, the
// way claude does for an http or sse server.
func httpClient(headers map[string]string) *http.Client {
	if len(headers) == 0 {
		return nil
	}
	return &http.Client{Transport: headerRoundTripper{headers: headers}}
}

type headerRoundTripper struct{ headers map[string]string }

func (h headerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	for k, v := range h.headers {
		req.Header.Set(k, v)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// checkRoleShell checks that a configured shell exists and can be executed: a
// session inherits it as $SHELL, and every Bash tool call fails when it cannot
// be run.
//
// Loading bees.toml already refuses a shell that is missing or a directory, so
// the failure that gets here is the one validation does not look at: a file
// without the executable bit.
func (d *Deps) checkRoleShell(role config.ResolvedRole) func(context.Context) Result {
	return func(context.Context) Result {
		name := role.Name + " shell"
		remediation := fmt.Sprintf("run `chmod +x %s`, or set roles.%s.shell (or global.shell) in %s to a shell that can be run",
			role.Shell, role.Name, d.Config.Path)
		st, err := os.Stat(role.Shell)
		if err != nil {
			return fail(name, GroupRoles, oneLine(err.Error()), remediation)
		}
		if st.IsDir() || st.Mode().Perm()&0o111 == 0 {
			return fail(name, GroupRoles, role.Shell+" is not executable", remediation)
		}
		return pass(name, GroupRoles, role.Shell)
	}
}
