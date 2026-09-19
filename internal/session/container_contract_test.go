package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/core/agent"
	"github.com/kpenfound/busybees/core/agent/agenttest"
	"github.com/kpenfound/busybees/core/vcs"
	"github.com/kpenfound/busybees/internal/config"
)

func TestContainerSessionRefusedWithoutWhatItNeeds(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	claude := fakeClaude(t, `touch "$BEES_SESSION_DIR/ran"; echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'`)
	for _, tc := range []struct {
		name string
		role config.ResolvedRole
		gh   config.GitHub
		want string
	}{
		{"no image", config.ResolvedRole{Name: "developer", Sandbox: config.SandboxContainer}, config.GitHub{Login: "bot", Token: "t"}, "sandbox_image"},
		{"no github", config.ResolvedRole{Name: "developer", Sandbox: config.SandboxContainer, SandboxImage: "img"}, config.GitHub{}, "[github]"},
		{"no credential", config.ResolvedRole{Name: "developer", Sandbox: config.SandboxContainer, SandboxImage: "img"}, config.GitHub{Login: "bot", Token: "t"}, "ANTHROPIC_API_KEY"},
		{"sbx without github", config.ResolvedRole{Name: "developer", Sandbox: config.SandboxSbx}, config.GitHub{}, "[github]"},
		{"sbx for an agent without a template", config.ResolvedRole{Name: "developer", Agent: "pi", Sandbox: config.SandboxSbx}, config.GitHub{Login: "bot", Token: "t"}, `sandbox "sbx" has no template for agent "pi"`},
	} {
		r := newRunner(t, claude)
		r.GitHub = tc.gh
		_, err := r.Run(context.Background(), Request{Name: "boxed", Profile: ProfileForRole(tc.role), Workspace: vcs.Directory(t.TempDir())})
		if err == nil {
			t.Fatalf("%s: a container session ran", tc.name)
		}
		for _, want := range []string{"developer", tc.want} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not mention %q", tc.name, err, want)
			}
		}
		entries, _ := os.ReadDir(r.SessionsDir)
		for _, e := range entries {
			if _, err := os.Stat(filepath.Join(r.SessionsDir, e.Name(), "ran")); err == nil {
				t.Errorf("%s: claude ran", tc.name)
			}
		}
	}
}

