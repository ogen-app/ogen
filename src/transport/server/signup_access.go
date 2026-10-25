package server

import (
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/usecase/signupaccess"
)

// NewSignupAccessService builds the signup-access usecase: the signup mode,
// tester invites and the workspace cap. The HTTP app and the internal gRPC
// server each build one over the same database; a mode change made through one
// reaches the other within the settings cache TTL. emails may be nil (invites
// are stored, no mail is queued).
func NewSignupAccessService(db *bun.DB, cfg *config.Config, emails signupaccess.EmailEnqueuer) *signupaccess.Service {
	return signupaccess.New(signupaccess.Deps{
		DB:           db,
		Invites:      repository.NewTenantInviteRepository(db),
		Settings:     repository.NewSignupSettingsRepository(db),
		Accounts:     repository.NewAccountRepository(db),
		Tiers:        repository.NewTenantTierRepository(db),
		TierVersions: repository.NewTenantTierVersionRepository(db),
		Groups:       repository.NewTenantGroupRepository(db),
		Emails:       emails,
		AppBaseURL:   cfg.AppBaseURL,
	})
}
