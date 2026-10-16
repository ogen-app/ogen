package queues_test

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
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
