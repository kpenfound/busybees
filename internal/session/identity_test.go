package session

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghapp"
	"github.com/kpenfound/busybees/internal/ghapp/ghapptest"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

// botIdentity is a fully configured [github] table.
var botIdentity = config.GitHub{
	Login: "busybees-bot", Token: "ghp_bot", GitName: "busybees", GitEmail: "bot@example.com",
}

// machineEnv is what the person running bees already has in their
// environment. Every identity variable is seeded with it so that "[github]
// unset changes nothing" can be asserted as "the machine's own
// value reached the session untouched", which is stronger — and steadier on
// a developer machine — than asserting the variable is absent.
const machineEnv = "machine-owner"

// sessionEnv runs one session with the given identity and returns the
// environment it was handed, last assignment winning as os/exec does.
func sessionEnv(t *testing.T, gh config.GitHub) map[string]string {
	t.Helper()
	env, _ := sessionEnvDir(t, gh)
	return env
}

// sessionEnvDir is sessionEnv plus the session directory, for the callers that
// have something to say about what was written there. The environment is
// dumped outside that directory on purpose: it holds every secret a session
// was given, and a test asserting nothing secret reached the session directory
// must not be reading its own scaffolding.
func sessionEnvDir(t *testing.T, gh config.GitHub) (map[string]string, string) {
	t.Helper()
	for _, k := range []string{EnvGHToken, "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, machineEnv)
	}
	// The GIT_CONFIG_* block defers to an operator who set the count
	// themselves; pin it empty so the test does not depend on the shell it
	// runs in. The numbered entries need the same treatment, and for a
	// sharper reason: this repository's own bees.toml sets [github], so a
	// suite run from inside a developer session inherits the four entries
	// that session was handed. The runner overwrites keys 0 and 1 itself,
	// but 2 and 3 would survive and gitConfigEntries would read the outer
	// factory's credential helper back as an injection. t.Setenv
	// registers the cleanup that restores the original value; the
	// os.Unsetenv after it makes the variable genuinely absent, which is
	// what gitConfigEntries' stop condition needs.
	t.Setenv("GIT_CONFIG_COUNT", "")
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(k, "GIT_CONFIG_KEY_") || strings.HasPrefix(k, "GIT_CONFIG_VALUE_") {
			t.Setenv(k, "")
			if err := os.Unsetenv(k); err != nil {
				t.Fatal(err)
			}
		}
	}

	dump := filepath.Join(t.TempDir(), "env.txt")
	bin := fakeClaude(t, `
env > `+dump+`
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	r.GitHub = gh
	res, err := r.Run(context.Background(), Request{
		Name: "t", Profile: ProfileForRole(config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute}),
		Workspace: vcs.Directory(t.TempDir()),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(dump)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimRight(string(b), "\n"), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if ok {
			out[k] = v
		}
	}
	return out, res.SessionDir
}

// gitConfigEntries reads the GIT_CONFIG_KEY_n / GIT_CONFIG_VALUE_n pairs back
// out of a session's environment, stopping at the first index with no key so
// that a count larger than the entries is visible to the caller rather than
// panicking here.
func gitConfigEntries(env map[string]string) []envVar {
	var out []envVar
	for i := 0; ; i++ {
		k, ok := env["GIT_CONFIG_KEY_"+strconv.Itoa(i)]
		if !ok {
			return out
		}
		out = append(out, envVar{k, env["GIT_CONFIG_VALUE_"+strconv.Itoa(i)]})
	}
}

// TestGitHubIdentityReachesTheSession: with [github] set a session's gh acts
// as the bot, its commits are the bot's and its pushes go through gh's
// credential helper instead of the person's stored credentials. With the
// table unset nothing is injected and the machine's own environment reaches
// the session unchanged. sessionEnv's fakeClaude only dumps its environment;
// it does not confirm gh or git actually behave as the dumped variables say
// they would (TestSessionCommitsAndPushesAsTheFactory checks git's push).
func TestGitHubIdentityReachesTheSession(t *testing.T) {
	t.Run("configured", func(t *testing.T) {
		env := sessionEnv(t, botIdentity)
		want := map[string]string{
			EnvGHToken:            "ghp_bot",
			"GIT_AUTHOR_NAME":     "busybees",
			"GIT_COMMITTER_NAME":  "busybees",
			"GIT_AUTHOR_EMAIL":    "bot@example.com",
			"GIT_COMMITTER_EMAIL": "bot@example.com",
		}
		for k, v := range want {
			if env[k] != v {
				t.Errorf("%s = %q, want %q", k, env[k], v)
			}
		}
		entries := gitConfigEntries(env)
		if got := entries[len(entries)-2:]; got[0] != (envVar{"credential.helper", ""}) ||
			got[1] != (envVar{"credential.helper", "!gh auth git-credential"}) {
			t.Errorf("credential helper entries = %+v, want a reset then gh's helper", got)
		}
		// The push settings every session gets are there too.
		if entries[0] != (envVar{"push.autoSetupRemote", "true"}) || entries[1] != (envVar{"push.default", "current"}) {
			t.Errorf("push settings = %+v", entries[:2])
		}
	})

	t.Run("unset", func(t *testing.T) {
		env := sessionEnv(t, config.GitHub{})
		for _, k := range []string{EnvGHToken, "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
			if env[k] != machineEnv {
				t.Errorf("%s = %q, want the machine's own %q: [github] is unset and nothing may be injected", k, env[k], machineEnv)
			}
		}
		for _, e := range gitConfigEntries(env) {
			if e.name == "credential.helper" {
				t.Errorf("a credential helper was configured with no token to use: %+v", e)
			}
		}
	})

	// git_name and git_email are accepted without a token, so an identity
	// on its own reaches the session without a credential following it.
	t.Run("identity without a credential", func(t *testing.T) {
		env := sessionEnv(t, config.GitHub{GitName: "busybees", GitEmail: "bot@example.com"})
		if env["GIT_AUTHOR_NAME"] != "busybees" || env["GIT_COMMITTER_EMAIL"] != "bot@example.com" {
			t.Errorf("git identity did not reach the session: %q / %q", env["GIT_AUTHOR_NAME"], env["GIT_COMMITTER_EMAIL"])
		}
		if env[EnvGHToken] != machineEnv {
			t.Errorf("%s = %q, want the machine's own %q", EnvGHToken, env[EnvGHToken], machineEnv)
		}
	})
}

// TestGitConfigCountMatchesTheEntries: GIT_CONFIG_COUNT is what git believes,
// and an entry past it is silently ignored. The count is derived from the
// entries rather than written out, so it has to agree with them whether the
// credential helper is configured or not. As above, sessionEnv's fake only
// dumps the environment; it does not run git to confirm the count git would
// actually read matches.
func TestGitConfigCountMatchesTheEntries(t *testing.T) {
	for _, c := range []struct {
		name string
		gh   config.GitHub
		want int
	}{
		{"without a token", config.GitHub{}, 2},
		{"with a token", botIdentity, 4},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := sessionEnv(t, c.gh)
			entries := gitConfigEntries(env)
			if len(entries) != c.want {
				t.Errorf("%d GIT_CONFIG_KEY_n entries, want %d: %+v", len(entries), c.want, entries)
			}
			if env["GIT_CONFIG_COUNT"] != strconv.Itoa(len(entries)) {
				t.Errorf("GIT_CONFIG_COUNT = %q but %d entries are set: git would %s",
					env["GIT_CONFIG_COUNT"], len(entries), "read a different number of them")
			}
		})
	}
}

// TestSessionCommitsAndPushesAsTheFactory drives a session that does what a
// developer does — commit on a branch and push it — against the local bare
// remote. The credential helper is configured throughout; a file:// remote
// never asks for credentials, so this asserts the environment does not get
// in a push's way, not that it authenticated against GitHub.
func TestSessionCommitsAndPushesAsTheFactory(t *testing.T) {
	// git reads the person's own configuration too, and this machine may
	// already set push.autoSetupRemote there — which would let the push
	// succeed however bees configured the session. Neutralising it is what
	// makes the assertion about bees' own entries on every machine.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	origin, clone := testutil.SetupRepos(t)
	t.Setenv("GIT_CONFIG_COUNT", "")

	bin := fakeClaude(t, `
git checkout -q -b bees/issue-1
echo change > file.txt
git add file.txt
git commit -q -m "a commit by the factory"
git push -q
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	r.GitHub = botIdentity
	res, err := r.Run(context.Background(), Request{
		Name: "t", Profile: ProfileForRole(config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute}),
		Workspace: vcs.Directory(clone),
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		stderr, _ := os.ReadFile(filepath.Join(res.SessionDir, "stderr.log"))
		t.Fatalf("session failed: %+v: %s", res, stderr)
	}

	// The push reached the bare origin, so `git push` worked with the
	// credential helper and push.autoSetupRemote in place.
	out, err := workspace.Git(context.Background(), origin, "log", "-1", "--format=%an|%ae|%cn|%ce|%s", "bees/issue-1")
	if err != nil {
		t.Fatalf("the branch never reached the origin: %v", err)
	}
	// The clone SetupRepos made has its own user.name and user.email, so a
	// commit carrying the factory's is one the environment steered.
	want := "busybees|bot@example.com|busybees|bot@example.com|a commit by the factory"
	if got := strings.TrimSpace(out); got != want {
		t.Errorf("pushed commit = %q, want %q", got, want)
	}
}

