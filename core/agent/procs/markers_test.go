package procs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func TestCallerMarkers(t *testing.T) {
	markers := Markers{Session: "--name studio-", Codex: "mcp_servers.tools.env.STUDIO_DIR=", Container: "studio.session"}
	dir := t.TempDir()
	commands := "101 101 claude --name studio-builder --append-system-prompt-file " + dir + "/one/system-prompt.md\n" +
		"102 102 codex exec -c mcp_servers.tools.env.STUDIO_DIR=" + dir + "/two\n" +
		"103 103 docker run --name studio-box --label studio.session=" + dir + "/three\n" +
		"104 104 claude --name agent-other --append-system-prompt-file " + dir + "/four/system-prompt.md\n"
	if got := parsePS(commands, 999, dir, markers); len(got) != 3 {
		t.Fatalf("custom markers found %d processes, want 3: %+v", len(got), got)
	}
	engineDir := fakeEngine(t, "container-id\t"+filepath.Join(dir, "three"))
	found, err := FromContainers(context.Background(), dir, markers)
	if err != nil || len(found) != 1 {
		t.Fatalf("custom label: %+v, %v", found, err)
	}
	if calls := strings.Join(listings(t, engineDir), "\n"); !strings.Contains(calls, "label=studio.session") {
		t.Fatalf("engine did not query caller label: %s", calls)
	}
}
