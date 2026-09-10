package repository_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

func TestEmailTemplateRepoInsertIfAbsent(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewEmailTemplateRepository(db)
	ctx := t.Context()

	tmpl := &models.EmailTemplate{Key: "welcome", Subject: "Hi", HTML: "<p>[[ .Name ]]</p>", Text: "hi", Kind: models.EmailKindTransactional, Version: 1}

	created, err := repo.InsertIfAbsent(ctx, tmpl)
	if err != nil || !created {
		t.Fatalf("first insert: created=%v err=%v", created, err)
	}

	// A second insert with an edited body must be a no-op (DB row wins).
	created2, err := repo.InsertIfAbsent(ctx, &models.EmailTemplate{Key: "welcome", Subject: "Edited", HTML: "x", Text: "x", Kind: models.EmailKindTransactional, Version: 1})
	if err != nil || created2 {
		t.Fatalf("second insert: created=%v err=%v (want false, nil)", created2, err)
	}

	got, err := repo.GetByKey(ctx, "welcome")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Subject != "Hi" {
		t.Fatalf("subject: got %q, want original 'Hi' (re-seed must not clobber)", got.Subject)
	}

	if _, err := repo.GetByKey(ctx, "nope"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing key: got %v, want sql.ErrNoRows", err)
	}
}

func TestEmailTemplateRepoVariables(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewEmailTemplateRepository(db)
	ctx := t.Context()

	tmpl := &models.EmailTemplate{
		Key: "welcome", Subject: "Hi", HTML: "<p>[[ .Name ]]</p>", Text: "hi",
		Kind: models.EmailKindTransactional, Version: 1,
		Variables: models.StringMap{"Name": "recipient name", "AppURL": "app link"},
	}
	if _, err := repo.InsertIfAbsent(ctx, tmpl); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// Round-trips through the jsonb column.
	got, err := repo.GetByKey(ctx, "welcome")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Variables["Name"] != "recipient name" || got.Variables["AppURL"] != "app link" {
		t.Fatalf("variables round-trip: %#v", got.Variables)
	}

	// SyncVariables replaces the docs wholesale, leaving copy (subject) untouched.
	if err := repo.SyncVariables(ctx, "welcome", models.StringMap{"Name": "updated"}); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got, _ = repo.GetByKey(ctx, "welcome")
	if got.Variables["Name"] != "updated" || got.Subject != "Hi" {
		t.Fatalf("sync should update variables only: vars=%#v subject=%q", got.Variables, got.Subject)
	}
	if _, ok := got.Variables["AppURL"]; ok {
		t.Error("sync should replace the whole variables map, dropping AppURL")
	}

	// No-op on a missing key (no error).
	if err := repo.SyncVariables(ctx, "nope", models.StringMap{"x": "y"}); err != nil {
		t.Fatalf("sync missing key: %v", err)
	}
}

func TestEmailSuppressionRepoGate(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewEmailSuppressionRepository(db)
	ctx := t.Context()

	// Marketing unsubscribe for a mixed-case address.
	if err := repo.Upsert(ctx, &models.EmailSuppression{ID: mustID(t), Email: "User@X.com", Scope: models.EmailSuppressionScopeMarketing, Reason: models.EmailSuppressionReasonUnsubscribe, Source: models.EmailSuppressionSourceUser}); err != nil {
		t.Fatalf("upsert marketing: %v", err)
	}

	// created_at must come from the DB default (Upsert never sets it): the model
	// tag needs nullzero, else bun inserts the Go zero time (0001-01-01).
	var row models.EmailSuppression
	if err := db.NewSelect().Model(&row).Where("email = ? AND scope = ?", "user@x.com", models.EmailSuppressionScopeMarketing).Scan(ctx); err != nil {
		t.Fatalf("select back: %v", err)
	}
	if row.CreatedAt.IsZero() || row.CreatedAt.Year() < 2000 {
		t.Fatalf("created_at not populated by DB default: %v", row.CreatedAt)
	}

	// Marketing is blocked; transactional is not (normalised match).
	if ok, err := repo.IsSuppressed(ctx, "user@x.com", models.EmailKindMarketing); err != nil || !ok {
		t.Fatalf("marketing suppressed: ok=%v err=%v", ok, err)
	}
	if ok, err := repo.IsSuppressed(ctx, "USER@x.com", models.EmailKindTransactional); err != nil || ok {
		t.Fatalf("transactional must not be blocked by a marketing unsubscribe: ok=%v err=%v", ok, err)
	}

	// Idempotent upsert on (email, scope): no error, still blocks marketing.
	if err := repo.Upsert(ctx, &models.EmailSuppression{ID: mustID(t), Email: "user@x.com", Scope: models.EmailSuppressionScopeMarketing, Reason: models.EmailSuppressionReasonManual, Source: models.EmailSuppressionSourceOps}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}

	// A hard bounce (all scope) now blocks transactional too.
	if err := repo.Upsert(ctx, &models.EmailSuppression{ID: mustID(t), Email: "user@x.com", Scope: models.EmailSuppressionScopeAll, Reason: models.EmailSuppressionReasonBounce, Source: models.EmailSuppressionSourceWebhook}); err != nil {
		t.Fatalf("upsert all: %v", err)
	}
	if ok, err := repo.IsSuppressed(ctx, "user@x.com", models.EmailKindTransactional); err != nil || !ok {
		t.Fatalf("all-scope must block transactional: ok=%v err=%v", ok, err)
	}

	// A clean address is never suppressed.
	if ok, err := repo.IsSuppressed(ctx, "fresh@x.com", models.EmailKindMarketing); err != nil || ok {
		t.Fatalf("clean address: ok=%v err=%v", ok, err)
	}
}

