package handlers

import (
	"context"
	"database/sql"
	"io"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/email"
)

type fakeReportPrefs struct{ out map[string]bool }

func (f *fakeReportPrefs) OptedOut(_ context.Context, userID string) (bool, error) {
	v, ok := f.out[userID]
	if !ok {
		return false, sql.ErrNoRows
	}
	return v, nil
}

func (f *fakeReportPrefs) SetOptOut(_ context.Context, userID string, optOut bool) error {
	if _, ok := f.out[userID]; !ok {
		return sql.ErrNoRows
	}
	f.out[userID] = optOut
	return nil
}

func weeklyReportApp(prefs *fakeReportPrefs) *fiber.App {
	app := fiber.New()
	auth := func(c *fiber.Ctx) error {
		c.Locals("session", &models.Session{UserID: "u1", TenantID: "t1"})
		return c.Next()
	}
	secret := func(context.Context) (string, error) { return "link-secret", nil }
	NewWeeklyReportEmailHandler(prefs, secret, auth).Register(app)
	return app
}

func doReq(t *testing.T, app *fiber.App, method, target, body, contentType string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := app.Test(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestWeeklyReportUnsubscribeFlow(t *testing.T) {
	prefs := &fakeReportPrefs{out: map[string]bool{"u1": false}}
	app := weeklyReportApp(prefs)
	tok := url.QueryEscape(email.SignScoped("link-secret", email.ScopeWeeklyReport, "u1", time.Now()))

	// The link only renders a confirmation page.
	code, body := doReq(t, app, "GET", "/api/email/weekly-report/unsubscribe?token="+tok, "", "")
	if code != 200 || !strings.Contains(body, "/api/email/weekly-report/unsubscribe/confirm") || prefs.out["u1"] {
		t.Fatalf("GET: %d opted-out=%v", code, prefs.out["u1"])
	}

	code, _ = doReq(t, app, "POST", "/api/email/weekly-report/unsubscribe/confirm", "token="+tok, fiber.MIMEApplicationForm)
	if code != 200 || !prefs.out["u1"] {
		t.Fatalf("confirm: %d opted-out=%v", code, prefs.out["u1"])
	}
	code, _ = doReq(t, app, "POST", "/api/email/weekly-report/resubscribe", "token="+tok, fiber.MIMEApplicationForm)
	if code != 200 || prefs.out["u1"] {
		t.Fatalf("resubscribe: %d opted-out=%v", code, prefs.out["u1"])
	}
	code, body = doReq(t, app, "POST", "/api/email/weekly-report/unsubscribe?token="+tok, "List-Unsubscribe=One-Click", fiber.MIMEApplicationForm)
	if code != 200 || !prefs.out["u1"] || !strings.Contains(body, `"weekly_report":false`) {
		t.Fatalf("one-click: %d %s", code, body)
	}
}

func TestWeeklyReportUnsubscribeRejectsBadTokens(t *testing.T) {
	prefs := &fakeReportPrefs{out: map[string]bool{"u1": false}}
	app := weeklyReportApp(prefs)
	marketing := url.QueryEscape(email.SignUnsubscribe("link-secret", "u1", time.Now()))
	if code, _ := doReq(t, app, "POST", "/api/email/weekly-report/unsubscribe?token="+marketing, "", ""); code != 400 || prefs.out["u1"] {
		t.Fatalf("marketing token accepted: %d", code)
	}
	gone := url.QueryEscape(email.SignScoped("link-secret", email.ScopeWeeklyReport, "deleted-user", time.Now()))
	if code, _ := doReq(t, app, "GET", "/api/email/weekly-report/unsubscribe/confirm", "", ""); code != 405 && code != 404 {
		t.Fatalf("confirm via GET: %d", code)
	}
	if code, _ := doReq(t, app, "POST", "/api/email/weekly-report/unsubscribe/confirm", "token="+gone, fiber.MIMEApplicationForm); code != 400 {
		t.Fatalf("deleted membership: %d", code)
	}
}

func TestEmailPreferences(t *testing.T) {
	prefs := &fakeReportPrefs{out: map[string]bool{"u1": false}}
	app := weeklyReportApp(prefs)

	if code, body := doReq(t, app, "GET", "/api/me/email-preferences", "", ""); code != 200 || body != `{"weekly_report":true}` {
		t.Fatalf("get: %d %s", code, body)
	}
	if code, body := doReq(t, app, "PATCH", "/api/me/email-preferences", `{"weekly_report":false}`, fiber.MIMEApplicationJSON); code != 200 || body != `{"weekly_report":false}` || !prefs.out["u1"] {
		t.Fatalf("patch: %d %s", code, body)
	}
	if code, _ := doReq(t, app, "PATCH", "/api/me/email-preferences", `{"weekly_report":"no"}`, fiber.MIMEApplicationJSON); code != 400 {
		t.Fatalf("bad body: %d", code)
	}
}
