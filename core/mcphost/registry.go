// Package mcphost provides MCP registration and lifecycle without application
// roles or workflow policy. Handlers and their collaborators belong to callers.
package mcphost

import (
	"encoding/json"
	"fmt"
	"slices"
	"sync"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// RolePolicy determines what an empty or unknown role may see.
type RolePolicy int

const (
	RejectRole RolePolicy = iota
	AllTools
	NoTools
)

// Registry holds typed registrations. Servers are independent snapshots;
// later registrations do not change existing servers. Handlers must synchronize
// their own collaborators, but request decoding is private to each call.
type Registry struct {
	mu             sync.Mutex
	roles          []string
	empty, unknown RolePolicy
	tools          []registration
	names          map[string]bool
}

type registration struct {
	roles   []string
	install func(*mcp.Server)
}

// NewRegistry takes the complete role set and explicit fallback policies.
// Invalid policies and empty or duplicate role names are programming errors.
func NewRegistry(roles []string, empty, unknown RolePolicy) *Registry {
	for _, p := range []RolePolicy{empty, unknown} {
		if p < RejectRole || p > NoTools {
			panic("mcphost: invalid role policy")
		}
	}
	seen := map[string]bool{}
	for _, role := range roles {
		if role == "" || seen[role] {
			panic(fmt.Sprintf("mcphost: invalid or duplicate role %q", role))
		}
		seen[role] = true
	}
	return &Registry{roles: slices.Clone(roles), empty: empty, unknown: unknown, names: map[string]bool{}}
}

// AddTool registers a typed handler. No roles means every known role. Duplicate
// names (even across disjoint roles), unknown scopes and invalid metadata panic,
// like the SDK's typed registration. Metadata is copied; caller edits cannot
// alter a registration. An unregistered or hidden tool gets the SDK's unknown
// tool error and never reaches a handler.
func AddTool[In, Out any](r *Registry, tool *mcp.Tool, handler mcp.ToolHandlerFor[In, Out], roles ...string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if tool == nil || tool.Name == "" || handler == nil {
		panic("mcphost: tool needs a name and handler")
	}
	if r.names[tool.Name] {
		panic(fmt.Sprintf("mcphost: duplicate tool %q", tool.Name))
	}
	for _, role := range roles {
		if !slices.Contains(r.roles, role) {
			panic(fmt.Sprintf("mcphost: unknown tool role %q", role))
		}
	}
	data, err := json.Marshal(tool)
	if err != nil {
		panic(fmt.Sprintf("mcphost: tool metadata: %v", err))
	}
	install := func(srv *mcp.Server) {
		var copy mcp.Tool
		if err := json.Unmarshal(data, &copy); err != nil {
			panic(err)
		}
		mcp.AddTool(srv, &copy, handler)
	}
	// Validate schemas during registration, before publishing the entry.
	install(mcp.NewServer(&mcp.Implementation{Name: "schema-validation", Version: "1"}, nil))
	r.tools = append(r.tools, registration{roles: slices.Clone(roles), install: install})
	r.names[tool.Name] = true
}

// NewServer constructs a server for role using caller-provided MCP metadata.
func (r *Registry) NewServer(role string, metadata mcp.Implementation) (*mcp.Server, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	policy := RejectRole
	known := slices.Contains(r.roles, role)
	if role == "" {
		policy = r.empty
	} else if !known {
		policy = r.unknown
	}
	if !known && policy == RejectRole {
		return nil, fmt.Errorf("mcphost: unknown role %q", role)
	}
	srv := mcp.NewServer(&metadata, nil)
	for _, tool := range r.tools {
		if (!known && policy == AllTools) || (known && (len(tool.roles) == 0 || slices.Contains(tool.roles, role))) {
			tool.install(srv)
		}
	}
	return srv, nil
}

// SchemaFor infers a typed input schema and constrains named properties. Empty
// value lists leave properties unconstrained. Invalid types/properties panic.
func SchemaFor[In any](enums map[string][]string) *jsonschema.Schema {
	s, err := jsonschema.For[In](nil)
	if err != nil {
		panic(fmt.Sprintf("mcphost: schema for %T: %v", *new(In), err))
	}
	for prop, values := range enums {
		p, ok := s.Properties[prop]
		if !ok {
			panic(fmt.Sprintf("mcphost: %T has no property %q", *new(In), prop))
		}
		for _, v := range values {
			p.Enum = append(p.Enum, v)
		}
	}
	return s
}
