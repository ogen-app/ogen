package server

import (
	"cmp"
	"log/slog"
	"strings"

	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/usecase/inviteaccept"
	"github.com/ogen-app/ogen/src/usecase/oauthlogin"
)

// googleCallbackPath is the callback route; the default redirect URL appends it
// to APP_BASE_URL.
const googleCallbackPath = "/api/auth/google/callback"

// newGoogleLogin builds the Google sign-in service, or returns nil (Google off)
// when the OAuth client is not configured.
func newGoogleLogin(d *deps) *oauthlogin.Service {
	cfg, r := d.cfg, d.r
	if cfg.GoogleOAuthClientID == "" || cfg.GoogleOAuthClientSecret == "" {
		return nil
	}
	provider, err := oauthlogin.NewGoogle(oauthlogin.GoogleConfig{
		ClientID:     cfg.GoogleOAuthClientID,
		ClientSecret: cfg.GoogleOAuthClientSecret,
		RedirectURL:  cmp.Or(cfg.GoogleOAuthRedirectURL, strings.TrimRight(cfg.AppBaseURL, "/")+googleCallbackPath),
	})
	if err != nil {
		slog.Error("google sign-in disabled", logging.AttrComponent, "server", logging.AttrError, err)
		return nil
	}
	return oauthlogin.New(oauthlogin.Deps{
		DB:         d.db,
		Provider:   provider,
		OAuth:      r.oauthRepo,
		Identities: r.accountIdentityRepo,
		Accounts:   r.accountRepo,
		Users:      r.userRepo,
		Sessions:   r.sessionRepo,
		Signup:     d.svc.signup,
		Emails:     d.enqueuer,
		Invites:    inviteaccept.New(d.db, r.userRepo, r.accountRepo, r.invitationRepo, r.sessionRepo, d.entitlements.limiter),
	})
}
