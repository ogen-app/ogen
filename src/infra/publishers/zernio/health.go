package zernio

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"
)

// AccountHealth is one entry of Zernio's bulk account-health report
// (GET /v1/accounts/health), the forward-looking connection signal CON-219 acts
// on. Zernio derives Status/TokenValid/NeedsReconnect from the live OAuth token
// and granted scopes; TokenExpiresAt is when the token lapses (absent — nil —
// for platforms Zernio can't date, e.g. auto-refreshing ones or an
// already-broken link). Only the fields Ogen consumes are decoded; the rest of
// the payload (permissions, canFetchAnalytics, …) is ignored.
type AccountHealth struct {
	AccountID      string     `json:"accountId"`
	ProfileID      string     `json:"profileId"`
	Platform       string     `json:"platform"`
	Username       string     `json:"username"`
	DisplayName    string     `json:"displayName"`
	Status         string     `json:"status"` // healthy | warning | error
	TokenValid     bool       `json:"tokenValid"`
	TokenExpiresAt *time.Time `json:"tokenExpiresAt"`
	NeedsReconnect bool       `json:"needsReconnect"`
	CanPost        bool       `json:"canPost"`
	Issues         []string   `json:"issues"`
}

// accountHealthEnvelope mirrors Zernio's `{"summary": {...}, "accounts": [...]}`
// bulk-health shape. Only accounts is decoded; the summary counters are a
// convenience Ogen recomputes locally.
type accountHealthEnvelope struct {
	Accounts []AccountHealth `json:"accounts"`
}

// GetAccountsHealth returns the health of every account attached to profileID.
// The `profileId` filter is applied server-side; as with
// ListAccounts we additionally filter client-side on the echoed profileId so a
// future multi-profile Ogen instance isn't surprised by a wider remote view.
// Non-2xx responses surface as *APIError (an add-on 403 carries RequiresAddon).
func (c *Client) GetAccountsHealth(ctx context.Context, profileID string) ([]AccountHealth, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	q := url.Values{}
	if profileID != "" {
		q.Set("profileId", profileID)
	}
	var env accountHealthEnvelope
	if err := c.do(ctx, http.MethodGet, "/accounts/health", q, nil, &env); err != nil {
		return nil, err
	}
	out := make([]AccountHealth, 0, len(env.Accounts))
	for _, a := range env.Accounts {
		if profileID != "" && a.ProfileID != "" && a.ProfileID != profileID {
			continue
		}
		out = append(out, a)
	}
	return out, nil
}
