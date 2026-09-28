package handlers

import (
	"bufio"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/publishers"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

type testValidationError struct{ msg string }

func (e *testValidationError) Error() string { return e.msg }

type testAIError struct{ msg string }

func (e *testAIError) Error() string { return e.msg }

// runInCtx executes fn inside a real fiber request so helpers see a live
// *fiber.Ctx; setup runs first to seed Locals.
func runInCtx(t *testing.T, path, route string, setup func(c *fiber.Ctx), fn func(c *fiber.Ctx) error) (int, string) {
	t.Helper()
	app := fiber.New()
	app.Get(route, func(c *fiber.Ctx) error {
		if setup != nil {
			setup(c)
		}
		return fn(c)
	})
	resp, err := app.Test(httptest.NewRequest("GET", path, nil), -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(body)
}

func withSession(s *models.Session) func(c *fiber.Ctx) {
	return func(c *fiber.Ctx) { c.Locals("session", s) }
}

func TestFlowError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantCode int
		wantMsg  string
	}{
		{"validation", &testValidationError{"bad input"}, fiber.StatusBadRequest, "bad input"},
		{"wrapped validation", fmt.Errorf("run: %w", &testValidationError{"missing"}), fiber.StatusBadRequest, "missing"},
		{"ai", &testAIError{"model down"}, fiber.StatusBadGateway, "model down"},
		{"wrapped ai", fmt.Errorf("run: %w", &testAIError{"timeout"}), fiber.StatusBadGateway, "timeout"},
		{"other", errors.New("boom"), fiber.StatusInternalServerError, "boom"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := flowError[*testValidationError, *testAIError](tt.err)
			if got.Code != tt.wantCode || got.Message != tt.wantMsg {
				t.Fatalf("got %+v, want {%q %d}", got, tt.wantMsg, tt.wantCode)
			}
		})
	}
}

func TestSessionFrom(t *testing.T) {
	code, _ := runInCtx(t, "/", "/", nil, func(c *fiber.Ctx) error {
		_, err := sessionFrom(c)
		return err
	})
	if code != fiber.StatusUnauthorized {
		t.Fatalf("missing session: got %d, want 401", code)
	}

	code, body := runInCtx(t, "/", "/", withSession(&models.Session{UserID: "u1"}), func(c *fiber.Ctx) error {
		s, err := sessionFrom(c)
		if err != nil {
			return err
		}
		return c.SendString(s.UserID + "|" + actorID(c))
	})
	if code != fiber.StatusOK || body != "u1|u1" {
		t.Fatalf("with session: got %d %q", code, body)
	}

	_, body = runInCtx(t, "/", "/", nil, func(c *fiber.Ctx) error {
		return c.SendString("[" + actorID(c) + "]")
	})
	if body != "[]" {
		t.Fatalf("actorID without session: got %q", body)
	}
}

func TestLoad(t *testing.T) {
	fetch := func(_ context.Context, id string) (*models.Post, error) {
		switch id {
		case "p1":
			return &models.Post{ID: id}, nil
		case "gone":
			return nil, sql.ErrNoRows
		default:
			return nil, errors.New("db down")
		}
	}
	handler := func(c *fiber.Ctx) error {
		p, err := load(c, fetch, "post not found")
		if err != nil {
			return err
		}
		return c.SendString(p.ID)
	}
	tests := []struct {
		path     string
		wantCode int
		wantBody string
	}{
		{"/posts/p1", fiber.StatusOK, "p1"},
		{"/posts/gone", fiber.StatusNotFound, "post not found"},
		{"/posts/other", fiber.StatusInternalServerError, "db down"},
	}
	for _, tt := range tests {
		code, body := runInCtx(t, tt.path, "/posts/:id", nil, handler)
		if code != tt.wantCode || body != tt.wantBody {
			t.Errorf("%s: got %d %q, want %d %q", tt.path, code, body, tt.wantCode, tt.wantBody)
		}
	}

	code, body := runInCtx(t, "/posts/p1/notes", "/posts/:post_id/notes", nil, func(c *fiber.Ctx) error {
		p, err := loadParam(c, "post_id", fetch, "post not found")
		if err != nil {
			return err
		}
		return c.SendString(p.ID)
	})
	if code != fiber.StatusOK || body != "p1" {
		t.Fatalf("loadParam: got %d %q", code, body)
	}
}

