package agent

import (
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent/procs"
)

// TestBackendsMatchProcsExecutables holds the descriptors against procs'
// independent executable list, both directions and in order: procs cannot
// import this package, so its list cannot derive from the descriptors, and
// the two drifting apart is what #818 was — an agent the process scan does
// not recognize is not found after a crash, and one with no descriptor has
// no command line to run.
func TestBackendsMatchProcsExecutables(t *testing.T) {
	var names []string
	for _, b := range Backends {
		names = append(names, b.Name)
	}
	if !slices.Equal(names, procs.AgentExecutables) {
		t.Errorf("backends %v do not match procs.AgentExecutables %v", names, procs.AgentExecutables)
	}
}

// TestBackendsAreDeclaredCompletely refuses a descriptor that leaves one of
// its facts to the zero value: adding a backend that silently omits its
// credential, its provider environment, its executable override or its
// implementation is the omission this list exists to make impossible.
func TestBackendsAreDeclaredCompletely(t *testing.T) {
	var seen []string
	for _, b := range Backends {
		if b.Name == "" {
			t.Error("a backend declares no name")
		}
		if slices.Contains(seen, b.Name) {
			t.Errorf("backend %q is declared twice: %v", b.Name, seen)
		}
		seen = append(seen, b.Name)
		if len(b.Credentials) == 0 {
			t.Errorf("backend %q declares no credential variables", b.Name)
		}
		if len(b.ProviderEnv) == 0 {
			t.Errorf("backend %q declares no provider environment", b.Name)
		}
		if b.impl == nil {
			t.Errorf("backend %q declares no command-building implementation", b.Name)
		}
		if b.bin == nil {
			t.Errorf("backend %q declares no runner executable field", b.Name)
		}
	}
	if len(seen) == 0 {
		t.Fatal("no backend is declared")
	}
}

