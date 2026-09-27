// Package ghapp acts as a GitHub App ([github] app_id and private_key): it
// mints the App's installation tokens for one repository and hands them to
// the sessions bees starts.
//
// The process that runs sessions holds the private key in a Minter. Sessions
// never see the key. They read the current token from a file in the state
// directory, which every sandbox mounts, and ask for a new one by creating a
// second file there once the one they have has expired. The Minter answers
// those requests for as long as a session runs (Hold). Whoever asks, a token
// is minted only when the cached one has expired: GitHub's last an hour.
//
// The directory (Dir) holds:
//
//	token          "<expiry, Unix seconds> <token>", the current token
//	refresh        present while a session waits for a new token
//	token.sh       prints a current token, asking for one when it must
//	credential.sh  a git credential helper answering with that token
//	bin/gh         gh with that token, first on a session's PATH
package ghapp

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Skew is how long before its expiry a token counts as expired: a git push
// or a paginated gh call that starts with seconds left would fail halfway.
const Skew = 2 * time.Minute

// DefaultAPI is GitHub's REST API.
const DefaultAPI = "https://api.github.com"

// Files in Dir.
const (
	TokenFile      = "token"
	RefreshFile    = "refresh"
	TokenScript    = "token.sh"
	CredentialFile = "credential.sh"
	BinDir         = "bin"
)

// Dir is where a state directory keeps the App's token for its sessions.
func Dir(stateDir string) string { return filepath.Join(stateDir, "github") }

// Token is an installation token and when GitHub stops accepting it.
type Token struct {
	Value     string
	ExpiresAt time.Time
}

// fresh reports whether t has more than margin left at now.
func (t Token) fresh(now time.Time, margin time.Duration) bool {
	return t.Value != "" && t.ExpiresAt.Sub(now) > margin
}

// Minter mints installation tokens of one GitHub App, restricted to one
// repository. It is safe for concurrent use; concurrent callers that find
// the token expired share one mint.
type Minter struct {
	AppID int64
	Key   *rsa.PrivateKey
	// Repo is the repository tokens are restricted to, "owner/name". The
	// App's installation on it is looked up once.
	Repo string
	// Dir is where every minted token is written for sessions, and where
	// Hold answers their requests. "" writes nothing.
	Dir string
	// API is GitHub's REST API; DefaultAPI when empty.
	API string
	// HTTP makes the requests; http.DefaultClient when nil.
	HTTP *http.Client
	// Now is the clock; time.Now when nil.
	Now func() time.Time
	// Logger reports a failed answer to a session's request.
	Logger *slog.Logger
	// Poll is how often Hold looks for a request; a second when zero.
	Poll time.Duration

	mu           sync.Mutex
	installation int64
	token        Token

	holdMu sync.Mutex
	holds  int
	stop   context.CancelFunc
	done   chan struct{}
}

// NewMinter returns a Minter for the App with the given ID and PEM private
// key (PKCS #1 or PKCS #8, as GitHub hands it out).
func NewMinter(appID int64, key []byte, repo, dir string) (*Minter, error) {
	k, err := ParseKey(key)
	if err != nil {
		return nil, err
	}
	return &Minter{AppID: appID, Key: k, Repo: repo, Dir: dir}, nil
}

// ParseKey reads a GitHub App's PEM private key.
func ParseKey(data []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, errors.New("github.private_key is not a PEM private key")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("github.private_key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("github.private_key is not an RSA key: download a private key from the GitHub App's settings page")
	}
	return rk, nil
}

func (m *Minter) now() time.Time {
	if m.Now != nil {
		return m.Now()
	}
	return time.Now()
}

// Token returns a token with more than Skew left: the cached one, or a new
// one when it has expired.
func (m *Minter) Token(ctx context.Context) (string, error) {
	t, err := m.current(ctx, Skew)
	return t.Value, err
}

// current returns the cached token when it has more than margin left, and
// mints and writes a new one otherwise.
func (m *Minter) current(ctx context.Context, margin time.Duration) (Token, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.token.fresh(m.now(), margin) {
		return m.token, nil
	}
	t, err := m.mint(ctx)
	if err != nil {
		return Token{}, err
	}
	m.token = t
	if m.Dir != "" {
		if err := WriteToken(m.Dir, t); err != nil {
			return Token{}, err
		}
	}
	return t, nil
}

// Slug is the App's slug, which GitHub reports its login as with the
// "[bot]" suffix. It answers whether GitHub accepts the App ID and key.
func (m *Minter) Slug(ctx context.Context) (string, error) {
	var app struct {
		Slug string `json:"slug"`
	}
	if err := m.call(ctx, http.MethodGet, "/app", nil, &app); err != nil {
		return "", err
	}
	return app.Slug, nil
}

