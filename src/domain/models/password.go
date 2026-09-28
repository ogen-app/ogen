package models

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2Params holds the cost parameters used for hashing.
// These are stored inside the encoded hash so verification always uses
// the same parameters the hash was produced with.
type Argon2Params struct {
	Memory      uint32 // KiB
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// ProductionArgon2Params is the cost profile used for real password
// hashing — 64 MiB / 3 iterations / parallelism 2. This is the default
// used by HashPassword.
var ProductionArgon2Params = Argon2Params{
	Memory:      64 * 1024, // 64 MiB
	Iterations:  3,
	Parallelism: 2,
	SaltLength:  16,
	KeyLength:   32,
}

// FastTestArgon2Params is a deliberately low-cost profile for test
// runs. Production's 64 MiB + 3-iter argon2id can take >1s under
// `-procs=2 -race` — well beyond fiber's default 1000ms `app.Test`
// timeout — and produces hard-to-diagnose flakes. Tests
// that exercise auth code may assign this to DefaultArgon2Params in
// a TestMain (or an init in a *_test.go file) to keep hash latency
// in single-digit milliseconds without changing the production
// cost profile.
var FastTestArgon2Params = Argon2Params{
	Memory:      8, // 8 KiB — RFC 9106 minimum
	Iterations:  1,
	Parallelism: 1,
	SaltLength:  16,
	KeyLength:   32,
}

// DefaultArgon2Params is what HashPassword reads at call time. It's a
// var (not a const) so tests can swap in FastTestArgon2Params before
// the first call. VerifyPassword does NOT consult this — it reads the
// params encoded in the stored hash, so verifying a production hash
// after switching to fast params still works correctly.
var DefaultArgon2Params = ProductionArgon2Params

// HashPassword hashes password using argon2id and returns a PHC-encoded string
// that includes the salt and all cost parameters.
func HashPassword(password string) (string, error) {
	p := DefaultArgon2Params

	salt := make([]byte, p.SaltLength)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}

	hash := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)

	encoded := fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		p.Memory, p.Iterations, p.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
	return encoded, nil
}

// VerifyPassword returns true when password matches the PHC-encoded hash.
func VerifyPassword(password, encoded string) (bool, error) {
	p, salt, hash, err := decodeArgon2Hash(encoded)
	if err != nil {
		return false, fmt.Errorf("decode hash: %w", err)
	}

	candidate := argon2.IDKey([]byte(password), salt, p.Iterations, p.Memory, p.Parallelism, p.KeyLength)
	return subtle.ConstantTimeCompare(hash, candidate) == 1, nil
}

func decodeArgon2Hash(encoded string) (*Argon2Params, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	// Expected format: ["", "argon2id", "v=19", "m=65536,t=3,p=2", "<salt>", "<hash>"]
	if len(parts) != 6 || parts[1] != "argon2id" {
		return nil, nil, nil, fmt.Errorf("invalid hash format")
	}

	var version int
	if _, err := fmt.Sscanf(parts[2], "v=%d", &version); err != nil {
		return nil, nil, nil, fmt.Errorf("parse version: %w", err)
	}
	if version != argon2.Version {
		return nil, nil, nil, fmt.Errorf("unsupported argon2 version %d", version)
	}

	var p Argon2Params
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &p.Memory, &p.Iterations, &p.Parallelism); err != nil {
		return nil, nil, nil, fmt.Errorf("parse params: %w", err)
	}

	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode salt: %w", err)
	}
	p.SaltLength = uint32(len(salt))

	hash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil {
		return nil, nil, nil, fmt.Errorf("decode hash: %w", err)
	}
	p.KeyLength = uint32(len(hash))

	return &p, salt, hash, nil
}
