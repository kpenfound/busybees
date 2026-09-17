// Package session adapts busybees configuration and identity to core/agent.
package session

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/procs"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/skills"
)

// Environment variable names exported to every session.
const (
	EnvRole       = "BEES_ROLE"
	EnvSessionDir = "BEES_SESSION_DIR"
	EnvStateDir   = "BEES_STATE_DIR"
	EnvRepo       = "BEES_REPO"
	EnvLabel      = "BEES_LABEL"
	EnvIssue      = "BEES_ISSUE"
	EnvPR         = "BEES_PR"
	EnvBranch     = "BEES_BRANCH"
	EnvConfig     = "BEES_CONFIG"
	EnvBin        = "BEES_BIN"
)

// beesEnvPrefix marks the variables bees owns: they are set per session and
// never inherited from the process that started the scheduler.
const beesEnvPrefix = "BEES_"

// EnvGHToken is the variable gh reads its credentials from. It is not one of
// bees' own BEES_* variables — it is gh's, set for a session only when
// [github] configures a token — so it survives the BEES_ strip in env.
const EnvGHToken = config.EnvGHToken

// EnvMCPToken carries the bearer token `bees mcp serve --listen` requires
// of its client, for a container session's built-in server. It is set on
// the host server and container session environments; backend configuration
// refers to this variable without recording the token value.
const EnvMCPToken = "BEES_MCP_TOKEN"

// MCPListening is the prefix of the line `bees mcp serve --listen` prints
// once it listens, followed by the address.
const MCPListening = "listening on "

type Request = agent.Request
type Profile = agent.Profile
type Result = agent.Result
type RateLimit = agent.RateLimit
type MCPEntry = agent.MCPEntry
type Interrupted = agent.Interrupted

const ResultFile = agent.ResultFile
const TranscriptFile = agent.TranscriptFile
const InterruptedFile = agent.InterruptedFile

var CheckInterrupted = agent.CheckInterrupted
var MarkInterrupted = agent.MarkInterrupted
var CountTurns = agent.CountTurns
var WriteMCPConfig = agent.WriteMCPConfig

var ClaudeSandboxDomains = []string{"github.com", "*.github.com"}
var ProcessMarkers = procs.Markers{Session: "--name bees-", Codex: procs.CodexMarker(beesEnvPrefix), LegacyCodex: "mcp_servers.bees.env.BEES_SESSION_DIR=", Container: "bees.session"}

// ProfileForRole strips workflow settings after size and fallback selection.
func ProfileForRole(role config.ResolvedRole) Profile {
	return Profile{
		Name: role.Name, Agent: role.Agent, Model: role.Model, FallbackModel: role.FallbackModel,
		Effort: role.Effort, MaxTurns: role.MaxTurns, Timeout: role.Timeout,
		AllowedTools: slices.Clone(role.AllowedTools), DisallowedTools: slices.Clone(role.DisallowedTools),
		MCP: MCPEntries(role.MCP), Sandbox: role.Sandbox, SandboxImage: role.SandboxImage,
		SandboxDomains: slices.Clone(ClaudeSandboxDomains), VCSAccess: true,
		ContainerUseEnvironment: role.ContainerUseEnvironment, Shell: role.Shell,
		Env: maps.Clone(role.Env), Skills: slices.Clone(role.Skills),
	}
}

func MCPEntries(servers map[string]config.MCPServer) map[string]MCPEntry {
	entries := make(map[string]MCPEntry, len(servers))
	for name, s := range servers {
		entries[name] = MCPEntry{Type: s.Type, Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers}
	}
	return agent.MCPEntries(entries)
}

// Runner executes sessions.
type Runner struct {
	// ClaudeBin is the claude executable. Default "claude".
	ClaudeBin string
	// CodexBin is the codex executable, run for a role whose agent is
	// codex. Default "codex".
	CodexBin string
	// OpenCodeBin is the opencode executable, run for a role whose agent
	// is opencode. Default "opencode".
	OpenCodeBin string
	// DockerBin is the container engine a container session is run with.
	// Default config.ContainerEngine.
	DockerBin string
	// ContainerListen is the address the built-in MCP server listens on
	// for a container session. Empty picks the address the container
	// reaches the host by (see containerListen).
	ContainerListen string
	// BeesBin is the path of the bees executable, made available on PATH so
	// sessions can run `bees mail` and `bees done`.
	BeesBin string
	// SessionsDir is where per-session directories are created.
	SessionsDir string
	// StateDir, ConfigPath, Repo and Label are exported to sessions.
	StateDir   string
	ConfigPath string
	Repo       string
	Label      string
	// GitHub is the identity a session acts as: the token its `gh` calls and
	// its pushes use, and the name and email its commits carry. The zero
	// value means the machine's own gh authentication and git identity,
	// which is what every configuration without [github] gets.
	GitHub config.GitHub
	// Notes is [notes] from bees.toml: with the neo4j backend the variable
	// notes.neo4j_api_key reads has to reach the session, as github.token's
	// does (see sessionVars).
	Notes config.Notes
	// Skills prepares skill plugin dirs. Optional.
	Skills *skills.Manager
	// AddDirs are extra directories claude may access (the state dir).
	// Codex, which runs without a sandbox, needs no such list, and neither
	// does opencode, whose --auto approves writing outside the worktree.
	AddDirs []string
	// Stream, when set, receives every stream-json line (debug output).
	Stream io.Writer
	Logger *slog.Logger
}

