package agent

import (
	"context"
	"maps"
)

const (
	EnvRole       = "TASK_ROLE"
	EnvSessionDir = "TASK_SESSION_DIR"
	EnvStateDir   = "TASK_STATE_DIR"
	EnvIssue      = "TASK_ISSUE"
	EnvPR         = "TASK_PR"
	EnvBranch     = "TASK_BRANCH"
	EnvBin        = "TASK_BIN"
	EnvMCPToken   = "TASK_MCP_TOKEN"
)

var testDomains = []string{"example.com", "*.example.com"}

// testRunner is a caller fixture. Defaults here are not runner policy.
type testRunner struct {
	*Runner
	StateDir  string
	ServerBin string
}

func (r *testRunner) Run(ctx context.Context, req Request) (*Result, error) {
	if req.SessionDir == "" {
		dir, err := r.NewSessionDir(req.Name)
		if err != nil {
			return nil, err
		}
		req.SessionDir = dir
	}
	req.Profile.VCSAccess = true
	req.Profile.SandboxDomains = testDomains
	req.Env = maps.Clone(req.Env)
	if req.Env == nil {
		req.Env = map[string]string{}
	}
	req.Env[EnvSessionDir] = req.SessionDir
	if req.Profile.Sandbox != SandboxContainer {
		req.Env[EnvBin] = r.ServerBin
	}
	req.Env[EnvRole] = req.Profile.Name
	req.Env[EnvStateDir] = r.StateDir
	req.Env["ACCESS_TOKEN"] = "secret"
	req.Profile.MCP = MCPEntries(req.Profile.MCP)
	if req.Profile.MCP == nil {
		req.Profile.MCP = map[string]MCPEntry{}
	}
	entry := MCPEntry{Command: r.ServerBin, Args: []string{"mcp", "serve"}, Env: map[string]string{EnvSessionDir: req.SessionDir, EnvRole: req.Profile.Name, EnvStateDir: r.StateDir}}
	for _, key := range []string{EnvIssue, EnvPR, EnvBranch} {
		if v := req.Env[key]; v != "" {
			entry.Env[key] = v
		}
	}
	req.Profile.MCP["tools"] = entry
	req.HostMCP = &HostMCP{Name: "tools", Entry: entry, ListenArgs: []string{"--listen"}, TokenEnv: EnvMCPToken, ListeningPrefix: "listening on ", Path: "/mcp"}
	core := *r.Runner
	if r.StateDir != "" {
		core.MountDirs = []string{r.StateDir}
	}
	return core.Run(ctx, req)
}
