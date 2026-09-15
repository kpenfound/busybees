package mcphost_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/mcphost"
	"github.com/kpenfound/busybees/core/work"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestDoneStatusesContextAndReporter(t *testing.T) {
	for _, statuses := range [][]string{{"fired", "glazed"}, nil} {
		r := mcphost.NewRegistry([]string{"potter"}, mcphost.AllTools, mcphost.RejectRole)
		var got []agent.Outcome
		ref := work.Ref{Key: "urn:order:blue", Tags: map[string]string{"batch": "blue"}}
		mcphost.AddDone(r, mcphost.DoneOptions{Title: "Finish", Description: "Report the result.", Statuses: statuses, Work: ref,
			Report: func(_ context.Context, o agent.Outcome) (agent.Outcome, error) {
				if o.Note == "reject" {
					return o, errors.New("caller validation refused")
				}
				got = append(got, o)
				o.Status = "acknowledged"
				return o, nil
			},
		}, "potter")
		ref.Tags["batch"] = "changed"
		srv := server(t, r, "potter")
		list, err := mcphost.Tools(context.Background(), srv, clientInfo)
		if err != nil {
			t.Fatal(err)
		}
		desc := "Report the result."
		if len(statuses) > 0 {
			desc += " Valid statuses for this role: fired, glazed."
		}
		if len(list) != 1 || list[0].Description != desc {
			t.Fatalf("done: %+v", list)
		}
		schema := list[0].InputSchema.(map[string]any)
		status := schema["properties"].(map[string]any)["status"].(map[string]any)
		var want []any
		for _, s := range statuses {
			want = append(want, s)
		}
		if len(want) > 0 && !reflect.DeepEqual(status["enum"], want) {
			t.Fatalf("enum: %v", status)
		}
		if len(want) == 0 && status["enum"] != nil {
			t.Fatalf("unexpected enum: %v", status)
		}
		c := connect(t, srv)
		call := func(status, note string) *mcp.CallToolResult {
			res, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: "done", Arguments: mcphost.DoneInput{Status: status, Note: note}})
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		res := call("fired", "ready")
		if res.IsError || res.Content[0].(*mcp.TextContent).Text != "outcome recorded: acknowledged" {
			t.Fatalf("ack: %+v", res)
		}
		if len(got) != 1 || got[0].Work.Key != "urn:order:blue" || got[0].Work.Tags["batch"] != "blue" || got[0].Note != "ready" {
			t.Fatalf("reported: %+v", got)
		}
		res = call("glazed", "reject")
		if !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "caller validation refused") || len(got) != 1 {
			t.Fatalf("validator bypassed: %+v %+v", res, got)
		}
		res = call("invented", "")
		if len(statuses) > 0 {
			if !res.IsError || len(got) != 1 {
				t.Fatal("invalid enum reached reporter")
			}
		} else if res.IsError || len(got) != 2 {
			t.Fatal("unconstrained status rejected")
		}
	}
}

func TestDoneConcurrentWorkIsolation(t *testing.T) {
	r := mcphost.NewRegistry([]string{"potter"}, mcphost.RejectRole, mcphost.RejectRole)
	ref := work.Ref{Key: "order", Tags: map[string]string{"initial": "yes"}}
	entered := make(chan struct{}, 16)
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	defer unblock()
	var mu sync.Mutex
	seen := map[string]bool{}
	mcphost.AddDoneTool(r, mcphost.DoneOptions{Statuses: []string{"fired"}, Work: ref,
		Report: func(_ context.Context, o agent.Outcome) (agent.Outcome, error) {
			entered <- struct{}{}
			<-release
			mu.Lock()
			defer mu.Unlock()
			if o.Work.Tags["request"] != o.Note {
				return o, fmt.Errorf("shared work: %+v", o)
			}
			seen[o.Note] = true
			return o, nil
		},
	}, func(in mcphost.DoneInput, ref work.Ref) agent.Outcome {
		// Serialize mutations so removing the host's clone yields a deterministic
		// assertion failure as well as being detectable with the race detector.
		mu.Lock()
		ref.Tags["request"] = in.Note
		mu.Unlock()
		return agent.Outcome{Status: in.Status, Note: in.Note, Work: ref}
	})
	c := connect(t, server(t, r, "potter"))
	var wg sync.WaitGroup
	for i := range 16 {
		wg.Go(func() {
			res, err := c.CallTool(context.Background(), &mcp.CallToolParams{Name: "done", Arguments: mcphost.DoneInput{Status: "fired", Note: fmt.Sprint(i)}})
			if err != nil || res.IsError {
				t.Errorf("isolated done: %v %+v", err, res)
			}
		})
	}
	for range 16 {
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("done calls did not reach reporter")
		}
	}
	unblock()
	wg.Wait()
	if len(seen) != 16 || len(ref.Tags) != 1 {
		t.Fatalf("shared work mutated: %d reports; original %+v", len(seen), ref)
	}
}