// TestTheTokenVariableReachesTheSession: github.token may be a "$VAR"
// reference, and env strips every inherited BEES_* variable, so a
// BEES_-prefixed name would never reach the session. Its gh would still work
// — the scheduler resolves the token and passes GH_TOKEN — but every
// in-session `bees` command loads bees.toml again, and a reference that
// expands to nothing is a load error, so the built-in MCP server behind
// issue_view, comment, done and the rest would fail on the first call. The name has to reach
// the session; the value it carries is the one the scheduler resolved, so a
// session can never be handed a token from a stale environment.
// sessionEnvDir's fakeClaude only dumps its environment; the config.Load
// calls below are this test's own stand-in for the in-session `bees mcp
// serve`, not a run of the real one.
func TestTheTokenVariableReachesTheSession(t *testing.T) {
	const varName = "BEES_TEST_SESSION_TOKEN"
	const secret = "ghp_only_in_the_environment"
	const toml = "version = 1\n[project]\nrepo = \"a/b\"\ndefault_branch = \"main\"\n" +
		"[github]\nlogin = \"busybees-bot\"\ntoken = \"$" + varName + "\"\n"

	// The environment bees itself runs in, which is where the secret lives.
	t.Setenv(varName, secret)
	env, dir := sessionEnvDir(t, config.GitHub{Login: "busybees-bot", Token: "$" + varName})

	if env[varName] != secret {
		t.Errorf("%s = %q, want the resolved token: without it a session cannot load bees.toml", varName, env[varName])
	}
	if env[EnvGHToken] != secret {
		t.Errorf("%s = %q, want %q", EnvGHToken, env[EnvGHToken], secret)
	}

	// Load bees.toml the way an in-session `bees mcp serve` does: with the
	// environment the session was handed and nothing else. Dropping the test
	// process's own copy of the variable first is what makes the load below an
	// assertion about the session rather than about this process.
	path := filepath.Join(t.TempDir(), "bees.toml")
	if err := os.WriteFile(path, []byte(toml), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Unsetenv(varName); err != nil {
		t.Fatal(err)
	}
	if _, err := config.Load(path); err == nil {
		t.Fatal("bees.toml loads with the variable unset: the check below could not fail")
	}
	for k, v := range env {
		if strings.HasPrefix(k, beesEnvPrefix) {
			t.Setenv(k, v)
		}
	}
	if _, err := config.Load(path); err != nil {
		t.Errorf("a session started with [github] configured cannot load bees.toml: %v", err)
	}

	// mcp.json and the prompts are written into the session directory, which
	// outlives the session; the secret must be in none of them.
	if err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if strings.Contains(string(b), secret) {
			t.Errorf("the token was written to %s", p)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Codex filters the environment when starting an MCP server. Check the
// generated overrides against the session environment, then load bees.toml
// with only the credentials that would reach the built-in server. The bin
// standing in for codex only dumps its environment and args; it does not
// confirm the real codex CLI accepts `-c mcp_servers.bees.env_vars=...` and
// the other overrides in this exact shape.
func TestCodexBuiltinMCPCredentials(t *testing.T) {
	const secret = "ghp_codex_environment_only"
	for _, tc := range []struct {
		name, token, tokenVar string
	}{
		{"bees variable", "$BEES_GITHUB_TOKEN", "BEES_GITHUB_TOKEN"},
		{"braced variable", "${BEES_GITHUB_TOKEN}", "BEES_GITHUB_TOKEN"},
		{"custom variable", "$FACTORY_GITHUB_TOKEN", "FACTORY_GITHUB_TOKEN"},
		{"gh variable", "$GH_TOKEN", EnvGHToken},
		{"literal", secret, ""},
		{"machine identity", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(EnvGHToken, secret)
			if tc.tokenVar != "" {
				t.Setenv(tc.tokenVar, secret)
			}
			const notesVar = "BEES_NAMS_KEY"
			const notesSecret = "nams_codex_environment_only"
			t.Setenv(notesVar, notesSecret)
			r := newRunner(t, "")
			if tc.token != "" {
				r.GitHub = config.GitHub{Login: "busybees-bot", Token: tc.token}
			}
			r.Notes = config.Notes{Backend: config.NotesBackendNeo4j,
				Neo4jURL: "https://nams.example.com/v1", Neo4jAPIKey: "$" + notesVar}
			req := Request{Profile: Profile{Name: "developer", Agent: "codex", VCSAccess: true}, Workspace: vcs.Directory(t.TempDir())}
			dir := t.TempDir()
			entry := r.builtinMCP(req, dir)
			argsPath := filepath.Join(t.TempDir(), "args")
			envPath := filepath.Join(t.TempDir(), "environment")
			r.CodexBin = fakeClaude(t, "env > "+envPath+"\nprintf '%s\\n' \"$@\" > "+argsPath+"\necho '{\"type\":\"turn.completed\"}'")
			req.SessionDir = dir
			if _, err := r.Run(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			envData, err := os.ReadFile(envPath)
			if err != nil {
				t.Fatal(err)
			}
			parentEnv := map[string]string{}
			for _, kv := range strings.Split(string(envData), "\n") {
				k, v, _ := strings.Cut(kv, "=")
				parentEnv[k] = v
			}
			argsData, err := os.ReadFile(argsPath)
			if err != nil {
				t.Fatal(err)
			}
			args := strings.Split(strings.TrimSpace(string(argsData)), "\n")
			var overrides []string
			for i := 0; i+1 < len(args); i++ {
				if args[i] == "-c" {
					overrides = append(overrides, args[i+1])
				}
			}
			childEnv := map[string]string{}
			for _, o := range overrides {
				key, value, _ := strings.Cut(o, "=")
				switch {
				case key == "mcp_servers.bees.env_vars":
					var names []string
					if err := json.Unmarshal([]byte(value), &names); err != nil {
						t.Fatal(err)
					}
					for _, name := range names {
						childEnv[name] = parentEnv[name]
					}
				case strings.HasPrefix(key, "mcp_servers.bees.env."):
					var v string
					if err := json.Unmarshal([]byte(value), &v); err != nil {
						t.Fatal(err)
					}
					childEnv[strings.TrimPrefix(key, "mcp_servers.bees.env.")] = v
				}
			}
			if childEnv[EnvGHToken] != secret {
				t.Error("the built-in server did not receive GH_TOKEN")
			}
			if tc.tokenVar != "" {
				t.Setenv(tc.tokenVar, childEnv[tc.tokenVar])
			}
			t.Setenv(notesVar, childEnv[notesVar])
			path := filepath.Join(t.TempDir(), "bees.toml")
			body := fmt.Sprintf("version = 1\n[project]\nrepo = \"a/b\"\n[github]\nlogin = %q\ntoken = %q\n"+
				"[notes]\nbackend = \"neo4j\"\nneo4j_url = %q\nneo4j_api_key = %q\n",
				r.GitHub.Login, tc.token, r.Notes.Neo4jURL, r.Notes.Neo4jAPIKey)
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := config.Load(path); err != nil {
				t.Errorf("the built-in server cannot load bees.toml: %v", err)
			}
			if err := WriteMCPConfig(filepath.Join(dir, "mcp.json"), map[string]MCPEntry{config.BuiltinMCPServer: entry}); err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range []string{secret, notesSecret} {
				if strings.Contains(strings.Join(overrides, "\n"), s) || strings.Contains(string(b), s) {
					t.Error("a credential was written into the MCP config or command arguments")
				}
			}
		})
	}
}

// TestGitHubAppSession: a GitHub App's session is given no token. Its gh is
// the App's, first on PATH, and its git asks the App's credential helper;
// both ask the runner's Minter, which the runner holds for the session, for
// a token when they need one, and the Minter mints it then. The "gh" on
// PATH is a two-line fake that only echoes the token it was given, and
// ghapptest.New simulates GitHub's own App token endpoint: neither confirms
// a real `gh api` call or the real GitHub App token endpoint behaves this
// way, only that busybees' credential helper and PATH wiring reach them.
func TestGitHubAppSession(t *testing.T) {
	srv := ghapptest.New(t, "a/b")
	stateDir := t.TempDir()
	m := srv.Minter(t, ghapp.Dir(stateDir))
	m.Poll = 10 * time.Millisecond

	// The gh the App's gh runs: the next one on PATH, a fake.
	fake := t.TempDir()
	if err := os.WriteFile(filepath.Join(fake, "gh"), []byte("#!/bin/sh\necho \"gh token=$GH_TOKEN\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fake+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(EnvGHToken, machineEnv)
	t.Setenv("GIT_CONFIG_COUNT", "")
	t.Setenv("GIT_TERMINAL_PROMPT", "0")

	out := filepath.Join(t.TempDir(), "out.txt")
	bin := fakeClaude(t, `
{
  echo "GH_TOKEN=[$GH_TOKEN]"
  gh api user
  printf 'protocol=https\nhost=github.com\n\n' | git credential fill
} > `+out+` 2>&1
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	r.StateDir = stateDir
	r.BeesBin = filepath.Join(t.TempDir(), "bees")
	r.GitHub = config.GitHub{Login: "busybees[bot]", AppID: srv.AppID, PrivateKey: "$UNUSED"}
	r.GitHubApp = m
	if _, err := r.Run(context.Background(), Request{
		Name: "t", Profile: ProfileForRole(config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute}),
		Workspace: vcs.Directory(t.TempDir()),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"GH_TOKEN=[]\n", "gh token=ghs_1\n", "username=x-access-token\n", "password=ghs_1\n"} {
		if !strings.Contains(got, want) {
			t.Errorf("session output lacks %q:\n%s", want, got)
		}
	}
	if n := len(srv.Minted()); n != 1 {
		t.Errorf("minted %d tokens for one session, want 1", n)
	}
}