func TestContainerIdentityContract(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "agent-secret")
	t.Setenv("HOST_ONLY", "not-container-context")
	t.Setenv("BEES_STALE", "old")
	t.Setenv("BEES_GITHUB_KEY", "github-secret")
	t.Setenv("BEES_NOTES_KEY", "notes-secret")
	r := newRunner(t, fakeClaude(t, `echo '{"type":"result","subtype":"success","result":"ok"}'`))
	r.DockerBin = agenttest.Docker(t, "image", EnvSessionDir)
	r.BeesBin = agenttest.MCPServer(t, EnvSessionDir)
	r.ContainerListen = "127.0.0.1:0"
	r.StateDir = t.TempDir()
	r.ConfigPath = "/config/bees.toml"
	r.GitHub = config.GitHub{Login: "bot", Token: "$BEES_GITHUB_KEY", GitName: "Bot", GitEmail: "bot@example.com"}
	r.Notes = config.Notes{Backend: config.NotesBackendNeo4j, Neo4jAPIKey: "$BEES_NOTES_KEY"}
	profile := ProfileForRole(config.ResolvedRole{Name: "developer", Sandbox: config.SandboxContainer, SandboxImage: "image", Shell: "/bin/bash", Env: map[string]string{EnvGHToken: "role-must-not-win"}})
	req := Request{Name: "contract", Profile: profile, Workspace: vcs.Directory(t.TempDir()), Env: map[string]string{EnvIssue: "724", EnvPR: "900", EnvBranch: "bees/issue-724"}}
	prepared := r.prepare(req, "/session")
	for _, name := range []string{"PATH", "USER", "HOST_ONLY", EnvBin, "BEES_STALE"} {
		if _, ok := prepared.Env[name]; ok {
			t.Errorf("%s included in container context", name)
		}
	}
	res, err := r.Run(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	data, err := os.ReadFile(filepath.Join(res.SessionDir, "docker-env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	env := map[string]string{}
	for _, kv := range strings.Split(string(data), "\n") {
		key, value, _ := strings.Cut(kv, "=")
		env[key] = value
	}
	for key, want := range map[string]string{EnvRole: "developer", EnvIssue: "724", EnvPR: "900", EnvBranch: "bees/issue-724", EnvConfig: r.ConfigPath, EnvStateDir: r.StateDir, EnvRepo: "a/b", EnvLabel: "bees", EnvGHToken: "github-secret", "BEES_GITHUB_KEY": "github-secret", "BEES_NOTES_KEY": "notes-secret", "GIT_AUTHOR_NAME": "Bot", "GIT_COMMITTER_EMAIL": "bot@example.com", "SHELL": "/bin/bash", "GIT_CONFIG_COUNT": "7"} {
		if env[key] != want {
			t.Errorf("%s=%q, want %q", key, env[key], want)
		}
	}
	entries := gitConfigEntries(env)
	wantEntries := append(r.gitConfig(), containerGitConfig...)
	if !slices.Equal(entries, wantEntries) {
		t.Errorf("git entries: %+v, want %+v", entries, wantEntries)
	}
	for _, name := range []string{"docker-args.txt", "args.txt", "mcp.json"} {
		data, err := os.ReadFile(filepath.Join(res.SessionDir, name))
		if os.IsNotExist(err) && name == "args.txt" {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"github-secret", "notes-secret", "agent-secret"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("credential serialized in %s", name)
			}
		}
	}
	serverData, err := os.ReadFile(filepath.Join(res.SessionDir, "server-env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{EnvBin + "=" + r.BeesBin, "BEES_GITHUB_KEY=github-secret", "BEES_NOTES_KEY=notes-secret"} {
		if !strings.Contains(string(serverData), want) {
			t.Errorf("host server missing %s", want)
		}
	}
	args, err := os.ReadFile(filepath.Join(res.SessionDir, "docker-args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"bees.session=" + res.SessionDir, "destination=/home/bees"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("missing container convention %s", want)
		}
	}
}

func TestDeniedProfileDoesNotInjectFactoryVCSIdentity(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "agent-secret")
	for _, mode := range []string{config.SandboxNone, config.SandboxClaude, config.SandboxContainer} {
		t.Run(mode, func(t *testing.T) {
			r := newRunner(t, fakeClaude(t, `env > "$BEES_SESSION_DIR/agent-env"
echo '{"type":"result","subtype":"success","result":"ok"}'`))
			r.GitHub = config.GitHub{Login: "bot", Token: "denied-github-token", GitName: "denied-author", GitEmail: "denied@example.com"}
			r.DockerBin = agenttest.Docker(t, "image", EnvSessionDir)
			r.BeesBin = agenttest.MCPServer(t, EnvSessionDir)
			r.ContainerListen = "127.0.0.1:0"
			r.StateDir = t.TempDir()
			profile := ProfileForRole(config.ResolvedRole{Name: "developer", Sandbox: mode, SandboxImage: "image"})
			profile.VCSAccess = false
			req := Request{Name: "denied", Profile: profile, Workspace: vcs.Directory(t.TempDir())}
			prepared := r.prepare(req, "/session")
			for _, env := range []map[string]string{prepared.Env, prepared.ContainerEnv, prepared.HostMCP.Env} {
				for k := range env {
					if strings.HasPrefix(k, "GIT_") || k == EnvGHToken {
						t.Errorf("VCS entry in general environment: %s", k)
					}
				}
			}
			if len(prepared.HostMCP.Entry.EnvVars) != 0 {
				t.Fatalf("forwarded VCS credentials: %v", prepared.HostMCP.Entry.EnvVars)
			}
			res, err := r.Run(context.Background(), req)
			if mode == config.SandboxNone {
				// An unsandboxed host cannot keep VCS metadata unwritable.
				if !errors.Is(err, agent.ErrUnsupported) {
					t.Fatalf("unsandboxed session without VCS: %+v, %v", res, err)
				}
				return
			}
			if err != nil || res.IsError {
				t.Fatalf("run: %+v, %v", res, err)
			}
			data, err := os.ReadFile(filepath.Join(res.SessionDir, "agent-env"))
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"denied-github-token", "denied-author", "denied@example.com"} {
				if strings.Contains(string(data), secret) {
					t.Errorf("denied VCS identity reached agent: %s", secret)
				}
			}
		})
	}
}

