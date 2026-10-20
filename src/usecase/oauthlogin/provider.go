package oauthlogin

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/oauth2"
)

// Claims is what a verified ID token says about the person signing in.
type Claims struct {
	Subject       string
	Email         string
	EmailVerified bool
	Name          string
	// HostedDomain is the Google Workspace domain (the hd claim), "" for a
	// consumer account.
	HostedDomain string
}

// Authoritative reports whether the provider vouches for the address as
// belonging to this person now, which is what linking an existing account by
// email needs. Google is authoritative for its own consumer addresses and for
// a Workspace account's own domain. For any other address email_verified
// only records a check made when the Google account was created; the
// address may have changed hands since (a former employee, a lapsed domain).
func (c *Claims) Authoritative() bool {
	_, domain, ok := strings.Cut(strings.ToLower(c.Email), "@")
	if !ok || !c.EmailVerified {
		return false
	}
	return domain == "gmail.com" || domain == "googlemail.com" ||
		(c.HostedDomain != "" && strings.EqualFold(c.HostedDomain, domain))
}

// Provider is one OpenID Connect sign-in provider.
type Provider interface {
	// Name is the provider key stored on identities, e.g. "google".
	Name() string
	// AuthCodeURL is where the browser is sent to sign in. The nonce binds the
	// ID token to this flow and the PKCE verifier binds the code exchange.
	AuthCodeURL(state, nonce, verifier string) string
	// Exchange trades the authorization code for a verified ID token. It returns
	// ErrTokenInvalid when the token fails verification (signature, audience,
	// issuer, expiry or nonce) and any other error when the provider could not
	// be reached or refused the exchange.
	Exchange(ctx context.Context, code, verifier, nonce string) (*Claims, error)
}

// ErrTokenInvalid reports an ID token that failed verification.
var ErrTokenInvalid = errors.New("oauthlogin: id token failed verification")

// Google's published OpenID Connect endpoints.
const (
	googleIssuer   = "https://accounts.google.com"
	googleAuthURL  = "https://accounts.google.com/o/oauth2/v2/auth"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	googleJWKSURL  = "https://www.googleapis.com/oauth2/v3/certs"
)

// exchangeTimeout bounds the code exchange and any key fetch it triggers.
const exchangeTimeout = 10 * time.Second

// GoogleConfig configures the Google provider. The endpoint fields default to
// Google's and exist so tests can point the provider at a local issuer.
type GoogleConfig struct {
	ClientID     string
	ClientSecret string
	RedirectURL  string

	Issuer   string
	AuthURL  string
	TokenURL string
	JWKSURL  string
	// HTTPClient makes the exchange and key fetches; nil uses a client with
	// exchangeTimeout.
	HTTPClient *http.Client
}

type oidcProvider struct {
	name     string
	oauth    oauth2.Config
	verifier *oidc.IDTokenVerifier
	client   *http.Client
}

// NewGoogle returns the Google provider. It makes no network call: the
// endpoints are Google's published ones and signing keys are fetched on first
// use, so a Google outage can't stop the API from starting.
func NewGoogle(cfg GoogleConfig) (Provider, error) {
	if cfg.ClientID == "" || cfg.ClientSecret == "" || cfg.RedirectURL == "" {
		return nil, errors.New("oauthlogin: google client id, secret and redirect url are required")
	}
	issuer := cmp.Or(cfg.Issuer, googleIssuer)
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: exchangeTimeout}
	}
	keyCtx := oidc.ClientContext(context.Background(), client)
	keys := oidc.NewRemoteKeySet(keyCtx, cmp.Or(cfg.JWKSURL, googleJWKSURL))
	return &oidcProvider{
		name: "google",
		oauth: oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			RedirectURL:  cfg.RedirectURL,
			Endpoint: oauth2.Endpoint{
				AuthURL:   cmp.Or(cfg.AuthURL, googleAuthURL),
				TokenURL:  cmp.Or(cfg.TokenURL, googleTokenURL),
				AuthStyle: oauth2.AuthStyleInParams,
			},
			Scopes: []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier: oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: cfg.ClientID}),
		client:   client,
	}, nil
}

func (p *oidcProvider) Name() string { return p.name }

func (p *oidcProvider) AuthCodeURL(state, nonce, verifier string) string {
	return p.oauth.AuthCodeURL(state,
		oidc.Nonce(nonce),
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "select_account"),
	)
}

func (p *oidcProvider) Exchange(ctx context.Context, code, verifier, nonce string) (claims *Claims, err error) {
	ctx, span := tracer.Start(ctx, "oauthlogin.exchange", trace.WithAttributes(attribute.String("provider", p.name)))
	defer func() {
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, "exchange failed")
		}
		span.End()
	}()
	ctx, cancel := context.WithTimeout(ctx, exchangeTimeout)
	defer cancel()
	ctx = oidc.ClientContext(ctx, p.client)
	return p.exchange(ctx, code, verifier, nonce)
}

// tracer spans the code exchange; the token and key fetches inside it are
// client spans of the traced default transport.
var tracer = otel.Tracer("ogen/oauthlogin")

func (p *oidcProvider) exchange(ctx context.Context, code, verifier, nonce string) (*Claims, error) {
	tok, err := p.oauth.Exchange(ctx, code, oauth2.VerifierOption(verifier))
	if err != nil {
		return nil, fmt.Errorf("oauthlogin: exchange code: %w", err)
	}
	raw, ok := tok.Extra("id_token").(string)
	if !ok || raw == "" {
		return nil, fmt.Errorf("%w: no id_token in token response", ErrTokenInvalid)
	}
	idToken, err := p.verifier.Verify(ctx, raw)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTokenInvalid, err)
	}
	if idToken.Nonce != nonce {
		return nil, fmt.Errorf("%w: nonce mismatch", ErrTokenInvalid)
	}
	var c struct {
		Email         string    `json:"email"`
		EmailVerified boolClaim `json:"email_verified"`
		Name          string    `json:"name"`
		GivenName     string    `json:"given_name"`
		HostedDomain  string    `json:"hd"`
	}
	if err := idToken.Claims(&c); err != nil {
		return nil, fmt.Errorf("%w: claims: %w", ErrTokenInvalid, err)
	}
	if idToken.Subject == "" || c.Email == "" {
		return nil, fmt.Errorf("%w: missing sub or email", ErrTokenInvalid)
	}
	return &Claims{
		Subject:       idToken.Subject,
		Email:         c.Email,
		EmailVerified: bool(c.EmailVerified),
		Name:          cmp.Or(c.Name, c.GivenName),
		HostedDomain:  c.HostedDomain,
	}, nil
}

// boolClaim decodes email_verified, which Google sends as a JSON bool but
// some OIDC providers send as the string "true".
type boolClaim bool

func (b *boolClaim) UnmarshalJSON(data []byte) error {
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		return err
	}
	switch t := v.(type) {
	case bool:
		*b = boolClaim(t)
	case string:
		*b = t == "true"
	}
	return nil
}
