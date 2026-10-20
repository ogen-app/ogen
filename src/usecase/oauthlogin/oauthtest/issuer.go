// Package oauthtest is a local OpenID Connect issuer for tests. It plays
// Google's part of the authorization-code flow: Authorize stands in for the
// user signing in on the provider's page, and the token endpoint checks the
// client credentials and PKCE verifier before issuing a signed ID token.
package oauthtest

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
)

const (
	ClientID     = "test-client"
	ClientSecret = "test-secret"
	RedirectURL  = "http://app.test/api/auth/google/callback"
)

// TB is the part of testing.TB the issuer needs; Ginkgo's GinkgoT() satisfies
// it too.
type TB interface {
	Helper()
	Fatal(args ...any)
	Fatalf(format string, args ...any)
	Cleanup(func())
}

// Identity is who signs in at the issuer.
type Identity struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
}

type grant struct {
	identity  Identity
	nonce     string
	challenge string
}

// Issuer is a running fake provider.
type Issuer struct {
	srv    *httptest.Server
	key    *rsa.PrivateKey
	mu     sync.Mutex
	grants map[string]grant
	// Tamper, when set, edits the ID token claims before signing, so a test can
	// forge a bad audience, issuer, nonce or expiry.
	Tamper func(claims map[string]any)
}

// New starts an issuer that stops with the test.
func New(t TB) *Issuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	iss := &Issuer{key: key, grants: map[string]grant{}}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /token", iss.token)
	mux.HandleFunc("GET /jwks", iss.jwks)
	iss.srv = httptest.NewServer(mux)
	t.Cleanup(iss.srv.Close)
	return iss
}

// URL is the issuer identifier.
func (i *Issuer) URL() string { return i.srv.URL }

// Provider returns a Google provider pointed at this issuer.
func (i *Issuer) Provider(t TB) oauthlogin.Provider {
	t.Helper()
	p, err := oauthlogin.NewGoogle(oauthlogin.GoogleConfig{
		ClientID:     ClientID,
		ClientSecret: ClientSecret,
		RedirectURL:  RedirectURL,
		Issuer:       i.srv.URL,
		AuthURL:      i.srv.URL + "/auth",
		TokenURL:     i.srv.URL + "/token",
		JWKSURL:      i.srv.URL + "/jwks",
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// Authorize plays the user signing in as who on the provider page reached
// through authURL, and returns the authorization code and state the provider
// redirects back with.
func (i *Issuer) Authorize(t TB, authURL string, who Identity) (code, state string) {
	t.Helper()
	u, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("client_id") != ClientID || q.Get("redirect_uri") != RedirectURL || q.Get("code_challenge_method") != "S256" {
		t.Fatalf("unexpected authorization request: %s", authURL)
	}
	code = rand.Text()
	i.mu.Lock()
	i.grants[code] = grant{identity: who, nonce: q.Get("nonce"), challenge: q.Get("code_challenge")}
	i.mu.Unlock()
	return code, q.Get("state")
}

func (i *Issuer) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if r.PostForm.Get("client_id") != ClientID || r.PostForm.Get("client_secret") != ClientSecret {
		oauthError(w, "invalid_client")
		return
	}
	code := r.PostForm.Get("code")
	i.mu.Lock()
	g, ok := i.grants[code]
	delete(i.grants, code)
	i.mu.Unlock()
	sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
	if !ok || base64.RawURLEncoding.EncodeToString(sum[:]) != g.challenge {
		oauthError(w, "invalid_grant")
		return
	}
	now := time.Now()
	claims := map[string]any{
		"iss":            i.srv.URL,
		"aud":            ClientID,
		"sub":            g.identity.Subject,
		"email":          g.identity.Email,
		"email_verified": g.identity.EmailVerified,
		"name":           g.identity.Name,
		"nonce":          g.nonce,
		"iat":            now.Unix(),
		"exp":            now.Add(time.Hour).Unix(),
	}
	if i.Tamper != nil {
		i.Tamper(claims)
	}
	idToken, err := i.sign(claims)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "at-" + code,
		"token_type":   "Bearer",
		"expires_in":   3600,
		"id_token":     idToken,
	})
}

func (i *Issuer) sign(claims map[string]any) (string, error) {
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: i.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test-key"))
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return obj.CompactSerialize()
}

func (i *Issuer) jwks(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
		{Key: &i.key.PublicKey, KeyID: "test-key", Algorithm: string(jose.RS256), Use: "sig"},
	}})
}

func oauthError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
