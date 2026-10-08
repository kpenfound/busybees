package ghapp_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/ghapp"
	"github.com/kpenfound/busybees/internal/ghapp/ghapptest"
)

// TestMintsOnlyWhenExpired: a token is minted on first use and again only
// once the cached one is within Skew of expiring, however many calls come
// in between, and concurrent callers share one mint.
//
// ghapptest.Server checks the JWT's signature, issuer and lifetime and
// records the repositories each mint asked for, so it is not an
// unconditional success; it does not model GitHub actually scoping the
// returned token's permissions to those repositories, rate limiting, or an
// installation spanning more than one repository, and no test here drives
// its unauthorized (bad-signature) branch.
func TestMintsOnlyWhenExpired(t *testing.T) {
	srv := ghapptest.New(t, "acme/widgets")
	dir := t.TempDir()
	m := srv.Minter(t, dir)
	c := srv.Clock
	ctx := context.Background()

	var wg sync.WaitGroup
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if tok, err := m.Token(ctx); err != nil || tok != "ghs_1" {
				t.Errorf("Token = %q, %v", tok, err)
			}
		}()
	}
	wg.Wait()
	c.Advance(time.Hour - ghapp.Skew - time.Second)
	if tok, _ := m.Token(ctx); tok != "ghs_1" {
		t.Errorf("a token with more than Skew left was replaced: %q", tok)
	}
	c.Advance(2 * time.Second)
	if tok, _ := m.Token(ctx); tok != "ghs_2" {
		t.Errorf("an expired token was not replaced: %q", tok)
	}
	minted := srv.Minted()
	if len(minted) != 2 {
		t.Fatalf("minted %d tokens, want 2", len(minted))
	}
	// Each is restricted to the one repository.
	for _, mint := range minted {
		if !slices.Equal(mint.Repositories, []string{"widgets"}) {
			t.Errorf("token restricted to %q, want [widgets]", mint.Repositories)
		}
	}
	// And the current one is where sessions read it, for its owner alone.
	got, err := ghapp.ReadToken(dir)
	if err != nil || got.Value != "ghs_2" {
		t.Errorf("token file: %+v, %v", got, err)
	}
	if fi, err := os.Stat(filepath.Join(dir, ghapp.TokenFile)); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("token file mode: %v, %v", fi.Mode(), err)
	}
}

// TestSlugAndInstallation: the two questions bees init and bees doctor ask
// of the App, and the answer when it is not installed on the repository
// (permission denial). The fake answers "not installed" with a plain 404,
// as GitHub does for a repository the App cannot see; it does not model a
// repository that exists but the App lacks a specific permission on.
func TestSlugAndInstallation(t *testing.T) {
	srv := ghapptest.New(t, "acme/widgets")
	ctx := context.Background()
	m := srv.Minter(t, "")
	if slug, err := m.Slug(ctx); err != nil || slug != "busybees" {
		t.Errorf("Slug = %q, %v", slug, err)
	}
	if id, err := m.Installation(ctx); err != nil || id != 99 {
		t.Errorf("Installation = %d, %v", id, err)
	}
	m.Repo = "acme/other"
	if _, err := m.Installation(ctx); err == nil || !strings.Contains(err.Error(), "not installed on acme/other") {
		t.Errorf("an App not installed on the repository: %v", err)
	}
}

// TestParseKeyRefusesWhatIsNotAKey names the key in its error.
func TestParseKeyRefusesWhatIsNotAKey(t *testing.T) {
	if _, err := ghapp.ParseKey([]byte("not a key")); err == nil || !strings.Contains(err.Error(), "github.private_key") {
		t.Errorf("ParseKey: %v", err)
	}
}

// held starts a Minter answering sessions' requests in a fresh directory.
func held(t *testing.T) (*ghapptest.Server, *ghapp.Minter) {
	t.Helper()
	srv := ghapptest.New(t, "acme/widgets")
	m := srv.Minter(t, t.TempDir())
	m.Poll = 10 * time.Millisecond
	release, err := m.Hold()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(release)
	return srv, m
}

// TestFileSourceAsksWhenExpired: a session reads the token the Minter keeps
// in the directory, and asks for a new one only when that has expired.
func TestFileSourceAsksWhenExpired(t *testing.T) {
	srv, m := held(t)
	ctx := context.Background()
	f := &ghapp.FileSource{Dir: m.Dir, Poll: 10 * time.Millisecond, Wait: 5 * time.Second, Now: srv.Clock.Now}

	// Nothing minted yet: the first use asks.
	if tok, err := f.Token(ctx); err != nil || tok != "ghs_1" {
		t.Fatalf("Token = %q, %v", tok, err)
	}
	// Used again while fresh, it asks for nothing.
	if tok, _ := f.Token(ctx); tok != "ghs_1" || len(srv.Minted()) != 1 {
		t.Errorf("a fresh token was replaced: %q after %d mints", tok, len(srv.Minted()))
	}
	if _, err := os.Stat(filepath.Join(m.Dir, ghapp.RefreshFile)); !os.IsNotExist(err) {
		t.Errorf("the answered request is still there: %v", err)
	}
	// An hour on, it has expired: the session asks, and the Minter mints.
	srv.Clock.Advance(time.Hour)
	if tok, err := f.Token(ctx); err != nil || tok != "ghs_2" {
		t.Errorf("Token after expiry = %q, %v", tok, err)
	}
}