// Installation is the ID of the App's installation on Repo. It answers
// whether the App is installed there.
func (m *Minter) Installation(ctx context.Context) (int64, error) {
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := m.call(ctx, http.MethodGet, "/repos/"+m.Repo+"/installation", nil, &inst); err != nil {
		return 0, fmt.Errorf("the GitHub App is not installed on %s: %w", m.Repo, err)
	}
	return inst.ID, nil
}

// mint asks GitHub for a new installation token restricted to Repo. The
// caller holds mu.
func (m *Minter) mint(ctx context.Context) (Token, error) {
	if m.installation == 0 {
		id, err := m.Installation(ctx)
		if err != nil {
			return Token{}, err
		}
		m.installation = id
	}
	_, name, _ := strings.Cut(m.Repo, "/")
	body := map[string]any{"repositories": []string{name}}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := "/app/installations/" + strconv.FormatInt(m.installation, 10) + "/access_tokens"
	if err := m.call(ctx, http.MethodPost, path, body, &out); err != nil {
		return Token{}, fmt.Errorf("mint a GitHub App installation token for %s: %w", m.Repo, err)
	}
	if out.Token == "" {
		return Token{}, fmt.Errorf("mint a GitHub App installation token for %s: GitHub answered no token", m.Repo)
	}
	return Token{Value: out.Token, ExpiresAt: out.ExpiresAt}, nil
}

// call makes one request to GitHub authenticated as the App itself.
func (m *Minter) call(ctx context.Context, method, path string, body, out any) error {
	jwt, err := m.jwt()
	if err != nil {
		return err
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	api := m.API
	if api == "" {
		api = DefaultAPI
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(api, "/")+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+jwt)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	client := m.HTTP
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(data, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(data))
		}
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, e.Message)
	}
	return json.Unmarshal(data, out)
}

// jwt is the short-lived token that authenticates the App itself, signed
// with its private key. It is backdated a minute against clock drift, as
// GitHub recommends, and lives the ten minutes GitHub allows less one.
func (m *Minter) jwt() (string, error) {
	now := m.now().Unix()
	enc := base64.RawURLEncoding
	header := enc.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, err := json.Marshal(map[string]any{"iat": now - 60, "exp": now + 9*60, "iss": strconv.FormatInt(m.AppID, 10)})
	if err != nil {
		return "", err
	}
	signed := header + "." + enc.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signed))
	sig, err := rsa.SignPKCS1v15(rand.Reader, m.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signed + "." + enc.EncodeToString(sig), nil
}

// Hold makes the Minter answer the requests of the sessions in Dir until
// every Hold is released, and writes the scripts they run. The session
// runner holds it for each session, so it answers exactly while one runs.
func (m *Minter) Hold() (release func(), err error) {
	if m.Dir == "" {
		return func() {}, nil
	}
	m.holdMu.Lock()
	defer m.holdMu.Unlock()
	if m.holds == 0 {
		if err := WriteScripts(m.Dir); err != nil {
			return nil, err
		}
		ctx, cancel := context.WithCancel(context.Background())
		m.stop, m.done = cancel, make(chan struct{})
		go m.serve(ctx, m.done)
	}
	m.holds++
	var once sync.Once
	return func() { once.Do(m.release) }, nil
}

func (m *Minter) release() {
	m.holdMu.Lock()
	defer m.holdMu.Unlock()
	m.holds--
	if m.holds > 0 {
		return
	}
	m.stop()
	<-m.done
}

// serve answers requests until ctx ends. A request is answered with the
// cached token when that is still good for twice Skew, so a session whose
// clock runs a little ahead is not left waiting for a token that the
// Minter's own clock calls fresh. After a failure it waits before trying
// again rather than asking GitHub every poll.
func (m *Minter) serve(ctx context.Context, done chan struct{}) {
	defer close(done)
	poll := m.Poll
	if poll == 0 {
		poll = time.Second
	}
	tick := time.NewTicker(poll)
	defer tick.Stop()
	var retryAt time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if _, err := os.Stat(filepath.Join(m.Dir, RefreshFile)); err != nil || m.now().Before(retryAt) {
			continue
		}
		t, err := m.current(ctx, 2*Skew)
		if err == nil {
			// The file may hold an older token than the cache, written by
			// another bees process on the same state directory.
			err = WriteToken(m.Dir, t)
		}
		if err != nil {
			retryAt = m.now().Add(30 * time.Second)
			if m.Logger != nil && ctx.Err() == nil {
				m.Logger.Warn("could not give a session a GitHub App token", "err", err)
			}
			continue
		}
		_ = os.Remove(filepath.Join(m.Dir, RefreshFile))
	}
}

// WriteToken replaces the token file in dir, readable by its owner alone.
// It is written whole and renamed into place, so a reader never sees half.
func WriteToken(dir string, t Token) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := fmt.Fprintf(tmp, "%d %s\n", t.ExpiresAt.Unix(), t.Value); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, TokenFile))
}