// TestBackendCredentials pins the credentials every agent reads, as the
// descriptors declare them and as AgentCredentials forwards them into an
// isolated container. #819 shipped without opencode's, so its list is the
// one whose loss this test exists to catch.
func TestBackendCredentials(t *testing.T) {
	want := map[string][]string{
		AgentClaude:   {"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
		AgentCodex:    {"OPENAI_API_KEY", "CODEX_API_KEY"},
		AgentOpenCode: {"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY", "OPENROUTER_API_KEY", "GROQ_API_KEY", "MISTRAL_API_KEY", "XAI_API_KEY", "DEEPSEEK_API_KEY"},
		AgentPi:       {"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "OPENAI_API_KEY", "GEMINI_API_KEY", "OPENROUTER_API_KEY", "GROQ_API_KEY", "XAI_API_KEY", "MISTRAL_API_KEY", "DEEPSEEK_API_KEY", "CEREBRAS_API_KEY"},
	}
	if len(AgentCredentials) != len(Backends) {
		t.Errorf("AgentCredentials holds %d agents, want the %d backends", len(AgentCredentials), len(Backends))
	}
	for _, b := range Backends {
		if !slices.Equal(AgentCredentials[b.Name], b.Credentials) {
			t.Errorf("AgentCredentials[%s] = %v, want the descriptor's %v", b.Name, AgentCredentials[b.Name], b.Credentials)
		}
		if !slices.Equal(b.Credentials, want[b.Name]) {
			t.Errorf("credentials of %s = %v, want %v", b.Name, b.Credentials, want[b.Name])
		}
	}
}

// TestBackendProviderEnv pins the provider environment every agent is
// configured through: each list is what a session of that agent inherits
// beyond the host environment (internal/session.ProviderEnv, granted to
// the session by the busybees adapter).
func TestBackendProviderEnv(t *testing.T) {
	want := map[string][]string{
		AgentClaude:   {"ANTHROPIC_*", "CLAUDE_*", "AWS_*", "GOOGLE_*", "CLOUD_ML_REGION", "VERTEX_*", "DISABLE_*", "MAX_THINKING_TOKENS", "MCP_*"},
		AgentCodex:    {"OPENAI_*", "CODEX_*"},
		AgentOpenCode: {"OPENCODE_*", "ANTHROPIC_*", "OPENAI_*", "GEMINI_*", "GOOGLE_*", "AWS_*", "OPENROUTER_*", "GROQ_*", "MISTRAL_*", "XAI_*", "DEEPSEEK_*", "AZURE_*"},
		AgentPi:       {"PI_*", "ANTHROPIC_*", "OPENAI_*", "GEMINI_*", "GOOGLE_*", "AWS_*", "OPENROUTER_*", "GROQ_*", "MISTRAL_*", "XAI_*", "DEEPSEEK_*", "AZURE_*", "CEREBRAS_*", "MCP_*"},
	}
	for _, b := range Backends {
		if !slices.Equal(b.ProviderEnv, want[b.Name]) {
			t.Errorf("provider environment of %s = %v, want %v", b.Name, b.ProviderEnv, want[b.Name])
		}
	}
}

// TestBackendArgvMarkers pins which backends build a command line procs can
// find a session by: claude's --name and codex's session-directory override
// carry one, and opencode and pi carry none, so a session of either is found
// through its pid file alone (procs' package comment says why). A backend
// flipped here without procs learning a marker for it is a session the
// process scan mistakes for a stranger's.
func TestBackendArgvMarkers(t *testing.T) {
	want := map[string]bool{AgentClaude: true, AgentCodex: true, AgentOpenCode: false, AgentPi: false}
	for _, b := range Backends {
		if b.ArgvMarker != want[b.Name] {
			t.Errorf("argv marker of %s = %v, want %v", b.Name, b.ArgvMarker, want[b.Name])
		}
	}
}

// TestBackendFor pins the selection backendFor performs: an empty agent
// setting selects claude, every agent Agents accepts has a descriptor, and
// anything else is refused by name.
func TestBackendFor(t *testing.T) {
	for agent, want := range map[string]string{"": AgentClaude, AgentClaude: AgentClaude, AgentCodex: AgentCodex, AgentOpenCode: AgentOpenCode, AgentPi: AgentPi} {
		b, err := backendFor(agent)
		if err != nil {
			t.Fatalf("backendFor(%q): %v", agent, err)
		}
		if b.Name != want {
			t.Errorf("backendFor(%q) selected %q, want %q", agent, b.Name, want)
		}
		if b.impl == nil {
			t.Errorf("backendFor(%q) returned a backend with no implementation", agent)
		}
	}
	if _, err := backendFor("not-an-agent"); err == nil || !strings.Contains(err.Error(), `"not-an-agent"`) {
		t.Errorf("unknown agent: %v, want it named in the error", err)
	}
}

// TestBackendExecutable pins executable selection: a backend's name is the
// command a session of it runs, and the runner's configured executable for
// it replaces the name. Either half lost leaves the session running the
// wrong CLI or none.
func TestBackendExecutable(t *testing.T) {
	overrides := map[string]string{
		AgentClaude:   "/overridden/claude",
		AgentCodex:    "/overridden/codex",
		AgentOpenCode: "/overridden/opencode",
		AgentPi:       "/overridden/pi",
	}
	r := &Runner{ClaudeBin: overrides[AgentClaude], CodexBin: overrides[AgentCodex], OpenCodeBin: overrides[AgentOpenCode], PiBin: overrides[AgentPi]}
	for _, b := range Backends {
		if got := b.executable(&Runner{}); got != b.Name {
			t.Errorf("executable of %s with nothing configured = %q, want %q", b.Name, got, b.Name)
		}
		if got := b.executable(r); got != overrides[b.Name] {
			t.Errorf("executable of %s with its runner field set = %q, want %q", b.Name, got, overrides[b.Name])
		}
	}
}
