package handlers_test

import (
	"os"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

// TestMain swaps the production argon2id cost profile for the
// fast-test one before any spec runs.
//
// Production password hashing is intentionally heavy (64 MiB / 3
// iterations); under `-procs=2 -race` a single hash can take >1s,
// which exceeds fiber's default 1000ms `app.Test(req)` timeout. The
// observable symptom was a flaky, non-deterministic set of handler
// failures on every `make test` run — most consistently in the
// BeforeEach paths that seed test users (`createUser`, `seedUser`,
// the inline "Frank" / "Admin" inserts in setupApp).
//
// Tests don't care about the cost — they only need a deterministic
// round-trip — so we override the package-level
// models.DefaultArgon2Params before any *_test.go init() runs auth
// fixtures. VerifyPassword reads the cost params from the encoded
// hash itself, so previously-stored hashes (none in tests, but a
// generally-safe property) keep working too.
func TestMain(m *testing.M) {
	models.DefaultArgon2Params = models.FastTestArgon2Params
	os.Exit(m.Run())
}