func TestEmailSuppressionRemoveMarketing(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewEmailSuppressionRepository(db)
	ctx := t.Context()

	up := func(scope models.EmailSuppressionScope, reason models.EmailSuppressionReason) {
		t.Helper()
		if err := repo.Upsert(ctx, &models.EmailSuppression{ID: mustID(t), Email: "user@x.com", Scope: scope, Reason: reason, Source: models.EmailSuppressionSourceUser}); err != nil {
			t.Fatalf("upsert %s: %v", scope, err)
		}
	}

	// Resubscribe from a marketing-only suppression fully clears it.
	up(models.EmailSuppressionScopeMarketing, models.EmailSuppressionReasonUnsubscribe)
	if err := repo.RemoveMarketing(ctx, "User@X.com"); err != nil { // mixed case → normalised
		t.Fatalf("remove: %v", err)
	}
	if ok, _ := repo.IsSuppressed(ctx, "user@x.com", models.EmailKindMarketing); ok {
		t.Fatal("marketing suppression should be gone after resubscribe")
	}

	// A hard bounce (all) must survive resubscribe.
	up(models.EmailSuppressionScopeMarketing, models.EmailSuppressionReasonUnsubscribe)
	up(models.EmailSuppressionScopeAll, models.EmailSuppressionReasonBounce)
	if err := repo.RemoveMarketing(ctx, "user@x.com"); err != nil {
		t.Fatalf("remove: %v", err)
	}
	marketingLeft, _ := db.NewSelect().Model((*models.EmailSuppression)(nil)).
		Where("email = ?", "user@x.com").Where("scope = ?", models.EmailSuppressionScopeMarketing).Exists(ctx)
	if marketingLeft {
		t.Error("marketing row should be deleted even when an all-scope row exists")
	}
	if ok, _ := repo.IsSuppressed(ctx, "user@x.com", models.EmailKindTransactional); !ok {
		t.Error("all-scope suppression must survive resubscribe")
	}

	// No-op on a clean address.
	if err := repo.RemoveMarketing(ctx, "nobody@x.com"); err != nil {
		t.Fatalf("remove clean: %v", err)
	}
}

func TestEmailLogRepo(t *testing.T) {
	db := openMigratedDB(t)
	repo := repository.NewEmailLogRepository(db)
	ctx := t.Context()

	insert := func(idem, msgID string, status models.EmailLogStatus) {
		t.Helper()
		if err := repo.Insert(ctx, &models.EmailLog{
			ID: mustID(t), TemplateID: "welcome", Kind: models.EmailKindTransactional,
			ToEmail: "a@b.com", Status: status, Provider: models.ProviderResend,
			ProviderMessageID: msgID, IdempotencyKey: idem,
		}); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}

	insert("welcome:u1", "msg_1", models.EmailLogSent)

	// created_at/updated_at come from the DB default (Insert never sets them):
	// without nullzero these would be 0001-01-01 and the retention sweep would
	// delete every audit row on its first tick.
	var lg models.EmailLog
	if err := db.NewSelect().Model(&lg).Where("idempotency_key = ?", "welcome:u1").Scan(ctx); err != nil {
		t.Fatalf("select back: %v", err)
	}
	if lg.CreatedAt.Year() < 2000 || lg.UpdatedAt.Year() < 2000 {
		t.Fatalf("email_log timestamps not defaulted: created=%v updated=%v", lg.CreatedAt, lg.UpdatedAt)
	}

	// Webhook updates the row by provider message id.
	updated, err := repo.UpdateStatusByProviderMessageID(ctx, "msg_1", models.EmailLogBounced)
	if err != nil || !updated {
		t.Fatalf("update by msg id: updated=%v err=%v", updated, err)
	}
	if noMatch, _ := repo.UpdateStatusByProviderMessageID(ctx, "nope", models.EmailLogBounced); noMatch {
		t.Fatal("update matched a non-existent message id")
	}

	// Partial unique index: a second row with the same idempotency key is rejected.
	if err := repo.Insert(ctx, &models.EmailLog{
		ID: mustID(t), TemplateID: "welcome", Kind: models.EmailKindTransactional,
		ToEmail: "a@b.com", Status: models.EmailLogSent, Provider: models.ProviderResend,
		IdempotencyKey: "welcome:u1",
	}); err == nil {
		t.Fatal("duplicate idempotency_key was accepted; want unique violation")
	}

	// Empty idempotency keys are NULL, so they don't collide.
	insert("", "", models.EmailLogSkippedDisabled)
	insert("", "", models.EmailLogSkippedDisabled)

	// Retention sweep removes everything older than the cutoff.
	n, err := repo.DeleteOlderThan(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n < 3 {
		t.Fatalf("deleted %d rows, want >= 3", n)
	}
}
