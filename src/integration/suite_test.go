//go:build integration

package integration_test

import (
	"context"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/pgtest"
)

// tenantCtx is the default-tenant context for integration fixtures that seed or
// query tenant-scoped models directly (CON-97 scoping fails closed without a
// tenant). Requests through the authenticated API already carry the session's
// tenant, so this is only for direct DB/repo access in test setup.
func tenantCtx() context.Context {
	return tenantctx.With(context.Background(), models.DefaultTenantID)
}

func TestIntegration(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Integration Suite")
}

// seedTenantUser inserts a user in the default tenant directly, bypassing the
// (now auth-gated) POST /api/users route — since CON-97, signup is the only
// HTTP path that creates the first user. Login via POST /api/sessions still
// works because the user row exists.
func seedTenantUser(db *bun.DB, name, email, password string) *models.User {
	GinkgoHelper()
	hash, err := models.HashPassword(password)
	Expect(err).NotTo(HaveOccurred())
	id, err := models.NewID()
	Expect(err).NotTo(HaveOccurred())
	// Since CON-147 the credential lives on an accounts row; the users row is a
	// per-(account, tenant) membership FKing that account. Seed the account first
	// (1:1 with the user id here) so the NOT NULL account_id FK holds.
	acct := &models.Account{
		ID:           id,
		Email:        email,
		PasswordHash: hash,
		Name:         name,
	}
	_, err = db.NewInsert().Model(acct).Exec(context.Background())
	Expect(err).NotTo(HaveOccurred())
	u := &models.User{
		ID:        id,
		AccountID: id,
		TenantID:  models.DefaultTenantID,
		Name:      name,
		Email:     email,
	}
	_, err = db.NewInsert().Model(u).Exec(context.Background())
	Expect(err).NotTo(HaveOccurred())
	return u
}

// mustOpenIntegrationDB returns a fresh, fully-migrated Postgres database.
// Each Describe's BeforeAll gets its own database, so suites
// stay isolated. The pool is sized up from pgtest's default so the
// concurrent-upload integration test can exercise parallel handler requests.
func mustOpenIntegrationDB() *bun.DB {
	db := pgtest.MustDB()
	db.SetMaxOpenConns(10)
	db.SetMaxIdleConns(5)
	return db
}
