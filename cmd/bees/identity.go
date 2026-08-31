package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// meLookup answers "who is running bees?". It is a variable so tests can
// replace it, and it deliberately goes through github.CurrentUser, which
// carries no token: see resolveFilterAssignee.
var meLookup = github.CurrentUser

// githubClient builds the client every gh call the orchestrator makes goes
// through: the account [github] configures, or the machine's own gh
// authentication when the table is unset. config.Validate has already
// rejected a token that expands to nothing, so an empty token here means the
// operator asked for today's behaviour.
func githubClient(cfg *config.Config) *github.Client {
	return github.NewWithToken(cfg.Project.Repo, cfg.GitHub.ResolvedToken())
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
	return verifyAccount(ctx, cfg, githubClient(cfg))
}

// verifyAccount is verifyGitHubAccount against a given client, so the checks
// can be tested without a gh on the machine.
func verifyAccount(ctx context.Context, cfg *config.Config, gh *github.Client) (string, error) {
	if !cfg.GitHub.Configured() {
		// Nothing to verify, and nothing worth failing over: bees init has
		// always worked with a gh that cannot answer "who am I", so a lookup
		// that fails only costs the line that names the account.
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
