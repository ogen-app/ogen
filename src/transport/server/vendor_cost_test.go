package server

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/infra/anthropicadmin"
	"github.com/ogen-app/ogen/src/infra/secrets"
)

// adminKeyStore holds one secret; every other name reads as unset.
type adminKeyStore struct {
	secrets.Store
	name, value string
}

func (s adminKeyStore) Get(_ context.Context, name string) (string, error) {
	if name != s.name {
		return "", secrets.ErrNotFound
	}
	return s.value, nil
}

func TestNewAnthropicCostsReadsAdminKey(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-api-key")
		_, _ = w.Write([]byte(`{"data":[],"has_more":false}`))
	}))
	t.Cleanup(srv.Close)
	day := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

	configured := newAnthropicCosts(adminKeyStore{name: secrets.NameAnthropicAdminAPIKey, value: "sk-ant-admin01-x"}, srv.URL)
	if _, err := configured.DailyCosts(t.Context(), day, day); err != nil {
		t.Fatal(err)
	}
	if gotKey != "sk-ant-admin01-x" {
		t.Fatalf("x-api-key = %q, want the anthropic_admin_api_key value", gotKey)
	}

	// The flows' API key is not the Admin key.
	unconfigured := newAnthropicCosts(adminKeyStore{name: secrets.NameAnthropicAPIKey, value: "sk-ant-api03-x"}, srv.URL)
	if _, err := unconfigured.DailyCosts(t.Context(), day, day); !errors.Is(err, anthropicadmin.ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}