// Run supplies factory context and delegates execution to the standalone runner.
func (r *Runner) Run(ctx context.Context, req Request) (*Result, error) {
	p := req.Profile
	if err := config.CheckSandboxMode(p.Sandbox); err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}
	if err := config.CheckSandboxAgent(p.Sandbox, p.Agent); err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}
	if err := config.CheckSandboxContainer(config.ResolvedRole{Agent: p.Agent, Sandbox: p.Sandbox, SandboxImage: p.SandboxImage, ContainerUseEnvironment: p.ContainerUseEnvironment, Env: p.Env}, r.GitHub); err != nil {
		return nil, fmt.Errorf("%s: %w", p.Name, err)
	}
	core := r.coreRunner()
	if req.SessionDir == "" {
		dir, err := core.NewSessionDir(req.Name)
		if err != nil {
			return nil, err
		}
		req.SessionDir = dir
	}
	req = r.prepare(req, req.SessionDir)
	return core.Run(ctx, req)
}

func (r *Runner) coreRunner() *agent.Runner {
	var skillDirs []string
	var preparer agent.SkillPreparer
	if r.Skills != nil {
		skillDirs = []string{r.Skills.CacheDir}
		preparer = r.Skills
	}
	return &agent.Runner{
		ClaudeBin: r.ClaudeBin, CodexBin: r.CodexBin, OpenCodeBin: r.OpenCodeBin, DockerBin: r.DockerBin,
		ContainerListen: r.ContainerListen, SessionsDir: r.SessionsDir, Skills: preparer, SkillMountDirs: skillDirs,
		EnvironmentPrefix: beesEnvPrefix, NamePrefix: "bees-", ContainerLabel: ProcessMarkers.Container,
		ContainerHome: "/home/bees", ContainerUseRepository: "bees-container-use",
		MountDirs: nonempty(r.StateDir), AddDirs: r.AddDirs, Stream: r.Stream, Logger: r.Logger,
	}
}
func nonempty(s string) []string {
	if s == "" {
		return nil
	}
	return []string{s}
}
func (r *Runner) NewSessionDir(name string) (string, error) {
	return r.coreRunner().NewSessionDir(name)
}

func (r *Runner) prepare(req Request, dir string) Request {
	builtin := r.builtinMCP(req, dir)
	req.Profile.MCP = maps.Clone(req.Profile.MCP)
	if req.Profile.MCP == nil {
		req.Profile.MCP = map[string]MCPEntry{}
	}
	req.Profile.MCP[config.BuiltinMCPServer] = builtin
	host, container := map[string]string{}, map[string]string{}
	for _, v := range r.sessionVars(req, dir) {
		host[v.name] = v.value
		if v.name != EnvBin {
			container[v.name] = v.value
		}
	}
	if r.BeesBin != "" {
		host["PATH"] = filepath.Dir(r.BeesBin) + string(os.PathListSeparator) + os.Getenv("PATH")
	}
	vcsHost, vcsContainer := map[string]string{}, map[string]string{}
	for _, v := range r.vcsVars(req) {
		vcsHost[v.name] = v.value
		vcsContainer[v.name] = v.value
	}
	if os.Getenv("GIT_CONFIG_COUNT") == "" {
		for _, v := range gitConfigVars(r.gitConfig()) {
			vcsHost[v.name] = v.value
		}
	}
	for _, v := range gitConfigVars(append(r.gitConfig(), containerGitConfig...)) {
		vcsContainer[v.name] = v.value
	}
	req.VCSEnv = vcsHost
	req.VCSContainerEnv = vcsContainer
	req.Env = host
	if req.Profile.Sandbox == agent.SandboxContainer {
		req.Env = container
	}
	req.ContainerEnv = container
	req.HostMCP = &agent.HostMCP{Name: config.BuiltinMCPServer, Entry: builtin, ListenArgs: []string{"--listen"}, TokenEnv: EnvMCPToken, ListeningPrefix: MCPListening, Path: "/mcp"}
	// The server needs the host environment, including BEES_BIN and PATH.
	req.HostMCP.Env = host
	req.ValidOutcomes = ValidOutcomes(req.Profile.Name)
	req.Grants = r.grants(req)
	return req
}

