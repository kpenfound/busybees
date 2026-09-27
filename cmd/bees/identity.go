package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/ghapp"
	"github.com/kpenfound/busybees/internal/github"
	"github.com/kpenfound/busybees/internal/session"
)

// meLookup answers "who is running bees?". It is a variable so tests can
// replace it, and it deliberately goes through github.CurrentUser, which
// carries no token: see resolveFilterAssignee.
var meLookup = github.CurrentUser

// githubClient builds the client every gh call the orchestrator makes goes
// through: the account [github] configures, or the machine's own gh
// authentication when the table is unset. config.Validate has already
// rejected a token that expands to nothing, so an empty token here means the
// operator asked for the machine's own gh authentication.
//
// For a GitHub App it also returns the Minter the client's tokens come from,
// which a session runner holds so that its sessions get tokens too (nil
// otherwise). Inside a session there is no Minter and no private key: the
// client reads the tokens the session's bees process keeps in the state
// directory.
func githubClient(cfg *config.Config) (*github.Client, *ghapp.Minter, error) {
	stateDir := os.Getenv(session.EnvStateDir)
	if stateDir == "" {
		stateDir = cfg.StateDir()
	}
	return ghapp.NewClient(cfg.GitHub, cfg.Project.Repo, stateDir, os.Getenv(session.EnvSessionDir) != "")
}

// resolveFilterAssignee replaces filter.assignee = "@me" with the login of
// the person running bees.
//
// "@me" is about whose work the factory picks up, which is the machine
// owner's — so it is resolved with their own gh authentication, before and
// independently of the token in [github]. Resolving it as the bot would hide
// every issue the person assigned to themselves. Somebody who does want the
// bot's issues writes the bot's login out in full.
func resolveFilterAssignee(ctx context.Context, cfg *config.Config) error {
	if cfg.Filter.Assignee != "@me" {
		return nil
	}
	login, err := meLookup(ctx)
	if err != nil {
		return fmt.Errorf("resolve filter.assignee=@me: %w", err)
	}
	cfg.Filter.Assignee = login
	return nil
}

// resolveFilterSelf is the login the factory acts as, for github.Query.Self:
// what keeps the issues and pull requests the factory opens visible under
// filter.creator, which they are authored by and cannot be made to match.
// It is configuration when [github] is set and the machine's own gh user
// otherwise, and it is only looked up while filter.creator is set, because
// that lookup is an API call and nothing else needs the answer.
func resolveFilterSelf(ctx context.Context, cfg *config.Config) (string, error) {
	if cfg.Filter.Creator == "" {
		return "", nil
	}
	if cfg.GitHub.Configured() {
		return cfg.GitHub.Login, nil
	}
	login, err := meLookup(ctx)
	if err != nil {
		return "", fmt.Errorf("resolve the account the factory acts as, for filter.creator: %w", err)
	}
	return login, nil
}

// verifyGitHubAccount reports the GitHub login the factory acts as, checking
// the configured token before it is trusted with anything: that GitHub
// accepts it, that it belongs to the login bees.toml names, and that it can
// read the repository. With [github] unset there is nothing to verify and the
// answer is simply the machine's own gh user.
//
// Every failure names the key to change and the way back to the machine's own
// account, because a token that cannot do these three things cannot run the
// factory at all.
func verifyGitHubAccount(ctx context.Context, cfg *config.Config) (string, error) {
	gh, minter, err := githubClient(cfg)
	if err != nil {
		return "", err
	}
	if minter != nil {
		return verifyApp(ctx, cfg, gh, minter)
	}
	return verifyAccount(ctx, cfg, gh)
}

// verifyApp is verifyAccount for a GitHub App: GitHub accepts the App ID
// and key, github.login is the App's login, the App is installed on the
// repository, and a token it mints can read it.
func verifyApp(ctx context.Context, cfg *config.Config, gh *github.Client, m *ghapp.Minter) (string, error) {
	slug, err := m.Slug(ctx)
	if err != nil {
		return "", fmt.Errorf("GitHub did not accept github.app_id %d with github.private_key: %w (check both on the GitHub App's settings page)", cfg.GitHub.AppID, err)
	}
	login := slug + config.BotSuffix
	if !strings.EqualFold(login, cfg.GitHub.Login) {
		return "", fmt.Errorf("github.app_id %d is the GitHub App %s, but github.login says %q: set github.login = %q", cfg.GitHub.AppID, login, cfg.GitHub.Login, login)
	}
	if _, err := m.Installation(ctx); err != nil {
		return "", fmt.Errorf("%w (install it from the GitHub App's settings page, with access to %s)", err, cfg.Project.Repo)
	}
	if _, err := gh.DefaultBranch(ctx); err != nil {
		return "", fmt.Errorf("github.login %s cannot read %s: %w (grant the GitHub App the repository permissions bees needs)", login, cfg.Project.Repo, err)
	}
	return login, nil
}

// verifyAccount is verifyGitHubAccount against a given client, so the checks
// can be tested without a gh on the machine.
func verifyAccount(ctx context.Context, cfg *config.Config, gh *github.Client) (string, error) {
	if !cfg.GitHub.Configured() {
		// Nothing to verify, and nothing worth failing over: bees init works
		// with a gh that cannot answer "who am I", so a lookup that fails
		// only costs the line that names the account.
		login, _ := meLookup(ctx)
		return login, nil
	}
	login, err := gh.Login(ctx)
	if err != nil {
		return "", fmt.Errorf("github.token was not accepted by GitHub: %w (check the token, or remove github.login and github.token to act as your own gh account)", err)
	}
	if !strings.EqualFold(login, cfg.GitHub.Login) {
		return "", fmt.Errorf("github.token belongs to %q but github.login says %q: correct one of them", login, cfg.GitHub.Login)
	}
	if _, err := gh.DefaultBranch(ctx); err != nil {
		return "", fmt.Errorf("github.login %s cannot read %s: %w (the token needs repo access to it)", login, cfg.Project.Repo, err)
	}
	return login, nil
}

// actingAs is the "acting as" clause `bees status` prints. It is only shown
// when the factory acts as an account of its own: with [github] unset there
// is nothing to say that the reader does not already know, and finding out
// would cost an API call on every status.
func actingAs(cfg *config.Config) string {
	if !cfg.GitHub.Configured() {
		return ""
	}
	return "   acting as: " + cfg.GitHub.Login
}
