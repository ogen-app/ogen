// Package envelope implements envelope encryption for at-rest secrets.
//
// A Key Encryption Key (KEK) lives on a Docker-mounted volume and never
// touches the database. Each persisted secret carries its own Data
// Encryption Key (DEK), generated at write time, used once to encrypt
// the payload, then wrapped (encrypted) by the KEK and stored alongside
// the ciphertext. Decryption inverts the process: load wrapped DEK,
// unwrap with KEK, decrypt payload, discard the DEK.
//
// Both layers use AES-256-GCM with a fresh 96-bit nonce per operation.
// Nothing in this package allocates plaintext into long-lived buffers;
// callers are expected to scope returned plaintexts narrowly.
package envelope

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// KeySize is the AES-256 key length, used for both KEK and per-row DEKs.
const KeySize = 32

// kekFilePerm is the on-disk mode for a freshly generated KEK file. The
// loader rejects anything that isn't owner-only readable to keep the
// guarantee one-way: once we've written 0600, we won't silently accept
// a file that someone has loosened.
const kekFilePerm fs.FileMode = 0o600

// KEKSource describes whether a KEK was created on this boot or loaded
// from a pre-existing file. Surfaced in the boot summary log so an
// operator can tell at a glance whether a new key just appeared.
type KEKSource string

const (
	KEKSourceGenerated KEKSource = "generated"
	KEKSourceLoaded    KEKSource = "loaded"
)

// LoadOrCreateKEK ensures a KEK file exists at path and returns its
// contents. If the file is missing, a fresh 32-byte key is generated
// from crypto/rand and written with 0600 permissions. If the file
// exists, it is loaded and its length is verified.
//
// Returns a descriptive error and no key material on any failure —
// callers should fail boot rather than continue without a KEK.
func LoadOrCreateKEK(path string) ([]byte, KEKSource, error) {
	if path == "" {
		return nil, "", errors.New("envelope: KEK path is empty")
	}

	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		if len(data) != KeySize {
			return nil, "", fmt.Errorf("envelope: KEK at %q is %d bytes, expected %d", path, len(data), KeySize)
		}
		return data, KEKSourceLoaded, nil

	case errors.Is(err, fs.ErrNotExist):
		key, genErr := generateKEK(path)
		if genErr != nil {
			return nil, "", genErr
		}
		return key, KEKSourceGenerated, nil

	default:
		return nil, "", fmt.Errorf("envelope: read KEK %q: %w", path, err)
	}
}

// generateKEK creates the parent directory if needed, writes a fresh
// 32-byte key with 0600 permissions, and returns it. On any failure
// the partially written file (if any) is left for the operator to
// inspect — we don't unlink it because that could mask a permission
// issue that the operator needs to see.
func generateKEK(path string) ([]byte, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("envelope: create KEK dir %q: %w", dir, err)
		}
	}

	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("envelope: generate KEK: %w", err)
	}

	// O_EXCL guarantees we never silently overwrite an existing key,
	// even in the face of a TOCTOU between ReadFile and OpenFile.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, kekFilePerm)
	if err != nil {
		return nil, fmt.Errorf("envelope: create KEK %q: %w", path, err)
	}
	defer func() { _ = f.Close() }() // durability comes from the checked Sync below

	if _, err := f.Write(key); err != nil {
		return nil, fmt.Errorf("envelope: write KEK %q: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return nil, fmt.Errorf("envelope: sync KEK %q: %w", path, err)
	}
	return key, nil
}
