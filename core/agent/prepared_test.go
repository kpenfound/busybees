package agent

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent/agenttest"
)

type preparedSkills struct {
	sources []string
	dir     string
}

func (p *preparedSkills) Prepare(_ context.Context, sources []string) ([]string, error) {
	p.sources = slices.Clone(sources)
	return []string{p.dir}, nil
}

func TestPreparedInputs(t *testing.T) {
	t.Setenv("EXPAND_ME", "must-not-replace-literal")
	dir := t.TempDir()
	skills := &preparedSkills{dir: t.TempDir()}
	r := Runner{Skills: skills, ClaudeBin: agenttest.Script(t, "claude", `printf '%s\n' "$@" > "$ARGS_FILE"
echo '{"type":"result","subtype":"success","result":"ok"}'`)}
	_, err := r.Run(context.Background(), Request{SessionDir: dir, WorkDir: dir,
		Env: map[string]string{"ARGS_FILE": filepath.Join(dir, "args")},
		Profile: Profile{Skills: []string{"prepared-source"}, MCP: map[string]MCPEntry{
			"tools": {Command: "custom-server", Env: map[string]string{"LITERAL": "$EXPAND_ME"}, EnvVars: []string{"PRIVATE_TOKEN"}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(skills.sources, []string{"prepared-source"}) {
		t.Fatalf("skill sources: %v", skills.sources)
	}
	args, err := os.ReadFile(filepath.Join(dir, "args"))
	if err != nil || !strings.Contains(string(args), "--plugin-dir\n"+skills.dir+"\n") {
		t.Fatalf("prepared plugin not passed: %s, %v", args, err)
	}
	entry := readMCPConfig(t, dir)["tools"]
	if entry.Env["LITERAL"] != "$EXPAND_ME" {
		t.Fatalf("prepared MCP entry changed: %+v", entry)
	}
}
