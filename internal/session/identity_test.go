package session

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/workspace"
)

// botIdentity is a fully configured [github] table.
var botIdentity = config.GitHub{
	Login: "busybees-bot", Token: "ghp_bot", GitName: "busybees", GitEmail: "bot@example.com",
}

// machineEnv is what the person running bees already has in their
// environment. Every identity variable is seeded with it so that "[github]
// is unset means today's behaviour" can be asserted as "the machine's own
// value reached the session untouched", which is stronger — and steadier on
// a developer machine — than asserting the variable is absent.
const machineEnv = "machine-owner"

// sessionEnv runs one session with the given identity and returns the
// environment it was handed, last assignment winning as os/exec does.
func sessionEnv(t *testing.T, gh config.GitHub) map[string]string {
	t.Helper()
	for _, k := range []string{EnvGHToken, "GIT_AUTHOR_NAME", "GIT_AUTHOR_EMAIL", "GIT_COMMITTER_NAME", "GIT_COMMITTER_EMAIL"} {
		t.Setenv(k, machineEnv)
	}
	// The GIT_CONFIG_* block defers to an operator who set the count
	// themselves; pin it empty so the test does not depend on the shell it
	// runs in.
	t.Setenv("GIT_CONFIG_COUNT", "")

	bin := fakeClaude(t, `
env > "$BEES_SESSION_DIR/env.txt"
echo '{"type":"result","subtype":"success","is_error":false,"result":"ok"}'
`)
	r := newRunner(t, bin)
	r.GitHub = gh
	res, err := r.Run(context.Background(), Request{
		Name: "t", Role: config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute},
		WorkDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(res.SessionDir, "env.txt"))
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
	return out
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
// the session unchanged.
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
		// The push settings that predate [github] are still there.
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
// credential helper is configured or not.
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
		Name: "t", Role: config.ResolvedRole{Name: "developer", Model: "opus", MaxTurns: 1, Timeout: time.Minute},
		WorkDir: clone,
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
