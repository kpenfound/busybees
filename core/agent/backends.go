package agent

import (
	"errors"
	"strconv"
)

// A Backend is one agent CLI a session can run as, described once: the
// per-agent facts every consumer reads — the executable a session runs,
// the variables that hold its credential, the environment that
// configures it, whether the command line carries a marker the
// process-table scan finds the session by — together with the
// implementation that builds that command line and reads what the CLI
// printed. The lists these facts used to be repeated in derive from the
// descriptors: AgentCredentials below, internal/session.ProviderEnv in
// the busybees adapter, the executable every backend defaults to, and
// the backend backendFor selects. procs' executable list is the one that
// cannot derive, because procs cannot import this package: it stays the
// independent source, and a test holds the two together.
type Backend struct {
	// Name is the backend's identity everywhere: the value of the agent
	// setting that selects it, and the basename of the executable a
	// session of it runs. The two are one fact because Agents is procs'
	// list of executable basenames and an agent setting is one of its
	// entries.
	Name string
	// Credentials are the variables the CLI reads its provider
	// credential from. opencode and pi are authenticated through
	// whichever provider they are configured for, so their lists hold
	// one key per provider they read: any one of them is a credential,
	// and every one of them that is set is forwarded.
	Credentials []string
	// ProviderEnv are the variables the CLI is configured and
	// authenticated through, beyond the host environment every session
	// inherits: its providers' credentials and its own settings. A
	// trailing "*" is a prefix.
	ProviderEnv []string
	// ArgvMarker says whether the command line the backend's
	// implementation builds carries a marker procs finds the session by
	// in the process table. A backend without one is found through its
	// pid file alone; procs' package comment says why.
	ArgvMarker bool
	// bin is the runner's executable override for this backend
	// (Runner.ClaudeBin and friends): what a caller configured in the
	// executable's place, empty when it did not. executable resolves it,
	// with Name as the default.
	bin func(*Runner) string
	// impl builds the command line and reads the CLI's stream: the
	// backend implementation (backend.go, pi.go).
	impl backend
}

// executable is the command a session of this backend runs: the
// executable the runner was configured with for it, or the backend's own
// name.
func (b Backend) executable(r *Runner) string {
	if b.bin != nil {
		if bin := b.bin(r); bin != "" {
			return bin
		}
	}
	return b.Name
}

// Backends are the backends the runner builds a command line for, one
// descriptor each, in the order Agents lists them. Adding a backend here
// is what gives it credentials, provider environment and executable
// selection everywhere they are read; procs.AgentExecutables still has to
// name its executable, and doctor still has to carry its check, either of
// which a test reports missing.
var Backends = []Backend{
	{
		Name:        AgentClaude,
		Credentials: []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"},
		ProviderEnv: []string{
			"ANTHROPIC_*", "CLAUDE_*", "AWS_*", "GOOGLE_*", "CLOUD_ML_REGION",
			"VERTEX_*", "DISABLE_*", "MAX_THINKING_TOKENS", "MCP_*",
		},
		ArgvMarker: true,
		bin:        func(r *Runner) string { return r.ClaudeBin },
		impl:       claudeBackend{},
	},
	{
		Name:        AgentCodex,
		Credentials: []string{"OPENAI_API_KEY", "CODEX_API_KEY"},
		ProviderEnv: []string{"OPENAI_*", "CODEX_*"},
		ArgvMarker:  true,
		bin:         func(r *Runner) string { return r.CodexBin },
		impl:        codexBackend{},
	},
	{
		Name: AgentOpenCode,
		Credentials: []string{
			"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "GEMINI_API_KEY", "GOOGLE_API_KEY",
			"OPENROUTER_API_KEY", "GROQ_API_KEY", "MISTRAL_API_KEY", "XAI_API_KEY",
			"DEEPSEEK_API_KEY",
		},
		ProviderEnv: []string{
			"OPENCODE_*", "ANTHROPIC_*", "OPENAI_*", "GEMINI_*", "GOOGLE_*", "AWS_*",
			"OPENROUTER_*", "GROQ_*", "MISTRAL_*", "XAI_*", "DEEPSEEK_*", "AZURE_*",
		},
		bin:  func(r *Runner) string { return r.OpenCodeBin },
		impl: opencodeBackend{},
	},
	{
		Name: AgentPi,
		Credentials: []string{
			"ANTHROPIC_API_KEY", "ANTHROPIC_OAUTH_TOKEN", "OPENAI_API_KEY",
			"GEMINI_API_KEY", "OPENROUTER_API_KEY", "GROQ_API_KEY", "XAI_API_KEY",
			"MISTRAL_API_KEY", "DEEPSEEK_API_KEY", "CEREBRAS_API_KEY",
		},
		ProviderEnv: []string{
			"PI_*", "ANTHROPIC_*", "OPENAI_*", "GEMINI_*", "GOOGLE_*", "AWS_*",
			"OPENROUTER_*", "GROQ_*", "MISTRAL_*", "XAI_*", "DEEPSEEK_*", "AZURE_*",
			"CEREBRAS_*", "MCP_*",
		},
		bin:  func(r *Runner) string { return r.PiBin },
		impl: piBackend{},
	},
}

// backendFor selects the requested backend's descriptor. An empty name
// selects Claude, the default agent.
func backendFor(agent string) (Backend, error) {
	if agent == "" {
		agent = AgentClaude
	}
	for _, b := range Backends {
		if b.Name == agent {
			return b, nil
		}
	}
	return Backend{}, errors.New("session: unknown agent " + strconv.Quote(agent))
}

// AgentCredentials names credentials forwarded into an isolated container,
// per agent: the backends' Credentials indexed by name. opencode and pi
// are authenticated through whichever provider they are configured for,
// so their entries list one key per provider they read: any one of them
// is a credential, and every one of them that is set is forwarded.
var AgentCredentials = credentialsByAgent()

// credentialsByAgent indexes the backends' credential variables by agent
// name, so a backend added to Backends is known here without a second
// list to keep in step.
func credentialsByAgent() map[string][]string {
	m := make(map[string][]string, len(Backends))
	for _, b := range Backends {
		m[b.Name] = b.Credentials
	}
	return m
}