// HostEnv are the host variables every session inherits: the shell's own,
// locale, proxies and certificates, and the toolchains a role's build and
// test commands run. A trailing "*" is a prefix. Anything else of the host
// environment reaches a session only through its role's env.
var HostEnv = []string{
	"PATH", "HOME", "USER", "LOGNAME", "SHELL", "TMPDIR", "TMP", "TEMP", "TERM", "COLORTERM",
	"LANG", "LANGUAGE", "LC_*", "TZ", "XDG_*", "__CF_USER_TEXT_ENCODING",
	"http_proxy", "https_proxy", "no_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "ALL_PROXY",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE",
	"GO*", "CGO_*", "DOCKER_*", "DAGGER_*", "NODE_*", "NPM_CONFIG_*", "NVM_*", "CARGO_*", "RUSTUP_*",
	"JAVA_HOME", "PYTHON*", "VIRTUAL_ENV", "HOMEBREW_*", "SDKROOT", "DEVELOPER_DIR",
}

// ProviderEnv are the variables each agent is configured and authenticated
// through: its provider's credentials and settings.
var ProviderEnv = map[string][]string{
	agent.AgentClaude:   {"ANTHROPIC_*", "CLAUDE_*", "AWS_*", "GOOGLE_*", "CLOUD_ML_REGION", "VERTEX_*", "DISABLE_*", "MAX_THINKING_TOKENS", "MCP_*"},
	agent.AgentCodex:    {"OPENAI_*", "CODEX_*"},
	agent.AgentOpenCode: {"OPENCODE_*", "ANTHROPIC_*", "OPENAI_*", "GEMINI_*", "GOOGLE_*", "AWS_*", "OPENROUTER_*", "GROQ_*", "MISTRAL_*", "XAI_*", "DEEPSEEK_*", "AZURE_*"},
}

// VCSEnv are the host variables a session with VCS access inherits: gh's
// and git's credentials and configuration and the SSH agent.
var VCSEnv = []string{"GH_*", "GITHUB_*", "GIT_*", "GCM_*", "SSH_AUTH_SOCK", "SSH_AGENT_PID", "SSH_ASKPASS", "SSH_ASKPASS_REQUIRE"}

// grants turns busybees policy into the session's complete capabilities:
// the variables it inherits and is given, every built-in tool and the MCP
// servers its role configures, and the filesystem its sandbox reaches.
func (r *Runner) grants(req Request) *agent.Grants {
	p := req.Profile
	backend := p.Agent
	if backend == "" {
		backend = agent.AgentClaude
	}
	g := &agent.Grants{VCS: p.VCSAccess, Tools: []string{agent.ToolsAll}}
	g.Env = append(slices.Clone(HostEnv), ProviderEnv[backend]...)
	g.Env = append(g.Env, beesEnvPrefix+"*")
	if p.VCSAccess {
		g.Env = append(g.Env, VCSEnv...)
		g.Env = append(g.Env, slices.Collect(maps.Keys(req.VCSEnv))...)
	}
	g.Env = append(g.Env, slices.Collect(maps.Keys(p.Env))...)
	g.Env = append(g.Env, slices.Collect(maps.Keys(req.Env))...)
	servers := map[string]bool{}
	for name, entry := range p.MCP {
		servers[name] = true
		g.Env = append(g.Env, entry.EnvVars...)
	}
	// An allowed tool may name a server a skill's plugin brings, which the
	// role's mcp table does not list.
	for _, t := range p.AllowedTools {
		if rest, ok := strings.CutPrefix(t, "mcp__"); ok {
			if server, _, _ := strings.Cut(rest, "__"); server != "" {
				servers[strings.SplitN(server, "(", 2)[0]] = true
			}
		}
	}
	for _, s := range slices.Sorted(maps.Keys(servers)) {
		g.Tools = append(g.Tools, "mcp__"+s)
	}
	slices.Sort(g.Env)
	g.Env = slices.Compact(g.Env)
	dir := ""
	if req.Workspace != nil {
		dir = req.Workspace.Directory()
	}
	switch p.Sandbox {
	case agent.SandboxClaude:
		g.Mounts = []agent.Mount{{Path: "/", Access: agent.ReadOnly}, {Path: dir, Access: agent.ReadWrite}}
		for _, d := range r.AddDirs {
			g.Mounts = append(g.Mounts, agent.Mount{Path: d, Access: agent.ReadWrite})
		}
	case agent.SandboxContainer:
		g.Mounts = []agent.Mount{{Path: dir, Access: agent.ReadWrite}}
		if r.StateDir != "" {
			g.Mounts = append(g.Mounts, agent.Mount{Path: r.StateDir, Access: agent.ReadWrite})
		}
	default:
		// No sandbox: the session reaches whatever its user can.
		g.Mounts = []agent.Mount{{Path: "/", Access: agent.ReadWrite}}
	}
	return g
}

