package queues

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email/templates"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

//go:fix inline

func TestClassifyHealth(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	const lead = 7

	cases := []struct {
		name string
		h    zernio.AccountHealth
		want string
	}{
		{"healthy far expiry", zernio.AccountHealth{Status: "healthy", TokenValid: true, TokenExpiresAt: new(now.AddDate(0, 0, 30))}, ""},
		{"healthy no expiry", zernio.AccountHealth{Status: "healthy", TokenValid: true}, ""},
		// Auto-refreshed short-lived access token: always near, never a signal.
		{"healthy token expiring in an hour", zernio.AccountHealth{Status: "healthy", TokenValid: true, TokenExpiresAt: new(now.Add(time.Hour))}, ""},
		{"healthy within lead window", zernio.AccountHealth{Status: "healthy", TokenExpiresAt: new(now.AddDate(0, 0, 3))}, ""},
		{"healthy token just lapsed", zernio.AccountHealth{Status: "healthy", TokenValid: true, TokenExpiresAt: new(now.Add(-time.Minute))}, ""},
		{"warning status", zernio.AccountHealth{Status: "warning", TokenExpiresAt: new(now.AddDate(0, 0, 30))}, templates.StageExpiringSoon},
		{"warning with lapsed token", zernio.AccountHealth{Status: "warning", TokenExpiresAt: new(now.AddDate(0, 0, -1))}, templates.StageExpiringSoon},
		{"error status", zernio.AccountHealth{Status: "error"}, templates.StageActionRequired},
		{"needs reconnect", zernio.AccountHealth{Status: "healthy", NeedsReconnect: true}, templates.StageActionRequired},
		{"needs reconnect trumps warning", zernio.AccountHealth{Status: "warning", NeedsReconnect: true}, templates.StageActionRequired},
		// No status: the token date is all there is to go on.
		{"no status far expiry", zernio.AccountHealth{TokenExpiresAt: new(now.AddDate(0, 0, 30))}, ""},
		{"no status within lead window", zernio.AccountHealth{TokenExpiresAt: new(now.AddDate(0, 0, 3))}, templates.StageExpiringSoon},
		{"no status exactly at lead edge", zernio.AccountHealth{TokenExpiresAt: new(now.AddDate(0, 0, 7))}, templates.StageExpiringSoon},
		{"no status already expired", zernio.AccountHealth{TokenExpiresAt: new(now.AddDate(0, 0, -1))}, templates.StageActionRequired},
		{"no status no expiry", zernio.AccountHealth{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classifyHealth(tc.h, now, lead); got != tc.want {
				t.Errorf("classifyHealth = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestEpisodeStart(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	prev := now.Add(-48 * time.Hour)

	if got := episodeStart(nil, templates.StageExpiringSoon, now); got == nil || !got.Equal(now) {
		t.Errorf("new episode should start now, got %v", got)
	}
	if got := episodeStart(&prev, templates.StageActionRequired, now); got == nil || !got.Equal(prev) {
		t.Errorf("ongoing episode should keep its start, got %v", got)
	}
	if got := episodeStart(&prev, "", now); got != nil {
		t.Errorf("healthy should close the episode, got %v", got)
	}
}

func TestExpiryIdempotencyKey(t *testing.T) {
	since := time.Date(2026, 9, 15, 8, 30, 0, 0, time.UTC)
	// Same account+stage+episode+owner ⇒ stable key (notify-once across sweeps).
	a := expiryIdempotencyKey("acc1", templates.StageExpiringSoon, since, "owner1")
	if b := expiryIdempotencyKey("acc1", templates.StageExpiringSoon, since, "owner1"); a != b {
		t.Fatalf("key not stable: %q vs %q", a, b)
	}
	if want := "conn_expiring:acc1:expiring_soon:2026-09-15T08:30:00Z:owner1"; a != want {
		t.Errorf("key = %q, want %q", a, want)
	}
	// A new episode ⇒ a fresh key, so a later degradation re-notifies.
	if c := expiryIdempotencyKey("acc1", templates.StageExpiringSoon, since.AddDate(0, 0, 60), "owner1"); c == a {
		t.Errorf("new episode should change the key, both = %q", a)
	}
	// Escalating within an episode ⇒ its own key.
	if d := expiryIdempotencyKey("acc1", templates.StageActionRequired, since, "owner1"); d == a {
		t.Errorf("different stage should change the key")
	}
	// A different owner ⇒ its own key.
	if e := expiryIdempotencyKey("acc1", templates.StageExpiringSoon, since, "owner2"); e == a {
		t.Errorf("different owner should change the key")
	}
}

// healthAccountRepo is an in-memory account mirror that applies UpdateHealth,
// so consecutive sweeps see the episode start the previous one stored.
type healthAccountRepo struct {
	repository.SocialAccountRepository
	tenantID, profileID string
	accounts            map[string]*models.SocialAccount
}

func (r *healthAccountRepo) ListActiveTenantProfiles(context.Context) ([]repository.TenantProfile, error) {
	return []repository.TenantProfile{{TenantID: r.tenantID, ProfileID: r.profileID}}, nil
}

func (r *healthAccountRepo) ListActive(context.Context, string) ([]models.SocialAccount, error) {
	out := make([]models.SocialAccount, 0, len(r.accounts))
	for _, a := range r.accounts {
		out = append(out, *a)
	}
	return out, nil
}

func (r *healthAccountRepo) UpdateHealth(_ context.Context, id string, h repository.SocialAccountHealth) error {
	a := r.accounts[id]
	a.TokenExpiresAt = h.TokenExpiresAt
	a.HealthStatus = h.HealthStatus
	a.UnhealthySince = h.UnhealthySince
	return nil
}

// ownerRepo returns a single owner for any tenant.
type ownerRepo struct {
	repository.UserRepository
	owner models.User
}

func (o ownerRepo) ListOwnersByTenant(context.Context, string) ([]models.User, error) {
	return []models.User{o.owner}, nil
}

// seenKeyLog reports every idempotency key as already sent and records the
// lookups, so a sweep exercises the dedupe without needing a River client.
type seenKeyLog struct {
	repository.EmailLogRepository
	keys []string
}

func (l *seenKeyLog) ExistsByIdempotencyKey(_ context.Context, key string) (bool, error) {
	l.keys = append(l.keys, key)
	return true, nil
}

func TestSweepDedupesAcrossMovingTokenExpiry(t *testing.T) {
	var health []zernio.AccountHealth
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": health})
	}))
	defer srv.Close()

	accounts := &healthAccountRepo{
		tenantID:  "t1",
		profileID: "p1",
		accounts:  map[string]*models.SocialAccount{"acc1": {ID: "acc1", Platform: "youtube"}},
	}
	logs := &seenKeyLog{}
	p := &DetectExpiringConnectionsProcessor{
		Zernio: ZernioDeps{
			SocialAccountRepo: accounts,
			Client:            zernio.NewClient(zernio.StaticKey("k"), srv.URL, zernio.ClientOpts{Timeout: 5 * time.Second}),
		},
		Users:     ownerRepo{owner: models.User{ID: "owner1"}},
		EmailLogs: logs,
	}
	ctx := tenantctx.WithSystem(context.Background())
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	sweep := func(status string, at time.Time) {
		t.Helper()
		health = []zernio.AccountHealth{{AccountID: "acc1", ProfileID: "p1", Platform: "youtube", Status: status, TokenValid: true, TokenExpiresAt: new(at.Add(30 * time.Hour))}}
		if _, err := p.sweep(ctx, at); err != nil {
			t.Fatalf("sweep: %v", err)
		}
	}

	// A healthy account whose refreshed token is always ~a day away never notifies.
	for i := range 4 {
		sweep("healthy", now.Add(time.Duration(i)*12*time.Hour))
	}
	if len(logs.keys) != 0 {
		t.Fatalf("healthy account looked up keys %v", logs.keys)
	}
	if accounts.accounts["acc1"].UnhealthySince != nil {
		t.Fatalf("healthy account has an open episode")
	}

	// A warning episode keeps one key while the reported expiry moves daily.
	start := now.AddDate(0, 0, 3)
	for i := range 4 {
		sweep("warning", start.Add(time.Duration(i)*24*time.Hour))
	}
	if len(logs.keys) != 4 {
		t.Fatalf("lookups = %d, want 4", len(logs.keys))
	}
	for _, k := range logs.keys[1:] {
		if k != logs.keys[0] {
			t.Fatalf("key changed within one episode: %q vs %q", logs.keys[0], k)
		}
	}
	if got := accounts.accounts["acc1"].UnhealthySince; got == nil || !got.Equal(start) {
		t.Fatalf("episode start = %v, want %v", got, start)
	}

	// Recovery closes the episode; a later degradation gets a fresh key.
	sweep("healthy", start.AddDate(0, 0, 5))
	sweep("warning", start.AddDate(0, 0, 6))
	if last := logs.keys[len(logs.keys)-1]; last == logs.keys[0] {
		t.Fatalf("new episode reused key %q", last)
	}
}

func TestPlatformLabel(t *testing.T) {
	cases := map[string]string{
		"linkedin": "LinkedIn",
		"twitter":  "X (Twitter)",
		"youtube":  "YouTube",
		"mastodon": "Mastodon", // unknown slug ⇒ capitalized
		"":         "social",
	}
	for slug, want := range cases {
		if got := platformLabel(slug); got != want {
			t.Errorf("platformLabel(%q) = %q, want %q", slug, got, want)
		}
	}
}

func TestAccountLabel(t *testing.T) {
	// Prefers the live health payload, falls back to the local mirror, then a default.
	if got := accountLabel(zernio.AccountHealth{DisplayName: "Acme Corp", Username: "acme"}, models.SocialAccount{}); got != "Acme Corp" {
		t.Errorf("display name preferred: got %q", got)
	}
	if got := accountLabel(zernio.AccountHealth{Username: "acme"}, models.SocialAccount{DisplayName: "ignored"}); got != "acme" {
		t.Errorf("username fallback: got %q", got)
	}
	if got := accountLabel(zernio.AccountHealth{}, models.SocialAccount{Username: "local-handle"}); got != "local-handle" {
		t.Errorf("local mirror fallback: got %q", got)
	}
	if got := accountLabel(zernio.AccountHealth{}, models.SocialAccount{}); got != "your account" {
		t.Errorf("default fallback: got %q", got)
	}
}

func TestHumanizeUntil(t *testing.T) {
	now := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		in   time.Time
		want string
	}{
		{now.AddDate(0, 0, -1), "today"},
		{now.Add(2 * time.Hour), "in less than a day"},
		{now.Add(25 * time.Hour), "in 1 day"},
		{now.AddDate(0, 0, 6), "in 6 days"},
	}
	for _, tc := range cases {
		if got := humanizeUntil(tc.in, now); got != tc.want {
			t.Errorf("humanizeUntil(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