// TestFileSourceWithNoMinter fails with what to look at rather than waiting
// forever.
func TestFileSourceWithNoMinter(t *testing.T) {
	f := &ghapp.FileSource{Dir: t.TempDir(), Poll: 10 * time.Millisecond, Wait: 50 * time.Millisecond}
	if _, err := f.Token(context.Background()); err == nil || !strings.Contains(err.Error(), "bees process that started this session") {
		t.Errorf("Token with no Minter: %v", err)
	}
}

// TestHoldStopsWithTheLastRelease: the Minter answers while any session
// holds it and stops when the last one lets go.
func TestHoldStopsWithTheLastRelease(t *testing.T) {
	srv := ghapptest.New(t, "acme/widgets")
	m := srv.Minter(t, t.TempDir())
	m.Poll = 10 * time.Millisecond
	one, err := m.Hold()
	if err != nil {
		t.Fatal(err)
	}
	two, _ := m.Hold()
	one()
	one() // a second release of the same hold changes nothing
	f := &ghapp.FileSource{Dir: m.Dir, Poll: 10 * time.Millisecond, Wait: 5 * time.Second}
	if _, err := f.Token(context.Background()); err != nil {
		t.Fatalf("still held once: %v", err)
	}
	two()
	if err := os.Remove(filepath.Join(m.Dir, ghapp.TokenFile)); err != nil {
		t.Fatal(err)
	}
	f.Wait = 100 * time.Millisecond
	if _, err := f.Token(context.Background()); err == nil {
		t.Error("answered with no session holding the Minter")
	}
}

// TestScripts runs the shell side of the handoff: token.sh answers with the
// token and asks for a new one when it has expired, credential.sh answers
// git for github.com alone, and bin/gh runs the next gh on PATH with the
// token.
func TestScripts(t *testing.T) {
	srv, m := held(t)

	run := func(t *testing.T, stdin string, env []string, name string, args ...string) string {
		t.Helper()
		cmd := exec.Command("sh", append([]string{filepath.Join(m.Dir, name)}, args...)...)
		cmd.Stdin = strings.NewReader(stdin)
		cmd.Env = append(os.Environ(), env...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s: %v: %s", name, err, out)
		}
		return string(out)
	}

	t.Run("token.sh asks for the first token", func(t *testing.T) {
		if out := run(t, "", nil, ghapp.TokenScript); out != "ghs_1\n" {
			t.Errorf("token.sh = %q", out)
		}
	})
	t.Run("token.sh asks again once it has expired", func(t *testing.T) {
		// A token file that expires within Skew by the script's clock,
		// date(1), and a cached token the Minter's clock calls expired too:
		// the script asks and waits for the next one.
		if err := ghapp.WriteToken(m.Dir, ghapp.Token{Value: "ghs_old", ExpiresAt: time.Now().Add(time.Minute)}); err != nil {
			t.Fatal(err)
		}
		srv.Clock.Advance(time.Hour)
		if out := run(t, "", nil, ghapp.TokenScript); out != "ghs_2\n" {
			t.Errorf("token.sh = %q", out)
		}
		if n := len(srv.Minted()); n != 2 {
			t.Errorf("minted %d tokens, want 2", n)
		}
	})
	t.Run("credential.sh", func(t *testing.T) {
		out := run(t, "protocol=https\nhost=github.com\n\n", nil, ghapp.CredentialFile, "get")
		if out != "username=x-access-token\npassword=ghs_2\n" {
			t.Errorf("credential.sh get = %q", out)
		}
		if out := run(t, "protocol=https\nhost=gitlab.com\n\n", nil, ghapp.CredentialFile, "get"); out != "" {
			t.Errorf("credential.sh answered for another host: %q", out)
		}
		if out := run(t, "", nil, ghapp.CredentialFile, "store"); out != "" {
			t.Errorf("credential.sh store = %q", out)
		}
	})
	t.Run("bin/gh", func(t *testing.T) {
		fake := t.TempDir()
		if err := os.WriteFile(filepath.Join(fake, "gh"), []byte("#!/bin/sh\necho \"token=$GH_TOKEN args=$*\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		bin := filepath.Join(m.Dir, ghapp.BinDir)
		cmd := exec.Command(filepath.Join(bin, "gh"), "api", "user")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+fake+":/usr/bin:/bin", "GH_TOKEN=the-machines-own")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("gh: %v: %s", err, out)
		}
		if string(out) != "token=ghs_2 args=api user\n" {
			t.Errorf("gh = %q", out)
		}
	})
}
