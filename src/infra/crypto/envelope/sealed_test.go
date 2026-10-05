package envelope

import (
	"bytes"
	"errors"
	"testing"
)

func TestSealOpenRoundTrip(t *testing.T) {
	cipher, err := NewCipher(mustKEK(t))
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	sealed, err := cipher.Seal([]byte("ogp_secret"))
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if bytes.Contains(sealed, []byte("ogp_secret")) {
		t.Fatal("sealed blob leaks the plaintext")
	}
	got, err := cipher.Open(sealed)
	if err != nil || string(got) != "ogp_secret" {
		t.Fatalf("Open = %q, %v", got, err)
	}
	if _, err := cipher.Open([]byte("not json")); !errors.Is(err, ErrMalformed) {
		t.Fatalf("Open(garbage) err = %v, want ErrMalformed", err)
	}
}