// ReadToken reads the token file in dir.
func ReadToken(dir string) (Token, error) {
	data, err := os.ReadFile(filepath.Join(dir, TokenFile))
	if err != nil {
		return Token{}, err
	}
	exp, value, ok := strings.Cut(strings.TrimSpace(string(data)), " ")
	sec, err := strconv.ParseInt(exp, 10, 64)
	if !ok || err != nil || value == "" {
		return Token{}, fmt.Errorf("%s: not a token file", filepath.Join(dir, TokenFile))
	}
	return Token{Value: value, ExpiresAt: time.Unix(sec, 0)}, nil
}

// FileSource is the token a session has: the one in Dir while it has more
// than Skew left, and otherwise a new one, asked of the Minter holding Dir.
// It is what `bees` commands inside a session authenticate with, and does
// in Go what token.sh does for gh and git.
type FileSource struct {
	Dir string
	// Wait bounds how long a request waits for an answer; a minute when
	// zero.
	Wait time.Duration
	// Poll is how often the token file is read while waiting; a second
	// when zero.
	Poll time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Token returns a token with more than Skew left.
func (f *FileSource) Token(ctx context.Context) (string, error) {
	now := time.Now
	if f.Now != nil {
		now = f.Now
	}
	wait, poll := f.Wait, f.Poll
	if wait == 0 {
		wait = time.Minute
	}
	if poll == 0 {
		poll = time.Second
	}
	deadline := time.Now().Add(wait)
	for {
		if t, err := ReadToken(f.Dir); err == nil && t.fresh(now(), Skew) {
			return t.Value, nil
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("no GitHub App token in %s after %s: the bees process that started this session answers for it, and it did not", f.Dir, wait)
		}
		// Asked again on every poll, in case the Minter answered an
		// earlier request with a token this clock already calls expired.
		if err := os.WriteFile(filepath.Join(f.Dir, RefreshFile), nil, 0o600); err != nil {
			return "", fmt.Errorf("ask for a GitHub App token: %w", err)
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(poll):
		}
	}
}

// WriteScripts writes the scripts sessions run into dir: token.sh,
// credential.sh and bin/gh. They are shell and read nothing but dir, so they
// work on the host and in every sandbox that mounts the state directory.
func WriteScripts(dir string) error {
	if err := os.MkdirAll(filepath.Join(dir, BinDir), 0o700); err != nil {
		return err
	}
	for name, body := range map[string]string{
		TokenScript:                 tokenScript,
		CredentialFile:              credentialScript,
		filepath.Join(BinDir, "gh"): ghScript,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
			return err
		}
	}
	return nil
}

// tokenScript is FileSource in shell.
var tokenScript = `#!/bin/sh
# Prints the factory's GitHub App token (bees): the one in this directory
# while it has more than ` + strconv.Itoa(int(Skew.Seconds())) + ` seconds left, and otherwise a new one, asked
# of the bees process that started the session.
dir=$(dirname "$0")
i=0
while :; do
  if [ -r "$dir/` + TokenFile + `" ]; then
    read -r exp tok < "$dir/` + TokenFile + `"
    case "$exp" in
      ''|*[!0-9]*) ;;
      *) if [ -n "$tok" ] && [ "$exp" -gt $(( $(date +%s) + ` + strconv.Itoa(int(Skew.Seconds())) + ` )) ]; then
           printf '%s\n' "$tok"
           exit 0
         fi ;;
    esac
  fi
  if [ "$i" -ge 60 ]; then
    echo "bees: no GitHub App token in $dir after 60s: the bees process that started this session answers for it, and it did not" >&2
    exit 1
  fi
  : > "$dir/` + RefreshFile + `"
  i=$((i + 1))
  sleep 1
done
`

// credentialScript answers git's "get" for github.com with the token.
const credentialScript = `#!/bin/sh
# A git credential helper answering for github.com with the factory's GitHub
# App token (bees).
[ "$1" = get ] || exit 0
host=
while IFS= read -r line && [ -n "$line" ]; do
  case "$line" in host=*) host=${line#host=} ;; esac
done
[ "$host" = github.com ] || exit 0
tok=$(sh "$(dirname "$0")/` + TokenScript + `") || exit 1
printf 'username=x-access-token\npassword=%s\n' "$tok"
`

// ghScript is gh with the token. It runs the first gh on PATH after its own
// directory, which the session runner puts first.
const ghScript = `#!/bin/sh
# gh with the factory's GitHub App token (bees).
here=$(dirname "$0")
tok=$(sh "$here/../` + TokenScript + `") || exit 1
real=
set -f
IFS=:
for d in $PATH; do
  [ "$d" = "$here" ] && continue
  if [ -x "$d/gh" ]; then
    real=$d/gh
    break
  fi
done
unset IFS
set +f
if [ -z "$real" ]; then
  echo "bees: gh is not installed" >&2
  exit 127
fi
GH_TOKEN=$tok exec "$real" "$@"
`
