package secrets

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
)

// memSecrets is an in-memory SecretRepository that counts reads.
type memSecrets struct {
	rows  map[string]*models.Secret
	reads int
}

func (m *memSecrets) Get(_ context.Context, name string) (*models.Secret, error) {
	m.reads++
	row, ok := m.rows[name]
	if !ok {
		return nil, sql.ErrNoRows
	}
	cp := *row
	return &cp, nil
}

func (m *memSecrets) List(context.Context) ([]models.Secret, error) { return nil, nil }

func (m *memSecrets) Upsert(_ context.Context, s *models.Secret) error {
	cp := *s
	m.rows[s.Name] = &cp
	return nil
}

func (m *memSecrets) Delete(_ context.Context, name string) (bool, error) {
	_, ok := m.rows[name]
	delete(m.rows, name)
	return ok, nil
}

func TestStoreGetIsCached(t *testing.T) {
	ctx := t.Context()
	repo := &memSecrets{rows: map[string]*models.Secret{}}
	s := NewStore(repo, mustCipher(t)).(*store)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { return now }

	if _, err := s.Get(ctx, NameZernioAPIKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unset secret: err = %v, want ErrNotFound", err)
	}
	if _, err := s.Get(ctx, NameZernioAPIKey); !errors.Is(err, ErrNotFound) || repo.reads != 1 {
		t.Fatalf("cached not-found: err = %v, reads = %d, want ErrNotFound from one read", err, repo.reads)
	}

	if _, _, err := s.Set(ctx, NameZernioAPIKey, "key-1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	reads := repo.reads
	for range 3 {
		if v, err := s.Get(ctx, NameZernioAPIKey); err != nil || v != "key-1" {
			t.Fatalf("Get after Set = (%q, %v), want key-1", v, err)
		}
	}
	if repo.reads != reads+1 {
		t.Fatalf("reads after Set = %d, want one more than %d", repo.reads, reads)
	}

	// Another replica rotates the key: this one sees it once the TTL lapses.
	rotated := NewStore(repo, s.cipher)
	if _, _, err := rotated.Set(ctx, NameZernioAPIKey, "key-2"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if v, _ := s.Get(ctx, NameZernioAPIKey); v != "key-1" {
		t.Fatalf("within the TTL Get = %q, want the cached key-1", v)
	}
	now = now.Add(cacheTTL + time.Second)
	if v, _ := s.Get(ctx, NameZernioAPIKey); v != "key-2" {
		t.Fatalf("after the TTL Get = %q, want key-2", v)
	}

	if err := s.Delete(ctx, NameZernioAPIKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(ctx, NameZernioAPIKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("after Delete: err = %v, want ErrNotFound", err)
	}
}
