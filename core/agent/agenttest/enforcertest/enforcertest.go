// Package enforcertest is an agent.Enforcer that starts no process: a caller
// tests what it does around Prepare, Run and Release, and what its grants
// let a turn do, with a Go function playing the agent. It lives beside
// agenttest rather than in it because it imports agent, which agent's own
// tests could then not import agenttest from.
//
// The fake checks grants and requests with the code the real enforcers use
// (agent.NewPolicy, agent.Admit) and judges what its agent tries with the
// policy's own Reads, Writes, Runs and Allows. It has no platform: its policy
// names no system paths and no denied paths, and it knows a VCS executable
// by its name alone.
package enforcertest

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/kpenfound/busybees/core/agent"
)

// Enforcer fakes the enforcer of one sandbox kind.
type Enforcer struct {
	// Sandbox is the kind: agent.SandboxNone when empty.
	Sandbox string
	// Image is what a container kind runs, as agent.NewContainer is given it.
	Image string
	// PrepareErr, when set, is what Prepare returns: a platform that cannot
	// enforce the kind returns one that wraps agent.ErrUnsupported.
	PrepareErr error
	// Agent plays the agent of every turn. Nil is an agent that does nothing
	// and succeeds.
	Agent func(ctx context.Context, turn *Turn) (*agent.Result, error)

	mu       sync.Mutex
	sessions []*Session
}

// Prepare checks the grants the way a real enforcer does and starts nothing.
func (e *Enforcer) Prepare(ctx context.Context, g agent.Grants) (agent.Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if e.PrepareErr != nil {
		return nil, e.PrepareErr
	}
	policy, err := agent.NewPolicy(e.Sandbox, g)
	if err != nil {
		return nil, err
	}
	if policy.Sandbox == agent.SandboxContainer {
		if e.Image == "" {
			return nil, fmt.Errorf("%w: a container enforcer needs the image its turns run", agent.ErrUnsupported)
		}
		policy.Image = e.Image
	}
	g.Env, g.Tools, g.Mounts = slices.Clone(g.Env), slices.Clone(g.Tools), slices.Clone(g.Mounts)
	s := &Session{enforcer: e, policy: policy, grants: g}
	e.mu.Lock()
	e.sessions = append(e.sessions, s)
	e.mu.Unlock()
	return s, nil
}

// Sessions are the sessions prepared so far, oldest first.
func (e *Enforcer) Sessions() []*Session {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.sessions)
}

// Session is a prepared fake session.
type Session struct {
	enforcer *Enforcer
	policy   agent.Policy
	grants   agent.Grants

	mu       sync.Mutex
	released bool
	requests []agent.Request
}

// Policy is the policy of the grants alone.
func (s *Session) Policy() agent.Policy {
	p := s.policy
	p.Env, p.Tools, p.MCPServers = slices.Clone(p.Env), slices.Clone(p.Tools), slices.Clone(p.MCPServers)
	p.Mounts, p.DeniedExecutables, p.Binds = slices.Clone(p.Mounts), slices.Clone(p.DeniedExecutables), slices.Clone(p.Binds)
	return p
}

// Run admits the request the way a real session does and hands the turn to
// the enforcer's Agent.
func (s *Session) Run(ctx context.Context, req agent.Request) (*agent.Result, error) {
	s.mu.Lock()
	released := s.released
	s.mu.Unlock()
	if released {
		return nil, agent.ErrReleased
	}
	admitted, err := agent.Admit(s.policy, s.grants, req)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", req.Profile.Name, err)
	}
	s.mu.Lock()
	s.requests = append(s.requests, admitted)
	s.mu.Unlock()
	res := &agent.Result{}
	if s.enforcer.Agent != nil {
		if res, err = s.enforcer.Agent(ctx, &Turn{Request: admitted, Policy: s.Policy()}); err != nil {
			return nil, err
		}
		if res == nil {
			res = &agent.Result{}
		}
	}
	if res.Name == "" {
		res.Name = admitted.Name
	}
	if res.Role == "" {
		res.Role = admitted.Profile.Name
	}
	if res.SessionDir == "" {
		res.SessionDir = admitted.SessionDir
	}
	return res, nil
}

// Release marks the session released.
func (s *Session) Release(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.released = true
	return nil
}

// Released reports whether Release was called.
func (s *Session) Released() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.released
}

// Requests are the requests the session ran, as it admitted them.
func (s *Session) Requests() []agent.Request {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.requests)
}

// Turn is what a fake agent is handed: the admitted request, the policy it
// runs under, and the things an agent tries, each judged by the policy. A
// refusal wraps fs.ErrPermission.
type Turn struct {
	Request agent.Request
	Policy  agent.Policy
}

func refused(what, subject string) error {
	return fmt.Errorf("%s %s: %w by the session's policy", what, subject, fs.ErrPermission)
}

// ReadFile reads a file of this machine when the policy reads it.
func (t *Turn) ReadFile(path string) ([]byte, error) {
	if !t.Policy.Reads(path) {
		return nil, refused("read", path)
	}
	return os.ReadFile(path)
}

// WriteFile writes a file of this machine when the policy writes it.
func (t *Turn) WriteFile(path string, data []byte) error {
	if !t.Policy.Writes(path) {
		return refused("write", path)
	}
	return os.WriteFile(path, data, 0o644)
}

// Exec reports whether the turn could run an executable, and runs nothing.
// The fake has no machine to look for VCS executables on, so a denied name
// is denied under every path that ends in it. Any other name runs, and any
// other path runs when the policy runs it.
func (t *Turn) Exec(name string) error {
	if slices.Contains(t.Policy.DeniedExecutables, filepath.Base(name)) {
		return refused("run", name)
	}
	if strings.ContainsRune(name, filepath.Separator) && !t.Policy.Runs(name) {
		return refused("run", name)
	}
	return nil
}

// UseTool reports whether the turn has a tool: a built-in one, or one of an
// MCP server ("mcp__<server>__<tool>").
func (t *Turn) UseTool(tool string) error {
	if !t.Policy.Allows(tool) {
		return refused("use tool", tool)
	}
	return nil
}