var containerGitConfig = []envVar{
	{"safe.directory", "*"},
	{"url.https://github.com/.insteadOf", "git@github.com:"},
	{"url.https://github.com/.insteadOf", "ssh://git@github.com/"},
}

// beesEnv returns the BEES_* variables that describe the session, in a stable
// order. They go both into claude's environment and, explicitly, into the
// built-in MCP server's entry in mcp.json — which is written to the session
// directory, so nothing secret may join them. The variable github.token reads
// is set in env alone for that reason.
func (r *Runner) beesEnv(req Request, sessionDir string) []envVar {
	vars := []envVar{
		{EnvRole, req.Profile.Name},
		{EnvSessionDir, sessionDir},
		{EnvStateDir, r.StateDir},
		{EnvConfig, r.ConfigPath},
		{EnvRepo, r.Repo},
		{EnvLabel, r.Label},
	}
	if r.BeesBin != "" {
		vars = append(vars, envVar{EnvBin, r.BeesBin})
	}
	// The per-request variables (issue, PR, branch) are set by
	// the scheduler; only the BEES_* ones describe the session.
	for _, k := range slices.Sorted(maps.Keys(req.Env)) {
		if strings.HasPrefix(k, beesEnvPrefix) {
			vars = append(vars, envVar{k, req.Env[k]})
		}
	}
	return vars
}

// envVar is one name/value pair.
type envVar struct{ name, value string }

// builtinMCP describes the built-in bees MCP server: this very binary, run as
// `bees mcp serve`, with the session's context passed explicitly.
func (r *Runner) builtinMCP(req Request, sessionDir string) MCPEntry {
	bin := r.beesBin()
	env := map[string]string{}
	for _, v := range r.beesEnv(req, sessionDir) {
		if v.value != "" {
			env[v.name] = v.value
		}
	}
	// Codex filters its child servers' environment. Forward credential names
	// explicitly, without putting their values in mcp.json or command args.
	var envVars []string
	if req.Profile.VCSAccess {
		envVars = []string{EnvGHToken, "GITHUB_TOKEN", "GH_ENTERPRISE_TOKEN", "GITHUB_ENTERPRISE_TOKEN"}
		if v := r.GitHub.TokenVar(); v != "" {
			envVars = append(envVars, v)
		}
	}
	if r.Notes.Backend == config.NotesBackendNeo4j {
		if v := r.Notes.Neo4jAPIKeyVar(); v != "" {
			envVars = append(envVars, v)
		}
	}
	slices.Sort(envVars)
	return MCPEntry{Type: "stdio", Command: bin, Args: []string{"mcp", "serve"}, Env: env, EnvVars: slices.Compact(envVars)}
}

