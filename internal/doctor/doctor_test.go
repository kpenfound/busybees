package doctor

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestStatusRoundTrip(t *testing.T) {
	results := []Result{
		pass("git", GroupToolchain, "/usr/bin/git"),
		warn("filter matches issues", GroupGitHub, "no open issue matches label bees", "check filter.label"),
		fail("workflow labels", GroupGitHub, "1 of 17 missing: bees:ready", "run `bees labels sync`"),
	}
	b, err := json.Marshal(results)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"status":"warn"`) {
		t.Errorf("statuses should marshal as names: %s", b)
	}
	var back []Result
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(back) != len(results) {
		t.Fatalf("got %d results, want %d", len(back), len(results))
	}
	for i := range results {
		if back[i] != results[i] {
			t.Errorf("result %d round-tripped to %+v, want %+v", i, back[i], results[i])
		}
	}
	var s Status
	if err := s.UnmarshalText([]byte("nonsense")); err == nil {
		t.Error("an unknown status name should be an error")
	}
}

// TestCheckWithItsOwnTimeout: a check that declares a longer budget (the MCP
// checks, which give every server MCPTimeout) must not be cut short by the
// runner's default, and CheapChecks must leave the expensive ones out.
func TestCheckWithItsOwnTimeout(t *testing.T) {
	slow := Check{
		Expensive: true,
		Timeout:   2 * time.Second,
		Run: func(ctx context.Context) Result {
			select {
			case <-time.After(150 * time.Millisecond):
				return pass("slow", GroupRoles, "answered")
			case <-ctx.Done():
				return fail("slow", GroupRoles, "cut short", "give it longer")
			}
		},
	}
	cheap := Check{Run: func(context.Context) Result { return pass("quick", GroupConfig, "") }}
	results := RunWith(context.Background(), []Check{slow, cheap}, 10*time.Millisecond)
	if results[0].Status != Pass {
		t.Errorf("a check that declares its own budget must get it: %+v", results[0])
	}
	got := CheapChecks([]Check{slow, cheap})
	if len(got) != 1 || got[0].Run(context.Background()).Name != "quick" {
		t.Errorf("CheapChecks returned %d checks, want only the cheap one", len(got))
	}
}

func TestFailuresAndSummary(t *testing.T) {
	results := []Result{
		pass("a", GroupConfig, ""),
		warn("b", GroupConfig, "", "fix b"),
		fail("c", GroupConfig, "", "fix c"),
		fail("d", GroupConfig, "", "fix d"),
	}
	if n := Failures(results); n != 2 {
		t.Errorf("Failures = %d, want 2", n)
	}
	if n := Failures(results[:2]); n != 0 {
		t.Errorf("warnings must not count as failures, got %d", n)
	}
	want := "4 checks: 1 passed, 1 warnings, 2 failed"
	if got := Summary(results); got != want {
		t.Errorf("Summary = %q, want %q", got, want)
	}
	if got := Summary(results[:1]); got != "1 check: 1 passed, 0 warnings, 0 failed" {
		t.Errorf("singular summary = %q", got)
	}
}

func TestText(t *testing.T) {
	got := Text([]Result{
		pass("gh authenticated", GroupToolchain, "logged in as kyle"),
		fail("workflow labels", GroupGitHub, "1 of 17 missing: bees:ready", "run `bees labels sync`"),
		warn("filter matches issues", GroupGitHub, "no open issue matches label bees", "check filter.label"),
	})
	want := strings.Join([]string{
		"toolchain",
		"  ✓ gh authenticated       logged in as kyle",
		"",
		"github",
		"  ✗ workflow labels        1 of 17 missing: bees:ready",
		"      → run `bees labels sync`",
		"  ! filter matches issues  no open issue matches label bees",
		"      → check filter.label",
		"",
		"3 checks: 1 passed, 1 warnings, 1 failed",
		"",
	}, "\n")
	if got != want {
		t.Errorf("Text():\n%s\nwant:\n%s", got, want)
	}
	if Text(nil) != "" {
		t.Errorf("Text(nil) = %q, want empty", Text(nil))
	}
}

func TestTextGroupOrder(t *testing.T) {
	got := Text([]Result{
		pass("w", GroupWorkspace, ""),
		pass("z", "zebra", ""),
		pass("c", GroupConfig, ""),
		pass("t", GroupToolchain, ""),
	})
	var headings []string
	for _, line := range strings.Split(got, "\n") {
		if line != "" && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "4 checks") {
			headings = append(headings, line)
		}
	}
	want := []string{GroupToolchain, GroupConfig, GroupWorkspace, "zebra"}
	if strings.Join(headings, ",") != strings.Join(want, ",") {
		t.Errorf("group order = %v, want %v", headings, want)
	}
}

func TestRunReportsAPanicAsAFailure(t *testing.T) {
	results := Run(context.Background(), []Check{
		{Run: func(context.Context) Result { panic("boom") }},
		{Run: func(context.Context) Result { return pass("fine", GroupConfig, "") }},
	})
	if len(results) != 2 {
		t.Fatalf("got %d results", len(results))
	}
	if results[0].Status != Fail || !strings.Contains(results[0].Detail, "boom") {
		t.Errorf("panicking check reported as %+v", results[0])
	}
	if results[0].Remediation == "" {
		t.Error("a failing result must carry remediation text")
	}
	if results[1].Status != Pass {
		t.Error("a panic must not stop the remaining checks")
	}
}

func TestRunTimesOutAndCancels(t *testing.T) {
	var cancelled bool
	results := RunWith(context.Background(), []Check{
		// Honours the cancelled context: its own result is used.
		{Run: func(ctx context.Context) Result {
			<-ctx.Done()
			cancelled = true
			return warn("slow but polite", GroupConfig, "gave up", "try again")
		}},
		// Ignores it: the runner reports the overrun itself.
		{Run: func(context.Context) Result {
			time.Sleep(500 * time.Millisecond)
			return pass("rude", GroupConfig, "")
		}},
	}, 20*time.Millisecond)

	if !cancelled {
		t.Error("the check's context should have been cancelled")
	}
	if results[0].Name != "slow but polite" {
		t.Errorf("a check that returns after cancellation keeps its result: %+v", results[0])
	}
	if results[1].Status != Fail || !strings.Contains(results[1].Detail, "did not finish") {
		t.Errorf("overrunning check reported as %+v", results[1])
	}
	if results[1].Remediation == "" {
		t.Error("a failing result must carry remediation text")
	}
}
