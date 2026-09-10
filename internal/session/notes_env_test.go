package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
)

// TestTheNotesKeyVariableReachesTheSession: notes.neo4j_api_key may be a
// "$VAR" reference, and env strips every inherited BEES_* variable, so a
// BEES_-prefixed name would never reach the session — and the in-session
// `bees mcp serve` behind notes_read and notes_write loads [notes] again,
// where a reference that expands to nothing is an error. The name reaches
// the session with the value the scheduler resolved, only with the neo4j
// backend: on the file backend nothing reads it and the secret stays home.
func TestTheNotesKeyVariableReachesTheSession(t *testing.T) {
	const varName = "BEES_TEST_SESSION_NAMS_KEY"
	const secret = "nams_only_in_the_environment"
	t.Setenv(varName, secret)
	notes := config.Notes{Backend: config.NotesBackendNeo4j, Neo4jURL: "https://nams.example.com/v1", Neo4jAPIKey: "$" + varName}

	env := sessionEnvWithNotes(t, notes)
	if env[varName] != secret {
		t.Errorf("%s = %q, want the resolved key: without it a session cannot load [notes]", varName, env[varName])
	}

	// Load [notes] the way an in-session `bees mcp serve` does: with the
	// environment the session was handed and nothing else.
	path := filepath.Join(t.TempDir(), "bees.toml")
	toml := "version = 1\n[project]\nrepo = \"a/b\"\n[notes]\nbackend = \"neo4j\"\nneo4j_url = \"https://nams.example.com/v1\"\nneo4j_api_key = \"$" + varName + "\"\n"
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv(varName); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadNotes(path); err == nil {
		t.Fatal("[notes] loads with the variable unset: the check below could not fail")
	}
	for k, v := range env {
		if strings.HasPrefix(k, beesEnvPrefix) {
			t.Setenv(k, v)
		}
	}
	if n, err := config.LoadNotes(path); err != nil || n.ResolvedNeo4jAPIKey() != secret {
		t.Errorf("a session started with the neo4j backend cannot load [notes]: %+v %v", n, err)
	}

	// The file backend forwards nothing, whatever the key says.
	t.Setenv(varName, secret)
	notes.Backend = config.NotesBackendFile
	if env := sessionEnvWithNotes(t, notes); env[varName] != "" {
		t.Errorf("file backend: %s = %q reached the session", varName, env[varName])
	}
}

// sessionEnvWithNotes runs a fake claude that dumps its environment, with
// [notes] set on the runner, and returns what it saw.
func sessionEnvWithNotes(t *testing.T, notes config.Notes) map[string]string {
	t.Helper()
	dump := filepath.Join(t.TempDir(), "env.txt")
	bin := fakeClaude(t, `
env > `+dump+`
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	r.Notes = notes
	if _, err := r.Run(context.Background(), Request{
		Name: "t", Role: config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute},
		WorkDir: t.TempDir(),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			out[k] = v
		}
	}
	return out
}
