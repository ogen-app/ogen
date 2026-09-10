package schedule

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// fakeAccountRepo is a tiny in-memory repository.SocialAccountRepository for
// exercising the CON-150 write-time gate without a database.
type fakeAccountRepo struct {
	byProfile map[string][]models.SocialAccount
}

func (r *fakeAccountRepo) ListAll(context.Context, string) ([]models.SocialAccount, error) {
	return nil, nil
}
func (r *fakeAccountRepo) ListActive(_ context.Context, profileID string) ([]models.SocialAccount, error) {
	return r.byProfile[profileID], nil
}
func (r *fakeAccountRepo) ListActiveByPlatform(_ context.Context, profileID, platform string) ([]models.SocialAccount, error) {
	var out []models.SocialAccount
	for _, a := range r.byProfile[profileID] {
		if a.Platform == platform && a.DeletedAt == nil {
			out = append(out, a)
		}
	}
	return out, nil
}
func (r *fakeAccountRepo) GetActive(_ context.Context, profileID, id string) (*models.SocialAccount, error) {
	for i := range r.byProfile[profileID] {
		if a := r.byProfile[profileID][i]; a.ID == id && a.DeletedAt == nil {
			return &a, nil
		}
	}
	return nil, sql.ErrNoRows
}
func (r *fakeAccountRepo) ApplyPlan(context.Context, []models.SocialAccount, []string, time.Time) error {
	return nil
}
func (r *fakeAccountRepo) SoftDelete(context.Context, string, time.Time) (bool, error) {
	return false, nil
}
func (r *fakeAccountRepo) UpdateHealth(context.Context, string, repository.SocialAccountHealth) error {
	return nil
}
func (r *fakeAccountRepo) ListActiveTenantProfiles(context.Context) ([]repository.TenantProfile, error) {
	return nil, nil
}

func gateService(accounts map[string][]models.SocialAccount) *Service {
	s := &Service{}
	s.SetAccountGate(&fakeAccountRepo{byProfile: accounts}, func(context.Context) (string, error) { return "p_test", nil })
	return s
}

func reasonOf(t *testing.T, err error) string {
	t.Helper()
	ae, ok := errors.AsType[*AccountSelectionError](err)
	if !ok {
		t.Fatalf("want *AccountSelectionError, got %v", err)
	}
	return ae.Reason
}

func TestCheckAccountSelection(t *testing.T) {
	ctx := context.Background()
	two := map[string][]models.SocialAccount{"p_test": {
		{ID: "acc-1", Platform: "linkedin", Username: "acme-corp", DisplayName: "Acme Corp"},
		{ID: "acc-2", Platform: "linkedin", Username: "acme-labs", DisplayName: "Acme Labs"},
	}}

	t.Run("ambiguous with no choice returns candidates", func(t *testing.T) {
		err := gateService(two).checkAccountSelection(ctx, &models.Post{}, "linkedin")
		if reasonOf(t, err) != "account_selection_required" {
			t.Fatalf("reason: %v", err)
		}
		ae, _ := errors.AsType[*AccountSelectionError](err)
		if len(ae.Candidates) != 2 {
			t.Fatalf("candidates: got %d want 2", len(ae.Candidates))
		}
		if ae.Candidates[0].ID != "acc-1" || ae.Candidates[0].Username != "acme-corp" {
			t.Errorf("candidate[0]: %+v", ae.Candidates[0])
		}
	})

	t.Run("explicit valid choice passes", func(t *testing.T) {
		if err := gateService(two).checkAccountSelection(ctx, &models.Post{SocialAccountID: "acc-2"}, "linkedin"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})

	t.Run("unknown choice is unavailable", func(t *testing.T) {
		err := gateService(two).checkAccountSelection(ctx, &models.Post{SocialAccountID: "ghost"}, "linkedin")
		if reasonOf(t, err) != "account_unavailable" {
			t.Fatalf("reason: %v", err)
		}
	})

	t.Run("wrong-platform choice mismatches", func(t *testing.T) {
		accts := map[string][]models.SocialAccount{"p_test": {{ID: "tw-1", Platform: "twitter"}}}
		err := gateService(accts).checkAccountSelection(ctx, &models.Post{SocialAccountID: "tw-1"}, "linkedin")
		if reasonOf(t, err) != "account_platform_mismatch" {
			t.Fatalf("reason: %v", err)
		}
	})

	t.Run("single account with no choice passes (worker auto-selects)", func(t *testing.T) {
		accts := map[string][]models.SocialAccount{"p_test": {{ID: "acc-1", Platform: "linkedin"}}}
		if err := gateService(accts).checkAccountSelection(ctx, &models.Post{}, "linkedin"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})

	t.Run("zero accounts with no choice passes (worker terminals)", func(t *testing.T) {
		if err := gateService(nil).checkAccountSelection(ctx, &models.Post{}, "linkedin"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})

	t.Run("unwired gate is a no-op", func(t *testing.T) {
		if err := (&Service{}).checkAccountSelection(ctx, &models.Post{}, "linkedin"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})

	t.Run("empty profile is a no-op", func(t *testing.T) {
		s := &Service{}
		s.SetAccountGate(&fakeAccountRepo{byProfile: two}, func(context.Context) (string, error) { return "", nil })
		if err := s.checkAccountSelection(ctx, &models.Post{}, "linkedin"); err != nil {
			t.Fatalf("want nil, got %v", err)
		}
	})
}
