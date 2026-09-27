package ghapp

import (
	"github.com/kpenfound/busybees/internal/config"
	"github.com/kpenfound/busybees/internal/github"
)

// NewClient is the client the factory's gh calls for repo go through, as
// [github] configures it: the configured token, the machine's own gh
// authentication when the table is unset, or a GitHub App's installation
// tokens. For a GitHub App it also returns the Minter those come from, which
// a session runner holds so its sessions get tokens too; inSession, a
// process a session started, gets no Minter and never reads the private key,
// and reads the tokens the session's bees process keeps in stateDir.
func NewClient(g config.GitHub, repo, stateDir string, inSession bool) (*github.Client, *Minter, error) {
	if !g.App() {
		return github.NewAs(repo, g.Login, g.ResolvedToken()), nil, nil
	}
	dir := Dir(stateDir)
	if inSession {
		return github.NewApp(repo, g.Login, &FileSource{Dir: dir}), nil, nil
	}
	key, err := g.ResolvedPrivateKey()
	if err != nil {
		return nil, nil, err
	}
	m, err := NewMinter(g.AppID, key, repo, dir)
	if err != nil {
		return nil, nil, err
	}
	return github.NewApp(repo, g.Login, m), m, nil
}