// An sbx session is the container contract run through the sbx CLI: the
// factory's identity and BEES_* context reach the sandbox by name, the bees
// binary and PATH do not (it is not in the sandbox), no agent credential
// of bees' own is required or forwarded, and the built-in server runs on
// the host.
func TestSbxIdentityContract(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	t.Setenv("CLAUDE_CODE_OAUTH_TOKEN", "")
	t.Setenv("HOST_ONLY", "not-sandbox-context")
	t.Setenv("BEES_GITHUB_KEY", "github-secret")
	r := newRunner(t, fakeClaude(t, `echo '{"type":"result","subtype":"success","result":"ok"}'`))
	r.SbxBin = agenttest.Sbx(t, EnvSessionDir)
	r.BeesBin = agenttest.MCPServer(t, EnvSessionDir)
	r.StateDir = t.TempDir()
	r.GitHub = config.GitHub{Login: "bot", Token: "$BEES_GITHUB_KEY", GitName: "Bot", GitEmail: "bot@example.com"}
	profile := ProfileForRole(config.ResolvedRole{Name: "developer", Sandbox: config.SandboxSbx, SandboxImage: "ghcr.io/acme/template:1"})
	req := Request{Name: "contract", Profile: profile, Workspace: vcs.Directory(t.TempDir()), Env: map[string]string{EnvIssue: "799"}}
	prepared := r.prepare(req, "/session")
	for _, name := range []string{"PATH", "USER", "HOST_ONLY", EnvBin} {
		if _, ok := prepared.Env[name]; ok {
			t.Errorf("%s included in sandbox context", name)
		}
	}
	res, err := r.Run(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	args, err := os.ReadFile(filepath.Join(res.SessionDir, "sbx-exec-args.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--env\n" + EnvGHToken + "\n", "--env\n" + EnvRole + "\n", "--env\n" + EnvIssue + "\n", "--env\n" + EnvMCPToken + "\n"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("sbx exec args lack %q:\n%s", want, args)
		}
	}
	for _, absent := range []string{"--env\n" + EnvBin + "\n", "--env\nPATH\n", "--env\nANTHROPIC_API_KEY\n", "github-secret"} {
		if strings.Contains(string(args), absent) {
			t.Errorf("sbx exec args carry %q:\n%s", absent, args)
		}
	}
	env, err := os.ReadFile(filepath.Join(res.SessionDir, "sbx-exec-env.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{EnvGHToken + "=github-secret", "GIT_AUTHOR_NAME=Bot", EnvRole + "=developer"} {
		if !strings.Contains(string(env), want) {
			t.Errorf("sbx client env lacks %s", want)
		}
	}
	create, err := os.ReadFile(filepath.Join(filepath.Dir(r.SbxBin), "sbx-create.txt"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--name\nbees-contract-", "--template\nghcr.io/acme/template:1\n", "--skills\noff\n"} {
		if !strings.Contains(string(create), want) {
			t.Errorf("sbx create args lack %q:\n%s", want, create)
		}
	}
	if _, err := os.Stat(filepath.Join(res.SessionDir, "server-env.txt")); err != nil {
		t.Errorf("the built-in server did not run on the host: %v", err)
	}
}

// A role's Dagger keys reach an sbx session of any agent as the profile's
// Dagger option and its grant: the CLI is installed at the role's release
// and the engine address reaches the sandbox by name. A fallback profile
// in another sandbox runs without it.
func TestSbxDaggerContract(t *testing.T) {
	t.Setenv("BEES_GITHUB_KEY", "github-secret")
	codex := agenttest.Script(t, "codex", `cat > /dev/null
echo '{"type":"thread.started","thread_id":"t1"}'
echo '{"type":"turn.completed"}'
`)
	r := newRunner(t, "")
	r.CodexBin = codex
	r.SbxBin = agenttest.Sbx(t, EnvSessionDir)
	r.BeesBin = agenttest.MCPServer(t, EnvSessionDir)
	r.StateDir = t.TempDir()
	r.GitHub = config.GitHub{Login: "bot", Token: "$BEES_GITHUB_KEY"}
	cfg, err := config.Parse(`version = 5
[profiles.boxed]
agent = "codex"
sandbox = "sbx"
fallback = "open"
[profiles.open]
agent = "codex"
[roles.developer]
profile = "boxed"
sandbox_dagger_engine = "tcp://127.0.0.1:1234"
sandbox_dagger_version = "v0.20.5"
`, filepath.Join(t.TempDir(), "bees.toml"))
	if err != nil {
		t.Fatal(err)
	}
	role, err := cfg.Role(config.RoleDeveloper)
	if err != nil {
		t.Fatal(err)
	}
	profile := ProfileForRole(role)
	if profile.Dagger == nil || *profile.Dagger != (agent.Dagger{Engine: "tcp://127.0.0.1:1234", Version: "v0.20.5"}) {
		t.Fatalf("profile Dagger: %+v", profile.Dagger)
	}
	if profile.Fallback == nil || profile.Fallback.Dagger != nil {
		t.Errorf("the unboxed fallback profile was given Dagger: %+v", profile.Fallback)
	}
	if g := r.grants(r.prepare(Request{Profile: profile, Workspace: vcs.Directory(t.TempDir())}, "/session")); g.DaggerEngine != "tcp://127.0.0.1:1234" {
		t.Errorf("grant: %q", g.DaggerEngine)
	}
	if g := r.grants(r.prepare(Request{Profile: *profile.Fallback, Workspace: vcs.Directory(t.TempDir())}, "/session")); g.DaggerEngine != "" {
		t.Errorf("the fallback was granted the engine %q", g.DaggerEngine)
	}

	res, err := r.Run(context.Background(), Request{Name: "dagger", Profile: profile, Workspace: vcs.Directory(t.TempDir())})
	if err != nil || res.IsError {
		t.Fatalf("run: %+v, %v", res, err)
	}
	engine := filepath.Dir(r.SbxBin)
	create, _ := os.ReadFile(filepath.Join(engine, "sbx-create.txt"))
	if !strings.Contains(string(create), "--skills\noff\ncodex\n") {
		t.Errorf("sbx create is not for codex:\n%s", create)
	}
	setup, _ := os.ReadFile(filepath.Join(engine, "sbx-setup.txt"))
	if !strings.Contains(string(setup), "\nDAGGER_VERSION=0.20.5\n") {
		t.Errorf("the Dagger CLI was not installed at the role's release:\n%s", setup)
	}
	args, _ := os.ReadFile(filepath.Join(res.SessionDir, "sbx-exec-args.txt"))
	if !strings.Contains(string(args), "--env\n"+agent.EnvDaggerRunnerHost+"\n") {
		t.Errorf("sbx exec args lack the engine address:\n%s", args)
	}
	env, _ := os.ReadFile(filepath.Join(res.SessionDir, "sbx-exec-env.txt"))
	if !strings.Contains(string(env), agent.EnvDaggerRunnerHost+"=tcp://host.docker.internal:1234\n") {
		t.Errorf("sbx client env lacks the engine address:\n%s", env)
	}
}
