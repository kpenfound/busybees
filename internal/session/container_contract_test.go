package session

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

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
