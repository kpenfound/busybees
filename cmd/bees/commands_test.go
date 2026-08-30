package main

import (
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/state"
)

func TestQueuesTextDependencies(t *testing.T) {
	st := state.Status{
		Queues:        map[string]int{"ready": 4, "triage": 1},
		WaitingOnDeps: map[int][]int{46: {44}, 40: {37, 38}},
	}
	got := queuesText(st)
	for _, want := range []string{
		"  ready          4  (2 waiting on deps)\n",
		"waiting on dependencies:\n",
		"  #40  blocked by #37, #38\n",
		"  #46  blocked by #44\n",
		"  triage         1\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	// Held issues are listed in number order, not map order.
	if strings.Index(got, "#40") > strings.Index(got, "#46") {
		t.Errorf("held issues out of order:\n%s", got)
	}

	got = queuesText(state.Status{Queues: map[string]int{"ready": 4}})
	if !strings.Contains(got, "  ready          4\n") || strings.Contains(got, "waiting on") {
		t.Errorf("no dependencies: %q", got)
	}
}
