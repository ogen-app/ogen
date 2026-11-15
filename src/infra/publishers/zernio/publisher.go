package zernio

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// PublisherID is the stable marker stamped on posts.publisher and
// returned by Publisher.ID(). It is the value CON-93's analytics
// pipeline filters on (`publisher = 'zernio'`) and matches the slug
// surfaced in API responses. Aliases models.PublisherZernio so the
// adapter and the repository layer share one source of truth.
const PublisherID = models.PublisherZernio

// Publisher adapts the Zernio integration to publishers.Publisher so
// the platforms handler can surface connection state alongside the
// local platforms metadata. One Publisher per integration is plenty
// — it's stateless beyond the wired collaborators.
type Publisher struct {
	integ       *Integration
	accountRepo repository.SocialAccountRepository
	settings    SettingsStore
}

// NewPublisher wires the adapter. accountRepo is queried at most once
// per /api/platforms request; settings is the (cached) store the
// bootstrap and worker also share.
func NewPublisher(integ *Integration, accountRepo repository.SocialAccountRepository, settings SettingsStore) *Publisher {
	return &Publisher{integ: integ, accountRepo: accountRepo, settings: settings}
}

func (p *Publisher) ID() string   { return PublisherID }
func (p *Publisher) Name() string { return "Zernio" }

// State maps the integration's controller state onto the public
// "disabled / degraded / ok" vocabulary the Publisher contract uses.
// A nil or unconfigured integration always reports "disabled".
func (p *Publisher) State() string {
	if !p.integ.Enabled() {
		return string(StateDisabled)
	}
	return string(p.integ.State())
}

// PlatformViews returns a view per Phase 1 allowlist platform with
// the curated post-type subset and the locally-mirrored accounts
// connected for that platform. social_accounts.platform holds the
// Zernio platform identifier (whatever Zernio's API returned), so
// the join uses ZernioID, not OgenID.
//
// One ListActive query per call regardless of platform count; the
// in-memory bucket-by-platform step is O(accounts).
func (p *Publisher) PlatformViews(ctx context.Context) ([]publishers.PlatformView, error) {
	profileID, _, err := p.settings.Get(ctx, SettingProfileID)
	if err != nil {
		return nil, err
	}

	byPlatform := map[string][]publishers.Account{}
	if profileID != "" {
		rows, err := p.accountRepo.ListActive(ctx, profileID)
		if err != nil {
			return nil, err
		}
		for _, r := range rows {
			byPlatform[r.Platform] = append(byPlatform[r.Platform], publishers.Account{
				ID:             r.ID,
				Username:       r.Username,
				DisplayName:    r.DisplayName,
				AvatarURL:      r.AvatarURL,
				IsActive:       r.IsActive,
				ConnectedAt:    r.ConnectedAt,
				TokenExpiresAt: r.TokenExpiresAt,
			})
		}
	}

	allowlist := SupportedPlatforms()
	out := make([]publishers.PlatformView, 0, len(allowlist))
	for _, sp := range allowlist {
		accounts := byPlatform[sp.ZernioID]
		if accounts == nil {
			accounts = []publishers.Account{}
		}
		out = append(out, publishers.PlatformView{
			OgenPlatformID:      sp.OgenID,
			PlatformName:        sp.Label, // canonical Ogen platform name; matches platforms.name
			PublisherPlatformID: sp.ZernioID,
			SupportedPostTypes:  append([]string(nil), sp.SupportedPostTypes...),
			Accounts:            accounts,
		})
	}
	return out, nil
}

// Compile-time check that *Publisher satisfies the interface.
var _ publishers.Publisher = (*Publisher)(nil)
