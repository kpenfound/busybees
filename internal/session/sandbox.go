package session

import (
	"encoding/json"
	"slices"
)

// ClaudeSandboxDomains are the hosts a session in config.SandboxClaude may
// reach: GitHub, where the factory's work is, for `gh` and `git push` over
// https. Nothing else resolves inside the box, so a module proxy, a package
// registry or an ssh remote needs the project to widen the list through its
// own .claude/settings.json, which Claude Code merges with this one.
var ClaudeSandboxDomains = []string{"github.com", "*.github.com"}

// sandboxFile is the copy of the settings block kept in the session
// directory. Claude reads the block inline from --settings, not from this
// file: the session directory is writable from inside the box, and Claude
// Code applies an edit to a loaded settings file to the running session, so
// a file it read would be one the session could loosen. The copy is for
// people reading the directory afterwards.
const sandboxFile = "sandbox.json"

// claudeSettings is the settings block bees passes with --settings for a
// session in config.SandboxClaude. Every key is one Claude Code documents at
// https://code.claude.com/docs/en/sandboxing and was measured against Claude
// Code 2.1.263 on macOS; the comments say what each one buys.
type claudeSettings struct {
	Sandbox     claudeSandbox     `json:"sandbox"`
	Permissions claudePermissions `json:"permissions"`
}

type claudeSandbox struct {
	// Enabled turns the sandbox on for every Bash command and the processes
	// it starts. Writes are then allowed in the working directory (the
	// worktree), the directories passed with --add-dir (the state dir), the
	// session's own temp dir and, for a linked worktree, the repository's
	// shared .git directory; reads everywhere; the network only through
	// Claude Code's proxy.
	Enabled bool `json:"enabled"`
	// AutoAllowBashIfSandboxed runs a sandboxed command without a
	// permission prompt: the box, not a prompt, decides.
	AutoAllowBashIfSandboxed bool `json:"autoAllowBashIfSandboxed"`
	// AllowUnsandboxedCommands false takes away the retry outside the box
	// Claude Code otherwise offers when a command hits the boundary; the
	// key is written even though false is the zero value, because Claude
	// Code's default is true.
	AllowUnsandboxedCommands bool `json:"allowUnsandboxedCommands"`
	// FailIfUnavailable makes claude refuse to start when the sandbox
	// cannot be built (bubblewrap or socat missing on Linux) instead of
	// warning and running the session unboxed.
	FailIfUnavailable bool `json:"failIfUnavailable"`
	// EnableWeakerNetworkIsolation lets a sandboxed process on macOS reach
	// the system's trust daemon, which Go programs need to verify a TLS
	// certificate: without it `gh` fails every call with "x509: OSStatus
	// -26276" under Seatbelt. It is a macOS key, written only there, and it
	// is the one hole the box has that bees chose: Claude Code names the
	// daemon a possible exfiltration channel.
	EnableWeakerNetworkIsolation bool                 `json:"enableWeakerNetworkIsolation,omitempty"`
	Network                      claudeSandboxNetwork `json:"network"`
}

type claudeSandboxNetwork struct {
	// AllowedDomains is ClaudeSandboxDomains.
	AllowedDomains []string `json:"allowedDomains"`
	// StrictAllowlist refuses a host outside the list instead of asking:
	// with no one to answer, asking would refuse too, but refusing outright
	// is what the transcript then says.
	StrictAllowlist bool `json:"strictAllowlist"`
}

type claudePermissions struct {
	// Allow are the permission rules the session runs under in acceptEdits
	// mode with prompts denied (see Runner.Run): every Bash command, which
	// the box confines, so the shapes the permission layer would otherwise
	// hold for a person (an env-prefixed command, `git -c`, a `$VAR`) run;
	// Read anywhere, as the box itself lets a shell read anywhere;
	// WebFetch to the same domains the box allows; and every MCP server of
	// the session, which runs on the host outside the box. What is not
	// listed is refused: a file write outside the worktree and the state
	// dir, WebSearch, a fetch elsewhere.
	Allow []string `json:"allow"`
}

// claudeSandboxSettings renders the settings block for a session whose MCP
// servers are named in servers, on the operating system goos.
func claudeSandboxSettings(servers []string, goos string) ([]byte, error) {
	allow := []string{"Bash", "Read"}
	for _, d := range ClaudeSandboxDomains {
		allow = append(allow, "WebFetch(domain:"+d+")")
	}
	for _, s := range slices.Sorted(slices.Values(servers)) {
		allow = append(allow, "mcp__"+s)
	}
	return json.Marshal(claudeSettings{
		Sandbox: claudeSandbox{
			Enabled:                      true,
			AutoAllowBashIfSandboxed:     true,
			AllowUnsandboxedCommands:     false,
			FailIfUnavailable:            true,
			EnableWeakerNetworkIsolation: goos == "darwin",
			Network: claudeSandboxNetwork{
				AllowedDomains:  slices.Clone(ClaudeSandboxDomains),
				StrictAllowlist: true,
			},
		},
		Permissions: claudePermissions{Allow: allow},
	})
}
