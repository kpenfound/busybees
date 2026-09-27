// Package ghapptest is a fake of the GitHub API a GitHub App mints its
// installation tokens from, for tests of internal/ghapp and its callers.
package ghapptest

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kpenfound/busybees/internal/ghapp"
)

var (
	keyOnce sync.Once
	key     *rsa.PrivateKey
)

// Key is a private key for a fake App, generated once per test binary.
func Key(t testing.TB) (*rsa.PrivateKey, []byte) {
	t.Helper()
	keyOnce.Do(func() {
		var err error
		if key, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
			panic(err)
		}
	})
	return key, pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// Server is a GitHub App's side of the GitHub API: /app, the App's
// installation on Repo, and its access tokens. Every request must carry a
// JWT the App's key signed.
type Server struct {
	*httptest.Server
	AppID        int64
	Slug         string
	Repo         string
	Installation int64
	PEM          []byte
	// TTL is how long a minted token lasts; an hour by default.
	TTL time.Duration

	// Clock is the time a minted token's expiry counts from.
	Clock *Clock

	mu     sync.Mutex
	minted []Mint
}

// Clock is real time moved by an offset, safe for concurrent use: the one
// clock a test hands the Server, a Minter and a FileSource, so that they
// agree when a token expires.
type Clock struct {
	mu     sync.Mutex
	offset time.Duration
}

// Now is the time on the clock.
func (c *Clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

// Advance moves the clock on by d.
func (c *Clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.offset += d
}

// Mint is one token the Server minted.
type Mint struct {
	Token        string
	Repositories []string
}

// New starts a Server for the App "busybees" installed on repo.
func New(t testing.TB, repo string) *Server {
	t.Helper()
	k, p := Key(t)
	s := &Server{AppID: 4242, Slug: "busybees", Repo: repo, Installation: 99, PEM: p, TTL: time.Hour, Clock: &Clock{}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { s.serve(w, r, &k.PublicKey) }))
	t.Cleanup(s.Close)
	return s
}

// Minter is a Minter for the Server's App on the Server's clock, writing
// tokens into dir.
func (s *Server) Minter(t testing.TB, dir string) *ghapp.Minter {
	t.Helper()
	m, err := ghapp.NewMinter(s.AppID, s.PEM, s.Repo, dir)
	if err != nil {
		t.Fatal(err)
	}
	m.API, m.HTTP, m.Now = s.URL, s.Client(), s.Clock.Now
	return m
}

// Minted is every token minted so far, oldest first.
func (s *Server) Minted() []Mint {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Mint(nil), s.minted...)
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request, pub *rsa.PublicKey) {
	if err := s.verify(r.Header.Get("Authorization"), pub); err != nil {
		http.Error(w, `{"message":"`+err.Error()+`"}`, http.StatusUnauthorized)
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/app":
		writeJSON(w, map[string]any{"id": s.AppID, "slug": s.Slug})
	case r.Method == http.MethodGet && r.URL.Path == "/repos/"+s.Repo+"/installation":
		writeJSON(w, map[string]any{"id": s.Installation})
	case r.Method == http.MethodPost && r.URL.Path == "/app/installations/"+strconv.FormatInt(s.Installation, 10)+"/access_tokens":
		var body struct {
			Repositories []string `json:"repositories"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		s.mu.Lock()
		tok := fmt.Sprintf("ghs_%d", len(s.minted)+1)
		s.minted = append(s.minted, Mint{Token: tok, Repositories: body.Repositories})
		exp := s.Clock.Now().Add(s.TTL)
		s.mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		writeJSON(w, map[string]any{"token": tok, "expires_at": exp.UTC().Format(time.RFC3339)})
	default:
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	}
}

// verify checks a request's JWT: RS256, signed by the App's key, issued by
// its ID, and current.
func (s *Server) verify(header string, pub *rsa.PublicKey) error {
	jwt, ok := strings.CutPrefix(header, "Bearer ")
	if !ok {
		return fmt.Errorf("no bearer token")
	}
	parts := strings.Split(jwt, ".")
	if len(parts) != 3 {
		return fmt.Errorf("not a JWT")
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return err
	}
	sum := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, sum[:], sig); err != nil {
		return fmt.Errorf("bad signature")
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return err
	}
	var claims struct {
		Iss string `json:"iss"`
		Iat int64  `json:"iat"`
		Exp int64  `json:"exp"`
	}
	if err := json.Unmarshal(raw, &claims); err != nil {
		return err
	}
	if claims.Iss != strconv.FormatInt(s.AppID, 10) {
		return fmt.Errorf("issuer %q", claims.Iss)
	}
	if claims.Exp-claims.Iat > 10*60 {
		return fmt.Errorf("JWT lives longer than ten minutes")
	}
	return nil
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
