package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/testutil"
	"github.com/kpenfound/busybees/internal/versions"
)

// botTOML is a factory that acts as a bot while picking up the work of the
// person running it. project.repo and default_branch are explicit because the
// test clone's origin is a local path, not a github.com URL.
const botTOML = `version = 1
[project]
repo = "acme/widgets"
default_branch = "main"
state_dir = ".bees"
[filter]
label = "bees"
assignee = "@me"
[github]
login = "busybees-bot"
token = "$BEES_TEST_TOKEN"
`

// setupBotFactory writes cfg into a git clone and makes "@me" resolve to
// kyle, the person running bees. It returns the path of the written config.
func setupBotFactory(t *testing.T, cfg string) string {
	t.Helper()
	_, clone := testutil.SetupRepos(t)
	path := filepath.Join(clone, "bees.toml")
	if err := os.WriteFile(path, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BEES_TEST_TOKEN", "ghp_bot")
	fakeMe(t, "kyle")
	return path
}

// fakeMe replaces the "who is running bees" lookup. The real one shells out
// to gh, which tests must never do.
func fakeMe(t *testing.T, login string) *int {
	t.Helper()
	calls := 0
	old := meLookup
	meLookup = func(context.Context) (string, error) {
		calls++
		return login, nil
	}
	t.Cleanup(func() { meLookup = old })
	return &calls
}

// TestOrchestratorActsAsTheConfiguredAccount covers the path every scheduler
// command takes: the client the orchestrator's own gh calls go through
// carries the configured token, while filter.assignee = "@me" still resolves
// to the person. The two identities are different on purpose — "@me" says
// whose work the factory picks up, not who it acts as — so they are asserted
// together.
func TestOrchestratorActsAsTheConfiguredAccount(t *testing.T) {
	path := setupBotFactory(t, botTOML)
	t.Setenv(versions.EnvSkip, "1")

	a, err := newApp(context.Background(), &globalFlags{config: path})
	if err != nil {
		t.Fatal(err)
	}
	if a.gh.Token != "ghp_bot" {
		t.Errorf("orchestrator client token: got %q want %q", a.gh.Token, "ghp_bot")
	}
	if a.cfg.Filter.Assignee != "kyle" {
		t.Errorf("filter.assignee: got %q want %q (the person, not the bot)", a.cfg.Filter.Assignee, "kyle")
	}
}

// TestMCPServerActsAsTheConfiguredAccount is the same claim for the second
// resolution path: the built-in MCP server loads its own configuration, so a
// fix applied only to the scheduler would leave every tool a session calls
// filtering on the wrong login.
func TestMCPServerActsAsTheConfiguredAccount(t *testing.T) {
	path := setupBotFactory(t, botTOML)

	b := &backend{g: &globalFlags{config: path}}
	if err := b.load(context.Background()); err != nil {
		t.Fatal(err)
	}
	if b.gh.Token != "ghp_bot" {
		t.Errorf("tool client token: got %q want %q", b.gh.Token, "ghp_bot")
	}
	if b.filter.Assignee != "kyle" {
		t.Errorf("filter.assignee: got %q want %q (the person, not the bot)", b.filter.Assignee, "kyle")
	}
	q, _, err := b.Rules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if q.Assignee != "kyle" {
		t.Errorf("visibility query assignee: got %q want %q", q.Assignee, "kyle")
	}
}

// TestNoGitHubTableInjectsNoToken pins the default: a bees.toml written
// before [github] existed produces exactly the client it always did.
func TestNoGitHubTableInjectsNoToken(t *testing.T) {
	body := strings.SplitN(botTOML, "[github]", 2)[0]
	path := setupBotFactory(t, body)
	t.Setenv(versions.EnvSkip, "1")

	a, err := newApp(context.Background(), &globalFlags{config: path})
	if err != nil {
		t.Fatal(err)
	}
	if a.gh.Token != "" {
		t.Errorf("token injected without [github]: %q", a.gh.Token)
	}
	if a.cfg.Filter.Assignee != "kyle" {
		t.Errorf("filter.assignee: got %q want %q", a.cfg.Filter.Assignee, "kyle")
	}
}

// fakeGH is a client whose gh calls are answered from a table keyed by the
// first two arguments ("api user", "repo view"); a call whose key is fails,
// or that has no answer, returns an error. Tests must never run the real gh.
func fakeGH(answers map[string]string, fails string) *github.Client {
	c := github.NewAs("acme/widgets", "bot", "ghp_bot")
	c.Exec = func(_ context.Context, args ...string) ([]byte, error) {
		key := strings.Join(args[:min(2, len(args))], " ")
		out, ok := answers[key]
		if !ok || key == fails {
			return nil, fmt.Errorf("gh %s: HTTP 401: Bad credentials", key)
		}
		return []byte(out + "\n"), nil
	}
	return c
}

// TestVerifyGitHubAccount covers what `bees init` checks before it writes
// anything: a token GitHub rejects, a token belonging to somebody else, and a
// token that cannot read the repository each fail by name.
func TestVerifyGitHubAccount(t *testing.T) {
	ctx := context.Background()
	cfg := func(t *testing.T) *config.Config {
		t.Helper()
		t.Setenv("BEES_TEST_TOKEN", "ghp_bot")
		c, err := config.Parse(botTOML, filepath.Join(t.TempDir(), "bees.toml"))
		if err != nil {
			t.Fatal(err)
		}
		return c
	}

	// The happy path: the token belongs to the login and reads the repo. The
	// client the checks run through is the factory's own, so they answer for
	// the account it will act as rather than for the machine's gh user.
	c := cfg(t)
	if got := githubClient(c).Token; got != "ghp_bot" {
		t.Fatalf("the verified client does not carry the configured token: %q", got)
	}
	// It carries the login as well: that is what makes a comment by the bot
	// a bee's comment without a marker (#243).
	if got := githubClient(c).ActsAs; got != "busybees-bot" {
		t.Fatalf("the verified client does not act as the configured login: %q", got)
	}
	login, err := verifyAccount(ctx, c, fakeGH(map[string]string{"api user": "busybees-bot", "repo view": "main"}, ""))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if login != "busybees-bot" {
		t.Errorf("login: got %q want %q", login, "busybees-bot")
	}

	for _, tc := range []struct {
		name  string
		resp  map[string]string
		fails string
		want  string
	}{
		{"token rejected", nil, "api user", "github.token was not accepted"},
		{"wrong login", map[string]string{"api user": "someone-else"}, "", "belongs to \"someone-else\""},
		{"no repo access", map[string]string{"api user": "busybees-bot"}, "repo view", "cannot read acme/widgets"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg(t)
			_, err := verifyAccount(ctx, c, fakeGH(tc.resp, tc.fails))
			if err == nil {
				t.Fatal("verification passed a token it should reject")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say what is wrong: %v", err)
			}
		})
	}

	// With [github] unset there is nothing to verify: the answer is the
	// machine's own gh user, and no token is involved.
	c, err = config.Parse(strings.SplitN(botTOML, "[github]", 2)[0], filepath.Join(t.TempDir(), "bees.toml"))
	if err != nil {
		t.Fatal(err)
	}
	fakeMe(t, "kyle")
	if login, err = verifyAccount(ctx, c, fakeGH(nil, "")); err != nil || login != "kyle" {
		t.Errorf("unset [github]: got %q, %v; want the machine owner", login, err)
	}
}

