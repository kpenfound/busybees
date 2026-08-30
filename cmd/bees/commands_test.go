package main

import (
	"os"
	"sort"
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

// TestCLAUDEMdListsEveryCommand pins the `cmd/bees` bullet in CLAUDE.md — the map
// every session reads before touching this repo — against the commands actually
// registered in newRootWithFlags. It compares sets, not order: the order in the
// bullet is a readability choice, not something to fail a test on.
func TestCLAUDEMdListsEveryCommand(t *testing.T) {
	// Go runs a test with its package directory as cwd, so the repo file is
	// two levels up. Reading it (rather than embedding a copy) is the point:
	// the doc is what has to stay true.
	const claudeMd = "../../CLAUDE.md"
	const bullet = "- `cmd/bees`"

	data, err := os.ReadFile(claudeMd)
	if err != nil {
		t.Fatalf("read %s: %v", claudeMd, err)
	}
	line := ""
	for _, l := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(l, bullet) {
			line = l
			break
		}
	}
	if line == "" {
		t.Fatalf("CLAUDE.md: no line starts with %q. That bullet lists the CLI commands and is kept in sync with cmd/bees/main.go; restore it (or update this test) rather than leaving the layout map without one.", bullet)
	}

	documented := map[string]bool{}
	for _, name := range backticked(line) {
		if strings.Contains(name, "/") { // `cmd/bees` itself, not a command
			continue
		}
		documented[name] = true
	}

	registered := map[string]bool{}
	for _, c := range newRoot().Commands() {
		// cobra adds `help` itself and `completion` lazily; both are documented
		// in docs/cli.md instead of in the layout bullet.
		if c.Name() == "help" || c.Name() == "completion" {
			continue
		}
		registered[c.Name()] = true
	}

	var missing, extra []string
	for name := range registered {
		if !documented[name] {
			missing = append(missing, name)
		}
	}
	for name := range documented {
		if !registered[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)

	if len(missing) > 0 {
		t.Errorf("CLAUDE.md: the %q line does not list %s, registered in newRootWithFlags (cmd/bees/main.go). Add the missing names to that line:\n%s",
			bullet, strings.Join(missing, ", "), line)
	}
	if len(extra) > 0 {
		t.Errorf("CLAUDE.md: the %q line lists %s, no longer a bees command. Drop the stale names from that line:\n%s",
			bullet, strings.Join(extra, ", "), line)
	}
}

// backticked returns the `…`-quoted spans of a markdown line, in order.
func backticked(line string) []string {
	var out []string
	for {
		i := strings.Index(line, "`")
		if i < 0 {
			return out
		}
		rest := line[i+1:]
		j := strings.Index(rest, "`")
		if j < 0 {
			return out
		}
		out = append(out, rest[:j])
		line = rest[j+1:]
	}
}
