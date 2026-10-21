package queues_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/jobs/queues"
)

type fakeBillingLive struct{ live bool }

func (f fakeBillingLive) HasLiveSubscription(context.Context, string) (bool, error) {
	return f.live, nil
}

// fakeLocalAccounts serves the two account-row calls the unpaid teardown
// makes; failDeletes fails that many SoftDelete calls first.
type fakeLocalAccounts struct {
	repository.SocialAccountRepository
	mu          sync.Mutex
	active      []string
	failDeletes int
}

func (f *fakeLocalAccounts) ListActive(context.Context, string) ([]models.SocialAccount, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]models.SocialAccount, 0, len(f.active))
	for _, id := range f.active {
		out = append(out, models.SocialAccount{ID: id})
	}
	return out, nil
}

func (f *fakeLocalAccounts) SoftDelete(_ context.Context, id string, _ time.Time) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failDeletes > 0 {
		f.failDeletes--
		return false, errors.New("db unavailable")
	}
	for i, a := range f.active {
		if a == id {
			f.active = append(f.active[:i], f.active[i+1:]...)
			return true, nil
		}
	}
	return false, nil
}

func unpaidJob(tenantID string) *river.Job[queues.TeardownZernioProfileTask] {
	return &river.Job[queues.TeardownZernioProfileTask]{Args: queues.TeardownZernioProfileTask{TenantID: tenantID, Reason: queues.TeardownReasonTrialUnpaid}}
}

func TestUnpaidTrialTeardownKeepsProfileUntilLocalRowsAreGone(t *testing.T) {
	stub := &teardownStub{accounts: []string{"acc-1"}}
	srv := stub.server()
	defer srv.Close()

	store := newFakeSettings()
	seedTeardownProfile(store, "acme", "prof-acme")
	local := &fakeLocalAccounts{active: []string{"acc-1", "acc-2"}, failDeletes: 1}
	p := newTeardownProcessor(srv.URL, store, fakeFence{status: models.TenantStatusActive})
	p.Billing, p.Accounts = fakeBillingLive{}, local

	// The first attempt fails on the local rows: the profile id must survive so
	// the retry finds them again.
	if err := p.Work(t.Context(), unpaidJob("acme")); err == nil {
		t.Fatal("first attempt should fail on the local soft-delete")
	}
	if _, ok, _ := store.Get(tctx("acme"), zernio.SettingProfileID); !ok {
		t.Fatal("profile id cleared before the local rows were disconnected")
	}
	if err := p.Work(t.Context(), unpaidJob("acme")); err != nil {
		t.Fatalf("retry: %v", err)
	}
	if len(local.active) != 0 {
		t.Fatalf("local rows left active: %v", local.active)
	}
	if _, ok, _ := store.Get(tctx("acme"), zernio.SettingProfileID); ok {
		t.Fatal("profile id kept after a complete teardown")
	}
	if got := stub.profileDeletes(); len(got) == 0 || got[0] != "prof-acme" {
		t.Fatalf("remote profile not deleted: %v", got)
	}
}

func TestUnpaidTrialTeardownSkipsSubscribedOrInactiveTenant(t *testing.T) {
	for name, tc := range map[string]struct {
		status string
		live   bool
	}{
		"subscribed since": {models.TenantStatusActive, true},
		"deleted tenant":   {models.TenantStatusDeleted, false},
	} {
		stub := &teardownStub{accounts: []string{"acc-1"}}
		srv := stub.server()
		store := newFakeSettings()
		seedTeardownProfile(store, "acme", "prof-acme")
		p := newTeardownProcessor(srv.URL, store, fakeFence{status: tc.status})
		p.Billing, p.Accounts = fakeBillingLive{live: tc.live}, &fakeLocalAccounts{active: []string{"acc-1"}}
		if err := p.Work(t.Context(), unpaidJob("acme")); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := stub.profileDeletes(); len(got) != 0 {
			t.Fatalf("%s: profile deleted: %v", name, got)
		}
		srv.Close()
	}
}
