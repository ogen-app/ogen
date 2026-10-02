package handlers

import (
	"context"
	"errors"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// labelStubAccounts satisfies repository.SocialAccountRepository by embedding
// it (left nil) and overriding only ListByIDs — the single method the account
// labelling calls. Any other method panics on the nil embed.
type labelStubAccounts struct {
	repository.SocialAccountRepository
	rows  []models.SocialAccount
	err   error
	calls int
}

func (s *labelStubAccounts) ListByIDs(_ context.Context, ids []string) ([]models.SocialAccount, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	want := map[string]bool{}
	for _, id := range ids {
		want[id] = true
	}
	var out []models.SocialAccount
	for _, r := range s.rows {
		if want[r.ID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func TestAccountRef(t *testing.T) {
	t.Run("prefers the entry matching the row platform", func(t *testing.T) {
		r := &models.PostAnalytics{Platform: "LinkedIn", PlatformAnalytics: models.PlatformAnalyticsList{
			{Platform: "twitter", AccountID: "acc-x", AccountUsername: "x"},
			{Platform: "linkedin", AccountID: "acc-li", AccountUsername: "li"},
		}}
		if id, u := accountRef(r); id != "acc-li" || u != "li" {
			t.Fatalf("got %q/%q, want acc-li/li", id, u)
		}
	})
	t.Run("falls back to the first entry", func(t *testing.T) {
		r := &models.PostAnalytics{Platform: "LinkedIn", PlatformAnalytics: models.PlatformAnalyticsList{
			{Platform: "twitter", AccountID: "acc-x", AccountUsername: "x"},
		}}
		if id, u := accountRef(r); id != "acc-x" || u != "x" {
			t.Fatalf("got %q/%q, want acc-x/x", id, u)
		}
	})
	t.Run("empty breakdown", func(t *testing.T) {
		if id, u := accountRef(&models.PostAnalytics{}); id != "" || u != "" {
			t.Fatalf("got %q/%q, want empty", id, u)
		}
	})
}

func TestAccountLabels(t *testing.T) {
	ctx := context.Background()
	stub := &labelStubAccounts{rows: []models.SocialAccount{
		{ID: "acc-1", Username: "acme", DisplayName: "Acme Inc", AvatarURL: "https://cdn/acme.png"},
		{ID: "acc-2", Username: "beta", DisplayName: "", AvatarURL: "https://cdn/beta.png"},
	}}
	h := &AnalyticsHandler{accounts: stub}
	known := h.loadAccounts(ctx, []string{"acc-1", "acc-2", "acc-missing"})

	t.Run("known account carries display name and avatar", func(t *testing.T) {
		got := labelAccount("acc-1", "acme", known)
		want := accountLabel{ID: "acc-1", Username: "acme", DisplayName: "Acme Inc", AvatarURL: "https://cdn/acme.png"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("empty display name falls back to the username", func(t *testing.T) {
		got := labelAccount("acc-2", "beta", known)
		if got.DisplayName != "beta" || got.AvatarURL != "https://cdn/beta.png" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("missing username is taken from the account row", func(t *testing.T) {
		got := labelAccount("acc-1", "", known)
		if got.Username != "acme" || got.DisplayName != "Acme Inc" {
			t.Fatalf("got %+v", got)
		}
	})
	t.Run("missing row degrades to the username", func(t *testing.T) {
		got := labelAccount("acc-missing", "ghost", known)
		want := accountLabel{ID: "acc-missing", Username: "ghost", DisplayName: "ghost"}
		if got != want {
			t.Fatalf("got %+v, want %+v", got, want)
		}
	})
	t.Run("no id never matches", func(t *testing.T) {
		got := labelAccount("", "anon", map[string]models.SocialAccount{"": {DisplayName: "wrong"}})
		if got.DisplayName != "anon" {
			t.Fatalf("got %+v", got)
		}
	})
}

func TestLoadAccounts_BestEffort(t *testing.T) {
	ctx := context.Background()

	t.Run("repo error yields an empty map", func(t *testing.T) {
		h := &AnalyticsHandler{accounts: &labelStubAccounts{err: errors.New("db down")}}
		if got := h.loadAccounts(ctx, []string{"acc-1"}); len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
	t.Run("nil repo yields an empty map", func(t *testing.T) {
		h := &AnalyticsHandler{}
		if got := h.loadAccounts(ctx, []string{"acc-1"}); len(got) != 0 {
			t.Fatalf("got %v, want empty", got)
		}
	})
	t.Run("no ids skips the read", func(t *testing.T) {
		stub := &labelStubAccounts{}
		h := &AnalyticsHandler{accounts: stub}
		h.loadAccounts(ctx, nil)
		if stub.calls != 0 {
			t.Fatalf("ListByIDs called %d times, want 0", stub.calls)
		}
	})
}
