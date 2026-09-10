package envelope

import (
	"bytes"
	"crypto/rand"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustKEK(t *testing.T) []byte {
	t.Helper()
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return k
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	cipher, err := NewCipher(mustKEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	plaintexts := [][]byte{
		[]byte("sk-ant-api03-abc"),
		[]byte(""),
		bytes.Repeat([]byte("A"), 4096),
		[]byte("\x00\x01\x02\xff"),
	}
	for _, pt := range plaintexts {
		rec, err := cipher.Encrypt(pt)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if rec.Algorithm != AlgorithmAES256GCM {
			t.Errorf("algorithm = %q, want %q", rec.Algorithm, AlgorithmAES256GCM)
		}
		if rec.KEKVersion != CurrentKEKVersion {
			t.Errorf("kek_version = %d, want %d", rec.KEKVersion, CurrentKEKVersion)
		}
		got, err := cipher.Decrypt(rec)
		if err != nil {
			t.Fatalf("Decrypt: %v", err)
		}
		if !bytes.Equal(got, pt) {
			t.Errorf("round-trip mismatch: got %q want %q", got, pt)
		}
	}
}

func TestEncryptUsesFreshNonces(t *testing.T) {
	cipher, err := NewCipher(mustKEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	pt := []byte("repeat me")
	const N = 16
	seen := make(map[string]struct{}, N)
	dekSeen := make(map[string]struct{}, N)
	for i := range N {
		rec, err := cipher.Encrypt(pt)
		if err != nil {
			t.Fatalf("Encrypt: %v", err)
		}
		if _, dup := seen[string(rec.Nonce)]; dup {
			t.Fatalf("duplicate ciphertext nonce after %d encrypts", i+1)
		}
		seen[string(rec.Nonce)] = struct{}{}
		if _, dup := dekSeen[string(rec.DEKNonce)]; dup {
			t.Fatalf("duplicate DEK nonce after %d encrypts", i+1)
		}
		dekSeen[string(rec.DEKNonce)] = struct{}{}
	}
}

func TestDecryptRejectsTamperedCiphertext(t *testing.T) {
	cipher, err := NewCipher(mustKEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	rec, err := cipher.Encrypt([]byte("the secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	rec.Ciphertext[0] ^= 0x01
	_, err = cipher.Decrypt(rec)
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}

func TestDecryptRejectsTamperedWrappedDEK(t *testing.T) {
	cipher, err := NewCipher(mustKEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	rec, err := cipher.Encrypt([]byte("the secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	rec.WrappedDEK[0] ^= 0x01
	_, err = cipher.Decrypt(rec)
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}

func TestDecryptRejectsWrongKEK(t *testing.T) {
	c1, _ := NewCipher(mustKEK(t))
	c2, _ := NewCipher(mustKEK(t))
	rec, err := c1.Encrypt([]byte("the secret"))
	if err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	_, err = c2.Decrypt(rec)
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("err = %v, want ErrAuthFailed", err)
	}
}

func TestDecryptRejectsMalformed(t *testing.T) {
	cipher, err := NewCipher(mustKEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	cases := []Record{
		{Ciphertext: []byte("x"), Nonce: make([]byte, 11), WrappedDEK: []byte("y"), DEKNonce: make([]byte, gcmNonceSize)},
		{Ciphertext: []byte("x"), Nonce: make([]byte, gcmNonceSize), WrappedDEK: []byte("y"), DEKNonce: make([]byte, 8)},
		{Ciphertext: nil, Nonce: make([]byte, gcmNonceSize), WrappedDEK: []byte("y"), DEKNonce: make([]byte, gcmNonceSize)},
		{Ciphertext: []byte("x"), Nonce: make([]byte, gcmNonceSize), WrappedDEK: nil, DEKNonce: make([]byte, gcmNonceSize)},
	}
	for i, rec := range cases {
		if _, err := cipher.Decrypt(rec); !errors.Is(err, ErrMalformed) {
			t.Errorf("case %d: err = %v, want ErrMalformed", i, err)
		}
	}
}

func TestDecryptRejectsUnknownAlgorithm(t *testing.T) {
	cipher, err := NewCipher(mustKEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	rec, _ := cipher.Encrypt([]byte("x"))
	rec.Algorithm = "AES-512-MAGIC"
	if _, err := cipher.Decrypt(rec); !errors.Is(err, ErrAlgorithmUnsupported) {
		t.Errorf("err = %v, want ErrAlgorithmUnsupported", err)
	}
}

func TestNewCipherRejectsBadKey(t *testing.T) {
	if _, err := NewCipher(nil); err == nil {
		t.Error("NewCipher(nil) returned no error")
	}
	if _, err := NewCipher(make([]byte, 16)); err == nil {
		t.Error("NewCipher(16 bytes) returned no error")
	}
}

func TestLoadOrCreateKEK_GeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "subdir", "kek.v1")

	k1, src1, err := LoadOrCreateKEK(path)
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if src1 != KEKSourceGenerated {
		t.Errorf("first source = %q, want %q", src1, KEKSourceGenerated)
	}
	if len(k1) != KeySize {
		t.Fatalf("first key length = %d, want %d", len(k1), KeySize)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("perm = %v, want 0600", info.Mode().Perm())
	}

	k2, src2, err := LoadOrCreateKEK(path)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if src2 != KEKSourceLoaded {
		t.Errorf("second source = %q, want %q", src2, KEKSourceLoaded)
	}
	if !bytes.Equal(k1, k2) {
		t.Error("loaded KEK differs from generated")
	}
}

func TestLoadOrCreateKEK_RejectsWrongLength(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kek.v1")
	if err := os.WriteFile(path, []byte("too short"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if _, _, err := LoadOrCreateKEK(path); err == nil || !strings.Contains(err.Error(), "expected") {
		t.Errorf("err = %v, want length error", err)
	}
}

func TestLoadOrCreateKEK_RejectsEmptyPath(t *testing.T) {
	if _, _, err := LoadOrCreateKEK(""); err == nil {
		t.Error("expected error on empty path")
	}
}
