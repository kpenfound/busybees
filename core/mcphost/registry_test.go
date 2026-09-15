package mcphost_test

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/mcphost"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

var metadata = mcp.Implementation{Name: "arbitrary-factory", Title: "Workshop", Version: "42"}
var clientInfo = mcp.Implementation{Name: "test-client", Version: "1"}

type echoInput struct {
	Value string `json:"value"`
}
type echoOutput struct {
	Value string `json:"value"`
}

func echo(_ context.Context, _ *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
	return nil, echoOutput(in), nil
}

func server(t *testing.T, r *mcphost.Registry, role string) *mcp.Server {
	t.Helper()
	srv, err := r.NewServer(role, metadata)
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

func connect(t *testing.T, srv *mcp.Server) *mcp.ClientSession {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	c, s, err := mcphost.Connect(ctx, srv, clientInfo)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
		if err := s.Wait(); !mcphost.IsCleanShutdown(err) {
			t.Error(err)
		}
		cancel()
	})
	if got := c.InitializeResult().ServerInfo; !reflect.DeepEqual(*got, metadata) {
		t.Fatalf("metadata = %+v", got)
	}
	return c
}

func TestRoleFilteringAndDispatch(t *testing.T) {
	r := mcphost.NewRegistry([]string{"potter", "weaver"}, mcphost.AllTools, mcphost.RejectRole)
	mcphost.AddTool(r, &mcp.Tool{Name: "echo"}, echo)
	mcphost.AddTool(r, &mcp.Tool{Name: "kiln"}, echo, "potter")
	mcphost.AddTool(r, &mcp.Tool{Name: "loom"}, echo, "weaver")
	for role, want := range map[string][]string{"potter": {"echo", "kiln"}, "weaver": {"echo", "loom"}, "": {"echo", "kiln", "loom"}} {
		t.Run(role, func(t *testing.T) {
			srv := server(t, r, role)
			list, err := mcphost.Tools(context.Background(), srv, clientInfo)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, tool := range list {
				names = append(names, tool.Name)
			}
			if !reflect.DeepEqual(names, want) {
				t.Fatalf("tools = %v, want %v", names, want)
			}
			c := connect(t, srv)
			for _, name := range want {
				res, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: echoInput{Value: name}})
				if err != nil || res.IsError {
					t.Fatalf("call %s: %v %+v", name, err, res)
				}
				data, _ := json.Marshal(res.StructuredContent)
				if string(data) != fmt.Sprintf(`{"value":%q}`, name) {
					t.Errorf("dispatch %s: %s", name, data)
				}
			}
			hidden := "missing"
			if role == "potter" {
				hidden = "loom"
			}
			for range 2 {
				_, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: hidden, Arguments: echoInput{}})
				if err == nil || !strings.Contains(err.Error(), hidden) {
					t.Fatalf("unknown/hidden tool: %v", err)
				}
			}
		})
	}
	if _, err := r.NewServer("visitor", metadata); err == nil {
		t.Fatal("unknown role accepted")
	}
}

func TestFallbackPolicies(t *testing.T) {
	for _, empty := range []mcphost.RolePolicy{mcphost.RejectRole, mcphost.AllTools, mcphost.NoTools} {
		for _, unknown := range []mcphost.RolePolicy{mcphost.RejectRole, mcphost.AllTools, mcphost.NoTools} {
			r := mcphost.NewRegistry([]string{"pilot"}, empty, unknown)
			mcphost.AddTool(r, &mcp.Tool{Name: "fly"}, echo, "pilot")
			for role, policy := range map[string]mcphost.RolePolicy{"": empty, "visitor": unknown} {
				srv, err := r.NewServer(role, metadata)
				if policy == mcphost.RejectRole {
					if err == nil {
						t.Fatal("expected role rejection")
					}
					continue
				}
				if err != nil {
					t.Fatal(err)
				}
				list, err := mcphost.Tools(context.Background(), srv, clientInfo)
				if err != nil {
					t.Fatal(err)
				}
				want := 0
				if policy == mcphost.AllTools {
					want = 1
				}
				if len(list) != want {
					t.Fatalf("role %q policy %d: %d tools", role, policy, len(list))
				}
			}
		}
	}
}

func mustPanic(t *testing.T, fn func()) {
	t.Helper()
	defer func() {
		if recover() == nil {
			t.Error("expected registration panic")
		}
	}()
	fn()
}

func TestRegistrationRejectsDuplicatesAndInvalidScopes(t *testing.T) {
	r := mcphost.NewRegistry([]string{"pilot", "navigator"}, mcphost.RejectRole, mcphost.RejectRole)
	mcphost.AddTool(r, &mcp.Tool{Name: "fly"}, echo, "pilot")
	mustPanic(t, func() { mcphost.AddTool(r, &mcp.Tool{Name: "fly"}, echo, "navigator") })
	mustPanic(t, func() { mcphost.AddTool(r, &mcp.Tool{Name: "ghost"}, echo, "unknown") })
	mustPanic(t, func() { mcphost.NewRegistry([]string{"pilot", "pilot"}, mcphost.AllTools, mcphost.AllTools) })
	mustPanic(t, func() { mcphost.SchemaFor[echoInput](map[string][]string{"absent": {"x"}}) })
	// A rejected registration does not replace the existing handler or publish
	// the invalid tool, even for an otherwise valid role.
	list, err := mcphost.Tools(context.Background(), server(t, r, "navigator"), clientInfo)
	if err != nil || len(list) != 0 {
		t.Fatalf("rejected registration leaked: %v %v", list, err)
	}
}

func TestMetadataSnapshotAndConcurrentInputs(t *testing.T) {
	roles := []string{"pilot"}
	r := mcphost.NewRegistry(roles, mcphost.RejectRole, mcphost.RejectRole)
	schema := mcphost.SchemaFor[echoInput](nil)
	tool := &mcp.Tool{Name: "echo", Title: "original", InputSchema: schema}
	mcphost.AddTool(r, tool, echo, "pilot")
	roles[0] = "changed"
	tool.Title = "changed"
	schema.Properties["value"].Enum = []any{"forbidden"}
	srv := server(t, r, "pilot")
	mcphost.AddTool(r, &mcp.Tool{Name: "later"}, echo)
	list, err := mcphost.Tools(context.Background(), srv, clientInfo)
	if err != nil || len(list) != 1 || list[0].Title != "original" {
		t.Fatalf("snapshot: %v %v", list, err)
	}
	c := connect(t, srv)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			value := fmt.Sprint(i)
			res, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: "echo", Arguments: echoInput{Value: value}})
			if err != nil || res.IsError {
				t.Errorf("echo: %v %+v", err, res)
				return
			}
			data, _ := json.Marshal(res.StructuredContent)
			if string(data) != fmt.Sprintf(`{"value":%q}`, value) {
				t.Errorf("request mixed: %s", data)
			}
		})
	}
	wg.Wait()
}