// sessionVars are the variables bees sets for a session, wherever it runs,
// in the order they are set: the role's configured environment first, so
// bees' own variables win, then the shell, the BEES_* variables, the
// caller's request context. VCS identity is supplied separately by vcsVars.
func (r *Runner) sessionVars(req Request, sessionDir string) []envVar {
	var vars []envVar
	set := func(k, v string) { vars = append(vars, envVar{k, v}) }
	for _, k := range slices.Sorted(maps.Keys(req.Profile.Env)) {
		set(k, os.ExpandEnv(req.Profile.Env[k]))
	}
	if req.Profile.Shell != "" {
		set("SHELL", req.Profile.Shell)
	}
	vars = append(vars, r.beesEnv(req, sessionDir)...)
	// notes.neo4j_api_key may be a $VAR reference too, and the same strip
	// would leave a session whose notes_read and notes_write cannot load
	// [notes]: put the name back the same way, with the value the scheduler
	// resolved, and only when the backend reads it.
	if r.Notes.Backend == config.NotesBackendNeo4j {
		if v, key := r.Notes.Neo4jAPIKeyVar(), r.Notes.ResolvedNeo4jAPIKey(); v != "" && key != "" {
			set(v, key)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(req.Env)) {
		set(k, req.Env[k])
	}
	return vars
}

// vcsVars are supplied separately so core gates identity with VCSAccess.
func (r *Runner) vcsVars(req Request) []envVar {
	var vars []envVar
	set := func(k, v string) { vars = append(vars, envVar{k, v}) }
	// The factory's own GitHub identity, so a session's gh, pushes and
	// commits are the bot's rather than the machine owner's. It sits with
	// bees' own variables, after req.Profile.Env, so a role cannot configure a
	// second identity for itself.
	if token := r.GitHub.ResolvedToken(); token != "" {
		set(EnvGHToken, token)
		// github.token may be a $VAR reference, and when the operator named a
		// BEES_* variable the strip above drops it — leaving a session whose
		// gh works but whose own `bees` commands cannot load bees.toml at all,
		// because a reference that expands to nothing is a load error. Put the
		// name back, holding the value the scheduler resolved, so no session
		// can be handed a token from a stale environment. It is deliberately
		// not one of beesEnv's variables: those are written into mcp.json in
		// the session directory, and the secret must not reach disk. claude
		// passes its own environment on to the MCP server it starts. Codex
		// filters that environment, so builtinMCP forwards the credential
		// names through its env_vars setting.
		if v := r.GitHub.TokenVar(); v != "" {
			set(v, token)
		}
	}
	vars = append(vars, r.gitIdentity()...)
	// Explicit per-request values retain their precedence over adapter defaults.
	for i, v := range vars {
		if value, ok := req.Env[v.name]; ok {
			vars[i].value = value
		}
	}
	return vars
}

// gitConfigVars numbers git configuration entries into the GIT_CONFIG_KEY_n
// and GIT_CONFIG_VALUE_n variables git reads. GIT_CONFIG_COUNT is derived
// from the entries and never written by hand: a count that is one short
// silently drops the last entry.
func gitConfigVars(entries []envVar) []envVar {
	var vars []envVar
	for i, e := range entries {
		n := strconv.Itoa(i)
		vars = append(vars, envVar{"GIT_CONFIG_KEY_" + n, e.name}, envVar{"GIT_CONFIG_VALUE_" + n, e.value})
	}
	return append(vars, envVar{"GIT_CONFIG_COUNT", strconv.Itoa(len(entries))})
}

// gitIdentity is the author and committer a session's commits carry. The two
// keys are independent: config accepts git_name or git_email on its own, and
// whichever is unset stays the machine's, because a half-set identity is
// still better than a wrong one.
func (r *Runner) gitIdentity() []envVar {
	var vars []envVar
	if n := r.GitHub.GitName; n != "" {
		vars = append(vars, envVar{"GIT_AUTHOR_NAME", n}, envVar{"GIT_COMMITTER_NAME", n})
	}
	if e := r.GitHub.GitEmail; e != "" {
		vars = append(vars, envVar{"GIT_AUTHOR_EMAIL", e}, envVar{"GIT_COMMITTER_EMAIL", e})
	}
	return vars
}

// gitConfig is the git configuration a session runs with, as the key/value
// pairs env numbers into GIT_CONFIG_KEY_n and GIT_CONFIG_VALUE_n.
func (r *Runner) gitConfig() []envVar {
	entries := []envVar{
		{"push.autoSetupRemote", "true"},
		{"push.default", "current"},
	}
	if r.GitHub.ResolvedToken() != "" {
		// Push over https as the bot, without reading or writing the
		// person's stored credentials. The empty value is load-bearing:
		// git asks credential helpers in configuration order and takes the
		// first answer, and GIT_CONFIG_* entries are read last, so without
		// it the machine owner's helper (a keychain, or their own gh)
		// answers first and the push is theirs. An empty credential.helper
		// resets the list built so far.
		entries = append(entries,
			envVar{"credential.helper", ""},
			envVar{"credential.helper", "!gh auth git-credential"},
		)
	}
	return entries
}

func (r *Runner) beesBin() string {
	if r.BeesBin != "" {
		return r.BeesBin
	}
	if self, err := os.Executable(); err == nil {
		return self
	}
	return "bees"
}

const OpenCodeConfigFile = agent.OpenCodeConfigFile
const EnvOpenCodeConfig = agent.EnvOpenCodeConfig