func TestRequireQuota(t *testing.T) {
	// A nil limiter allows everything; the hold records whether a tenant was
	// present so dispatch is only attempted for gated requests.
	_, body := runInCtx(t, "/", "/", nil, func(c *fiber.Ctx) error {
		q, err := requireQuota(c, nil, "content_bank_assets")
		if err != nil {
			return err
		}
		q.dispatch(reqCtx(c))
		return c.SendString(fmt.Sprint(q.held))
	})
	if body != "false" {
		t.Fatalf("no tenant: held = %s, want false", body)
	}

	_, body = runInCtx(t, "/", "/", func(c *fiber.Ctx) { c.Locals(tenantctx.Key, "t1") }, func(c *fiber.Ctx) error {
		q, err := requireQuotaAmount(c, nil, "media_storage_bytes", 42)
		if err != nil {
			return err
		}
		q.dispatch(reqCtx(c))
		return c.SendString(fmt.Sprintln(q.held, q.tenantID, q.decision.Allowed))
	})
	if body != "true t1 true\n" {
		t.Fatalf("with tenant: got %q", body)
	}
}

func TestEnsureMutable(t *testing.T) {
	for _, st := range []models.PostStatus{models.PostStatusDraft, models.PostStatusReadyForPublish} {
		if err := ensureMutable(&models.Post{Status: st}); err != nil {
			t.Errorf("%s: got %v, want nil", st, err)
		}
	}
	for _, st := range []models.PostStatus{models.PostStatusScheduled, models.PostStatusPublished} {
		err := ensureMutable(&models.Post{Status: st})
		fe, ok := errors.AsType[*fiber.Error](err)
		if !ok || fe.Code != fiber.StatusConflict || fe.Message != errPostSubmittedAttachments {
			t.Errorf("%s: got %v, want 409", st, err)
		}
	}
}

func TestStreamFlow(t *testing.T) {
	app := fiber.New()
	app.Get("/", func(c *fiber.Ctx) error {
		streamFlow(c, func(emit sseEmit) {
			emit("step", map[string]string{"step": "a"})
			emit("error", flowErrorPayload{Message: "x", Code: 502})
		})
		return nil
	})
	resp, err := app.Test(httptest.NewRequest("GET", "/", nil), -1)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	for k, want := range map[string]string{
		"Content-Type":      "text/event-stream",
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
	} {
		if got := resp.Header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	body, _ := io.ReadAll(resp.Body)
	want := "event: step\ndata: {\"step\":\"a\"}\n\nevent: error\ndata: {\"message\":\"x\",\"code\":502}\n\n"
	if string(body) != want {
		t.Fatalf("body = %q, want %q", body, want)
	}
}

func TestWriteNamedEvent(t *testing.T) {
	var sb strings.Builder
	w := bufio.NewWriter(&sb)
	writeNamedEvent(w, "complete", []int{1, 2})
	if got := sb.String(); got != "event: complete\ndata: [1,2]\n\n" {
		t.Fatalf("got %q", got)
	}
}

func TestPlatformIndexMatch(t *testing.T) {
	idx := newPlatformIndex([]models.Platform{{ID: "abc", Name: "LinkedIn"}, {ID: "def", Name: "X"}})
	tests := []struct {
		view   publishers.PlatformView
		wantID string
		wantOK bool
	}{
		{publishers.PlatformView{OgenPlatformID: "abc"}, "abc", true},
		{publishers.PlatformView{OgenPlatformID: "linkedin", PlatformName: "linkedin"}, "abc", true},
		{publishers.PlatformView{PlatformName: "x"}, "def", true},
		{publishers.PlatformView{OgenPlatformID: "zzz", PlatformName: "tiktok"}, "", false},
		{publishers.PlatformView{}, "", false},
	}
	for _, tt := range tests {
		id, ok := idx.match(tt.view)
		if id != tt.wantID || ok != tt.wantOK {
			t.Errorf("match(%+v) = %q,%v want %q,%v", tt.view, id, ok, tt.wantID, tt.wantOK)
		}
	}
	if got := toAccountViews(nil); got == nil || len(got) != 0 {
		t.Errorf("toAccountViews(nil) = %#v, want empty non-nil", got)
	}
}
