package secrets

import (
	"crypto/rand"
	"errors"
	"strings"
	"testing"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/infra/crypto/envelope"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/pgtest"
)

// mustOpenDB returns a fresh, isolated, fully-migrated Postgres DB (the
// `secret` table is part of the baseline schema). Each call gets its own
// database so tests cannot see each other's rows.
func mustOpenDB(t *testing.T) *bun.DB {
	t.Helper()
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func mustCipher(t *testing.T) *envelope.Cipher {
	t.Helper()
	kek := make([]byte, envelope.KeySize)
	if _, err := rand.Read(kek); err != nil {
		t.Fatalf("rand: %v", err)
	}
	c, err := envelope.NewCipher(kek)
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return c
}

func TestStoreSetGet(t *testing.T) {
	ctx := t.Context()
	store := NewStore(repository.NewSecretRepository(mustOpenDB(t)), mustCipher(t))

	meta, created, err := store.Set(ctx, NameAnthropicAPIKey, "sk-ant-api03-xyz")
	if err != nil {
		t.Fatalf("Set: %v", err)
	}
	if !created {
		t.Error("first Set: created should be true")
	}
	if meta.Name != NameAnthropicAPIKey || !meta.Decryptable {
		t.Errorf("metadata = %+v", meta)
	}

	got, err := store.Get(ctx, NameAnthropicAPIKey)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != "sk-ant-api03-xyz" {
		t.Errorf("Get = %q, want plaintext", got)
	}

	_, created2, err := store.Set(ctx, NameAnthropicAPIKey, "sk-ant-api03-renewed")
	if err != nil {
		t.Fatalf("renew Set: %v", err)
	}
	if created2 {
		t.Error("renew Set: created should be false")
	}
}

// TestAllowlist pins the closed set of accepted secret names. gemini_api_key
// joined it in CON-104; unlisted names must stay rejected. Pure (no DB) so it
// documents the contract independently of the store's storage path.
func TestAllowlist(t *testing.T) {
	for _, name := range []string{NameAnthropicAPIKey, NameZernioAPIKey, NameGeminiAPIKey} {
		if !IsAllowed(name) {
			t.Errorf("IsAllowed(%q) = false; want true", name)
		}
	}
	if IsAllowed("openai_api_key") {
		t.Error("IsAllowed(openai_api_key) = true; want false")
	}
}

func TestStoreRejectsUnknownName(t *testing.T) {
	ctx := t.Context()
	store := NewStore(repository.NewSecretRepository(mustOpenDB(t)), mustCipher(t))

	if _, _, err := store.Set(ctx, "openai_api_key", "x"); !errors.Is(err, ErrUnknownName) {
		t.Errorf("Set unknown: err = %v, want ErrUnknownName", err)
	}
	if _, err := store.Get(ctx, "openai_api_key"); !errors.Is(err, ErrUnknownName) {
		t.Errorf("Get unknown: err = %v, want ErrUnknownName", err)
	}
	if err := store.Delete(ctx, "openai_api_key"); !errors.Is(err, ErrUnknownName) {
		t.Errorf("Delete unknown: err = %v, want ErrUnknownName", err)
	}
}

func TestStoreNotFound(t *testing.T) {
	ctx := t.Context()
	store := NewStore(repository.NewSecretRepository(mustOpenDB(t)), mustCipher(t))

	if _, err := store.Get(ctx, NameAnthropicAPIKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get missing: err = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, NameAnthropicAPIKey); !errors.Is(err, ErrNotFound) {
		t.Errorf("Delete missing: err = %v, want ErrNotFound", err)
	}
}

func TestStoreValidationRejectsBadValues(t *testing.T) {
	ctx := t.Context()
	store := NewStore(repository.NewSecretRepository(mustOpenDB(t)), mustCipher(t))

	cases := []struct {
		label string
		val   string
	}{
		{"empty", ""},
		{"too long", strings.Repeat("a", MaxValueLen+1)},
		{"newline", "abc\ndef"},
		{"carriage return", "abc\rdef"},
		{"null byte", "abc\x00def"},
		{"DEL", "abc\x7fdef"},
	}
	for _, c := range cases {
		_, _, err := store.Set(ctx, NameAnthropicAPIKey, c.val)
		if !errors.Is(err, ErrInvalidValue) {
			t.Errorf("%s: err = %v, want ErrInvalidValue", c.label, err)
		}
		if err != nil && strings.Contains(err.Error(), c.val) && c.val != "" {
			t.Errorf("%s: error message echoed value: %s", c.label, err)
		}
	}
}

func TestStoreSubscribeFiresOnSetAndDelete(t *testing.T) {
	ctx := t.Context()
	store := NewStore(repository.NewSecretRepository(mustOpenDB(t)), mustCipher(t))

	calls := 0
	cancel := store.Subscribe(NameZernioAPIKey, func() { calls++ })

	if _, _, err := store.Set(ctx, NameZernioAPIKey, "key1"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if calls != 1 {
		t.Errorf("after first Set: calls = %d, want 1", calls)
	}

	if _, _, err := store.Set(ctx, NameZernioAPIKey, "key2"); err != nil {
		t.Fatalf("Set 2: %v", err)
	}
	if calls != 2 {
		t.Errorf("after second Set: calls = %d, want 2", calls)
	}

	if err := store.Delete(ctx, NameZernioAPIKey); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if calls != 3 {
		t.Errorf("after Delete: calls = %d, want 3", calls)
	}

	cancel()
	if _, _, err := store.Set(ctx, NameZernioAPIKey, "key3"); err != nil {
		t.Fatalf("Set 3: %v", err)
	}
	if calls != 3 {
		t.Errorf("after cancel: calls = %d, want still 3", calls)
	}

	// Subscriptions are name-scoped — Anthropic events shouldn't fire
	// the Zernio subscriber.
	otherCalls := 0
	store.Subscribe(NameAnthropicAPIKey, func() { otherCalls++ })
	if _, _, err := store.Set(ctx, NameZernioAPIKey, "key4"); err != nil {
		t.Fatalf("Set 4: %v", err)
	}
	if otherCalls != 0 {
		t.Errorf("Anthropic subscriber fired on Zernio event: otherCalls = %d", otherCalls)
	}
}

// Sentinel-based redaction test: submit a value that would be obvious
// in any leaked log/error, force a downstream failure path, and assert
// the sentinel never appears in the surfaced error message.
func TestStoreErrorsDoNotEchoPlaintext(t *testing.T) {
	const sentinel = "SENTINEL_LEAKED_VALUE_12345"

	// Length-violation path — the most likely place a naive
	// implementation would interpolate the value.
	ctx := t.Context()
	store := NewStore(repository.NewSecretRepository(mustOpenDB(t)), mustCipher(t))

	tooLong := sentinel + strings.Repeat("x", MaxValueLen)
	_, _, err := store.Set(ctx, NameAnthropicAPIKey, tooLong)
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("error message leaked plaintext: %s", err.Error())
	}

	// Control-char path — same property.
	_, _, err = store.Set(ctx, NameAnthropicAPIKey, sentinel+"\n")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Contains(err.Error(), sentinel) {
		t.Errorf("error message leaked plaintext: %s", err.Error())
	}
}

func TestStoreListReportsDecryptable(t *testing.T) {
	ctx := t.Context()
	repo := repository.NewSecretRepository(mustOpenDB(t))
	store := NewStore(repo, mustCipher(t))

	if _, _, err := store.Set(ctx, NameAnthropicAPIKey, "good"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	rows, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(rows) != 1 || !rows[0].Decryptable {
		t.Fatalf("List = %+v, want one decryptable row", rows)
	}

	// Corrupt the row → still listed, but decryptable=false.
	row, _ := repo.Get(ctx, NameAnthropicAPIKey)
	row.Ciphertext[0] ^= 0xff
	if err := repo.Upsert(ctx, row); err != nil {
		t.Fatalf("corrupt upsert: %v", err)
	}
	rows, _ = store.List(ctx)
	if len(rows) != 1 || rows[0].Decryptable {
		t.Errorf("List after corruption = %+v, want decryptable=false", rows)
	}
}