// TestInitWritesAndVerifiesTheGitHubAccount: --github-login/--github-token
// write the [github] table as active settings, and nothing is written until
// the account has been verified — a token that cannot read the repository
// would otherwise fail one step later, on the labels, leaving a
// half-initialised directory behind.
func TestInitWritesAndVerifiesTheGitHubAccount(t *testing.T) {
	_, clone := testutil.SetupRepos(t)
	deps, labels, _ := testInitDepsWithDoctor("")
	deps.currentRepo = func(context.Context, string) (string, error) { return "acme/widgets", nil }
	o := initOptions{
		dir: clone, remote: "origin", label: config.DefaultLabel,
		repo: "acme/widgets", defaultBranch: "main",
		githubLogin: "busybees-bot", githubToken: "$BEES_TEST_TOKEN",
	}
	t.Setenv("BEES_TEST_TOKEN", "ghp_bot")

	var err error
	out := captureStdout(t, func() { err = runInit(context.Background(), o, deps) })
	if err != nil {
		t.Fatalf("init: %v", err)
	}
	body, rerr := os.ReadFile(filepath.Join(clone, "bees.toml"))
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, want := range []string{"\nlogin = \"busybees-bot\"\n", "\ntoken = \"$BEES_TEST_TOKEN\"\n"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("bees.toml does not carry %q", want)
		}
	}
	if !strings.Contains(out, "acting on GitHub as busybees-bot") {
		t.Errorf("init does not say which account it acts as:\n%s", out)
	}
	if *labels != 1 {
		t.Errorf("labels created %d times, want 1", *labels)
	}

	// A token the verification rejects stops init before it writes anything.
	_, clone2 := testutil.SetupRepos(t)
	deps2, labels2, _ := testInitDepsWithDoctor("")
	deps2.verifyGitHub = func(context.Context, *config.Config) (string, error) {
		return "", errNoGH
	}
	o.dir = clone2
	if err := runInit(context.Background(), o, deps2); err == nil {
		t.Fatal("init passed a token the verification rejected")
	}
	assertClean(t, clone2)
	if *labels2 != 0 {
		t.Errorf("labels were created after a failed verification (%d)", *labels2)
	}
}

// TestActingAsLine: `bees status` names the account the factory acts as, and
// says nothing extra when it is your own — finding that out would cost an API
// call on every status, and the answer would tell the reader nothing new.
func TestActingAsLine(t *testing.T) {
	t.Setenv("BEES_TEST_TOKEN", "ghp_bot")
	path := filepath.Join(t.TempDir(), "bees.toml")

	cfg, err := config.Parse(botTOML, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := actingAs(cfg); got != "   acting as: busybees-bot" {
		t.Errorf("configured: got %q", got)
	}
	cfg, err = config.Parse(strings.SplitN(botTOML, "[github]", 2)[0], path)
	if err != nil {
		t.Fatal(err)
	}
	if got := actingAs(cfg); got != "" {
		t.Errorf("unset [github] still prints an account: %q", got)
	}
}
