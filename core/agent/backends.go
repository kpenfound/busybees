package agent

import (
	"errors"
	"fmt"
	"slices"
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
	// Restricted declares whether the backend can establish
	// RunRestricted's read-only floor, and whether a restricted turn can
	// continue a session it owns. The pointer is deliberate: nil means a
	// descriptor forgot to make the declaration, distinct from an explicit
	// unsupported declaration.
	Restricted *RestrictedCapabilities
	// WritableTools declares, for every placement (Placements), whether an
	// ordinary turn, one that may write, can be held to the built-in tools
	// its grants name when they do not grant ToolsAll. A placement the map
	// does not name is unsupported: the turn is refused with
	// ErrUnsupported before anything starts, and so is one declared
	// unsupported, whose Remedy the error carries. A test holds every
	// descriptor to a declaration for every placement.
	WritableTools map[Placement]ToolSupport
	// bin is the runner's executable override for this backend
	// (Runner.ClaudeBin and friends): what a caller configured in the
	// executable's place, empty when it did not. executable resolves it,
	// with Name as the default.
	bin func(*Runner) string
	// impl builds the command line and reads the CLI's stream: the
	// backend implementation (backend.go, pi.go).
	impl backend
}

// RestrictedCapabilities are the parts of restricted execution that differ
// by backend. Supported says the command builder can establish the complete
// floor. FollowUp says ResumeID is meaningful for that backend; a resume id is
// never handed to a different backend during fallback.
type RestrictedCapabilities struct {
	Supported bool
	FollowUp  bool
	// ReadServer says the backend has no read-only file tool of its own
	// once its command execution is switched off, so a restricted turn is
	// given the runner's read server (readserver.go) to read the
	// workspace with instead.
	ReadServer bool
}

// A Placement is where a turn runs: its sandbox (one of Sandboxes, never
// empty), and for a host sandbox whether the operating system confines it
// (Profile.Confine).
type Placement struct {
	Sandbox string
	Confine bool
}

// Placements are every placement a valid profile selects.
var Placements = []Placement{
	{Sandbox: SandboxNone}, {Sandbox: SandboxNone, Confine: true},
	{Sandbox: SandboxClaude}, {Sandbox: SandboxClaude, Confine: true},
	{Sandbox: SandboxContainer}, {Sandbox: SandboxSbx},
}

// PlacementOf is where a profile's turn runs.
func PlacementOf(p Profile) Placement {
	sandbox := p.Sandbox
	if sandbox == "" {
		sandbox = SandboxNone
	}
	return Placement{Sandbox: sandbox, Confine: p.Confine}
}

func (p Placement) String() string {
	if p.Confine {
		return strconv.Quote(p.Sandbox) + " confined"
	}
	return strconv.Quote(p.Sandbox)
}

// ToolSupport is one backend's declaration for one placement: whether a
// writable turn there is held to its granted built-in tools, and for one
// that is not, what the caller can change instead.
type ToolSupport struct {
	Supported bool
	Remedy    string
}

// supportEverywhere and supportNowhere declare every placement alike.
func supportEverywhere() map[Placement]ToolSupport {
	m := make(map[Placement]ToolSupport, len(Placements))
	for _, p := range Placements {
		m[p] = ToolSupport{Supported: true}
	}
	return m
}

func supportNowhere(remedy string) map[Placement]ToolSupport {
	m := make(map[Placement]ToolSupport, len(Placements))
	for _, p := range Placements {
		m[p] = ToolSupport{Remedy: remedy}
	}
	return m
}

// openCodeWritableTools is where opencode's granted agent (backend.go:
// opencodeBackend) is verified before launch: wherever the runner can run
// `opencode debug config` the way the turn itself runs, on the host,
// under the same confinement, in the same image or in the same sandbox.
// Claude Code's sandbox runs claude alone.
func openCodeWritableTools() map[Placement]ToolSupport {
	return supportOutsideClaudeBox(AgentOpenCode)
}

// codexWritableTools is where codex's granted configuration
// (codex_tools.go) is verified before launch: wherever the runner can run
// `codex features list` and `codex mcp list` the way the turn itself runs.
// Claude Code's sandbox runs claude alone.
func codexWritableTools() map[Placement]ToolSupport {
	return supportOutsideClaudeBox(AgentCodex)
}

// supportOutsideClaudeBox declares every placement supported but Claude
// Code's sandbox, confined or not, which runs claude alone.
func supportOutsideClaudeBox(agent string) map[Placement]ToolSupport {
	m := supportEverywhere()
	remedy := fmt.Sprintf("sandbox %q runs claude alone; run %s in sandbox %q, %q or %q", SandboxClaude, agent, SandboxNone, SandboxContainer, SandboxSbx)
	m[Placement{Sandbox: SandboxClaude}] = ToolSupport{Remedy: remedy}
	m[Placement{Sandbox: SandboxClaude, Confine: true}] = ToolSupport{Remedy: remedy}
	return m
}

// writableTools refuses a writable turn whose grants name built-in tools
// in a placement where its backend cannot hold it to them. A placement no
// valid profile selects is Profile.Validate's to refuse.
func writableTools(b Backend, p Profile) error {
	where := PlacementOf(p)
	if !slices.Contains(Placements, where) {
		return nil
	}
	support, ok := b.WritableTools[where]
	if ok && support.Supported {
		return nil
	}
	remedy := support.Remedy
	if remedy == "" {
		remedy = fmt.Sprintf("grant %q", ToolsAll)
	}
	return fmt.Errorf("%w: agent %q cannot hold a writable turn to its granted built-in tools in sandbox %s; %s", ErrUnsupported, b.Name, where, remedy)
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
		Restricted: &RestrictedCapabilities{Supported: true, FollowUp: true},
		// --tools holds every placement.
		WritableTools: supportEverywhere(),
		bin:           func(r *Runner) string { return r.ClaudeBin },
		impl:          claudeBackend{},
	},
	{
		Name:          AgentCodex,
		Credentials:   []string{"OPENAI_API_KEY", "CODEX_API_KEY"},
		ProviderEnv:   []string{"OPENAI_*", "CODEX_*"},
		ArgvMarker:    true,
		Restricted:    &RestrictedCapabilities{Supported: true, ReadServer: true},
		WritableTools: codexWritableTools(),
		bin:           func(r *Runner) string { return r.CodexBin },
		impl:          codexBackend{},
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
		Restricted:    &RestrictedCapabilities{Supported: true, FollowUp: true},
		WritableTools: openCodeWritableTools(),
		bin:           func(r *Runner) string { return r.OpenCodeBin },
		impl:          opencodeBackend{},
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
		Restricted:    &RestrictedCapabilities{Supported: true, FollowUp: true},
		WritableTools: supportNowhere(fmt.Sprintf("grant %q", ToolsAll)),
		bin:           func(r *Runner) string { return r.PiBin },
		impl:          piBackend{},
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
