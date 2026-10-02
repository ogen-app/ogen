package handlers

import (
	"context"
	"log/slog"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// accountLabel is the owning account's display block, shared by the
// performers board and the per-post header.
type accountLabel struct {
	ID          string
	Username    string
	DisplayName string
	AvatarURL   string
}

// accountRef pulls the owning account's id/username from the current row's
// per-platform breakdown, preferring the entry matching the row's platform.
func accountRef(r *models.PostAnalytics) (id, username string) {
	for _, pa := range r.PlatformAnalytics {
		if strings.EqualFold(pa.Platform, r.Platform) {
			return pa.AccountID, pa.AccountUsername
		}
	}
	if len(r.PlatformAnalytics) > 0 {
		return r.PlatformAnalytics[0].AccountID, r.PlatformAnalytics[0].AccountUsername
	}
	return "", ""
}

// loadAccounts fetches the social_accounts rows for ids, keyed by id. It is
// best-effort: a nil repo or a failed read yields an empty map so the caller
// degrades to username-only labels instead of failing the response.
func (h *AnalyticsHandler) loadAccounts(ctx context.Context, ids []string) map[string]models.SocialAccount {
	out := map[string]models.SocialAccount{}
	if h.accounts == nil || len(ids) == 0 {
		return out
	}
	rows, err := h.accounts.ListByIDs(ctx, ids)
	if err != nil {
		slog.WarnContext(ctx, "analytics account labels unavailable; falling back to usernames",
			"error", err, "accounts", len(ids))
		return out
	}
	for _, a := range rows {
		out[a.ID] = a
	}
	return out
}

// labelAccount composes the display block for one account: display_name and
// avatar_url come from the social_accounts row when there is one, and
// display_name falls back to the username otherwise.
func labelAccount(id, username string, known map[string]models.SocialAccount) accountLabel {
	l := accountLabel{ID: id, Username: username}
	if a, ok := known[id]; ok && id != "" {
		if l.Username == "" {
			l.Username = a.Username
		}
		l.DisplayName = a.DisplayName
		l.AvatarURL = a.AvatarURL
	}
	if l.DisplayName == "" {
		l.DisplayName = l.Username
	}
	return l
}
