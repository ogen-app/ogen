package handlers

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/jobs/outbox"
	"github.com/ogen-app/ogen/src/jobs/queues"
	"github.com/ogen-app/ogen/src/pgtest"
)

const receiptsSecretKey = "receipts-secret"

// postResend sends body to the Resend webhook route as Resend would, with a
// fresh svix-id per delivery attempt of the same message id.
func postResend(t *testing.T, app *fiber.App, svixID string, body []byte) int {
	t.Helper()
	ts := strconv.FormatInt(time.Now().Unix(), 10)
	_, sig := svixSecretAndSig(receiptsSecretKey, svixID, ts, body)
	req := httptest.NewRequest(fiber.MethodPost, "/api/webhooks/resend", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("svix-id", svixID)
	req.Header.Set("svix-timestamp", ts)
	req.Header.Set("svix-signature", sig)
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode
}

func TestResendWebhook_ReceiptQueuedOnce(t *testing.T) {
	db := pgtest.MustDB()
	t.Cleanup(func() { _ = db.Close() })
	secret, _ := svixSecretAndSig(receiptsSecretKey, "x", "0", nil)
	h := NewResendWebhookHandler(nil, nil, nil, func(context.Context) (string, error) { return secret, nil })
	h.SetReceipts("Receipts <Receipts@in.getogen.com>", queues.NewEnqueuer(outbox.NewWriter(db.DB)))
	app := fiber.New()
	h.Register(app)

	count := func(kind string) int {
		var n int
		if err := db.DB.QueryRowContext(t.Context(), `SELECT count(*) FROM job_outbox WHERE kind = $1`, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	stripe := []byte(`{"type":"email.received","created_at":"2026-10-28T10:00:00Z","data":{"email_id":"em-1","to":["receipts@in.getogen.com"],"received_for":[]}}`)
	// Resend redelivers the same event under a new attempt.
	for _, svixID := range []string{"msg_1", "msg_1", "msg_2"} {
		if code := postResend(t, app, svixID, stripe); code != fiber.StatusOK {
			t.Fatalf("delivery %s: status %d", svixID, code)
		}
	}
	if n := count(queues.NotifyHarborReceiptKind); n != 1 {
		t.Fatalf("receipt outbox rows = %d, want 1", n)
	}

	// Delivered for the address through a forward: received_for names it.
	forwarded := []byte(`{"type":"email.received","data":{"email_id":"em-2","to":["ops@getogen.com"],"received_for":["receipts@in.getogen.com"]}}`)
	if code := postResend(t, app, "msg_3", forwarded); code != fiber.StatusOK {
		t.Fatalf("forwarded: status %d", code)
	}
	// Another inbound address is acknowledged and ignored.
	other := []byte(`{"type":"email.received","data":{"email_id":"em-3","to":["hello@in.getogen.com"]}}`)
	if code := postResend(t, app, "msg_4", other); code != fiber.StatusOK {
		t.Fatalf("other address: status %d", code)
	}
	// Copied or blind-copied to the address.
	for i, field := range []string{"cc", "bcc"} {
		body := []byte(`{"type":"email.received","data":{"email_id":"em-cc-` + field + `","to":["ops@getogen.com"],"` + field + `":["receipts@in.getogen.com"]}}`)
		if code := postResend(t, app, "msg_cc_"+strconv.Itoa(i), body); code != fiber.StatusOK {
			t.Fatalf("%s: status %d", field, code)
		}
	}
	if n := count(queues.NotifyHarborReceiptKind); n != 4 {
		t.Fatalf("receipt outbox rows = %d, want 4", n)
	}
}

func TestResendWebhook_ReceiptPathOff(t *testing.T) {
	secret, _ := svixSecretAndSig(receiptsSecretKey, "x", "0", nil)
	h := NewResendWebhookHandler(nil, nil, nil, func(context.Context) (string, error) { return secret, nil })
	app := fiber.New()
	h.Register(app)
	body := []byte(`{"type":"email.received","data":{"email_id":"em-1","to":["receipts@in.getogen.com"]}}`)
	if code := postResend(t, app, "msg_1", body); code != fiber.StatusOK {
		t.Fatalf("status %d, want 200 with the receipt path off", code)
	}
}
