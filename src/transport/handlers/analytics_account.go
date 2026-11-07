package handlers

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/analytics"
)

// accountLabel is the owning account's display block, shared by the
// performers board and the per-post header.
type accountLabel = analytics.AccountLabel

// accountRef pulls the owning account's id/username from the current row's
// per-platform breakdown, preferring the entry matching the row's platform.
func accountRef(r *models.PostAnalytics) (id, username string) { return analytics.AccountRef(r) }

// loadAccounts fetches the social_accounts rows for ids, keyed by id; best
// effort, so labels degrade to usernames.
func (h *AnalyticsHandler) loadAccounts(ctx context.Context, ids []string) map[string]models.SocialAccount {
	return h.Service().LoadAccounts(ctx, ids)
}

// labelAccount composes the display block for one account.
func labelAccount(id, username string, known map[string]models.SocialAccount) accountLabel {
	return analytics.LabelAccount(id, username, known)
}
