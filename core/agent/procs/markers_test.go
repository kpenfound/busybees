package procs

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// A caller's own marker set, not the default agent- one, is what the ps scan
// matches a session by: the session marker, the codex marker and the engine
// client named after the caller's label and naming convention each pick out
// their process, and a process carrying only the default agent- marker is
// left unmatched.
func TestCallerMarkersCustomizeTheProcessScan(t *testing.T) {
	markers := Markers{Session: "--name studio-", Codex: "mcp_servers.tools.env.STUDIO_DIR=", Container: "studio.session"}
	dir := t.TempDir()
	commands := "101 101 claude --name studio-builder --append-system-prompt-file " + dir + "/one/system-prompt.md\n" +
		"102 102 codex exec -c mcp_servers.tools.env.STUDIO_DIR=" + dir + "/two\n" +
		"103 103 docker run --name studio-box --label studio.session=" + dir + "/three\n" +
		"104 104 claude --name agent-other --append-system-prompt-file " + dir + "/four/system-prompt.md\n"
	got := parsePS(commands, 999, dir, markers)
	var pids []int
	for _, p := range got {
		pids = append(pids, p.PID)
	}
	want := []int{101, 102, 103}
	if !slices.Equal(pids, want) {
		t.Fatalf("parsePS with custom markers: got pids %v want %v (%+v)", pids, want, got)
	}
}

// A caller's own container label, not the default agent.session one, is
// what FromContainers asks the engine to filter by, and it finds the
// container carrying that label.
func TestCallerMarkersCustomizeContainerDiscovery(t *testing.T) {
	markers := Markers{Container: "studio.session"}
	dir := t.TempDir()
	three := filepath.Join(dir, "three")
	engineDir := fakeEngine(t, "container-id\t"+three)

	found, err := FromContainers(context.Background(), dir, markers)
	if err != nil || len(found) != 1 || found[0].SessionDir != three {
		t.Fatalf("FromContainers with a custom label: %+v, %v, want the labelled container alone", found, err)
	}
	if calls := strings.Join(listings(t, engineDir), "\n"); !strings.Contains(calls, "label=studio.session") {
		t.Fatalf("engine was asked %q, want a filter on the caller's label", calls)
	}
}
