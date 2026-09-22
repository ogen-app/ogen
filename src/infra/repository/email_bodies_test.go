package repository_test

import (
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
)

// TestEmailBodyInsertAndGet covers the CON-306 body store: a stored body
// round-trips, an absent id yields (nil, nil) so the caller falls back to the
// live Resend fetch, and Insert is idempotent on the primary key (a retried job
// that re-persists the same log id is a no-op, not a duplicate-key error).
func TestEmailBodyInsertAndGet(t *testing.T) {
	db := openMigratedDB(t)
	logs := repository.NewEmailLogRepository(db)
	bodies := repository.NewEmailBodyRepository(db)
	ctx := t.Context()

	// Seed the parent email_logs row so the email_bodies.email_log_id FK is
	// satisfied (openMigratedDB disables FK enforcement, but seeding keeps the
	// test realistic and correct regardless).
	id := mustID(t)
	if err := logs.Insert(ctx, &models.EmailLog{
		ID: id, TemplateID: "welcome", Kind: models.EmailKindTransactional,
		ToEmail: "a@b.com", Status: models.EmailLogSent, Provider: models.ProviderResend,
	}); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	if err := bodies.Insert(ctx, &models.EmailBody{
		EmailLogID: id,
		Subject:    "Welcome",
		HTML:       "<p>hi</p>",
		Text:       "hi",
		From:       "Ogen <hello@ogen.test>",
		ReplyTo:    "reply@ogen.test",
	}); err != nil {
		t.Fatalf("insert: %v", err)
	}

	got, err := bodies.GetByEmailLogID(ctx, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got == nil {
		t.Fatal("stored body not found")
	}
	if got.Subject != "Welcome" || got.HTML != "<p>hi</p>" || got.Text != "hi" {
		t.Fatalf("body content mismatch: %+v", got)
	}
	if got.From != "Ogen <hello@ogen.test>" || got.ReplyTo != "reply@ogen.test" {
		t.Fatalf("envelope mismatch: from=%q replyTo=%q", got.From, got.ReplyTo)
	}
	if got.CreatedAt.IsZero() {
		t.Fatal("created_at default not applied")
	}

	// Absent id → (nil, nil), so GetTenantEmail falls back to the live fetch.
	if b, err := bodies.GetByEmailLogID(ctx, mustID(t)); err != nil || b != nil {
		t.Fatalf("absent lookup = (%v, %v), want (nil, nil)", b, err)
	}

	// Idempotent on the PK: a re-persist keeps the first-written row.
	if err := bodies.Insert(ctx, &models.EmailBody{EmailLogID: id, Subject: "CHANGED", HTML: "x", Text: "x"}); err != nil {
		t.Fatalf("re-insert: %v", err)
	}
	if again, _ := bodies.GetByEmailLogID(ctx, id); again == nil || again.Subject != "Welcome" {
		t.Fatalf("re-insert should be a no-op (idempotent), got %+v", again)
	}

	// Empty id is a defensive no-op (no row, no error) on both read and write.
	if err := bodies.Insert(ctx, &models.EmailBody{EmailLogID: ""}); err != nil {
		t.Fatalf("empty-id insert should be a no-op: %v", err)
	}
	if b, err := bodies.GetByEmailLogID(ctx, ""); err != nil || b != nil {
		t.Fatalf("empty-id lookup = (%v, %v), want (nil, nil)", b, err)
	}
}
