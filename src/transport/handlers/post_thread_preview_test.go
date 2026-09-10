package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
)

// fakePreviewPlatformRepo is a minimal PlatformRepository for the stateless
// thread-preview handler test: GetByID returns the canned platform, else ErrNoRows.
type fakePreviewPlatformRepo struct{ p *models.Platform }

func (f *fakePreviewPlatformRepo) List(context.Context) ([]models.Platform, error) { return nil, nil }
func (f *fakePreviewPlatformRepo) Create(context.Context, *models.Platform) error  { return nil }
func (f *fakePreviewPlatformRepo) GetByID(_ context.Context, id string) (*models.Platform, error) {
	if f.p != nil && f.p.ID == id {
		return f.p, nil
	}
	return nil, sql.ErrNoRows
}
func (f *fakePreviewPlatformRepo) Update(context.Context, *models.Platform) error { return nil }
func (f *fakePreviewPlatformRepo) Delete(context.Context, string) (bool, error)   { return false, nil }
func (f *fakePreviewPlatformRepo) ListEnabled(context.Context) ([]models.Platform, error) {
	return nil, nil
}
func (f *fakePreviewPlatformRepo) GetByZernioID(context.Context, string) (*models.Platform, error) {
	return nil, sql.ErrNoRows
}
func (f *fakePreviewPlatformRepo) SetEnabled(context.Context, string, bool) (*models.Platform, error) {
	return f.p, nil
}
func (f *fakePreviewPlatformRepo) InUseCounts(context.Context, *models.Platform) (int, int, error) {
	return 0, 0, nil
}

func previewApp() *fiber.App {
	app := fiber.New()
	h := &PostsHandler{
		platformRepo: &fakePreviewPlatformRepo{p: &models.Platform{
			ID:               "xplat",
			Name:             "X",
			PostTypes:        models.PostTypeMap{"thread": "Thread"},
			TextConstraints:  models.TextConstraints{MaxContentChars: 280},
			ImageConstraints: models.ImageConstraints{MaxAttachmentsPerPost: 4},
		}},
		auth: func(c *fiber.Ctx) error { return c.Next() },
	}
	h.Register(app)
	return app
}

func doPreview(t *testing.T, app *fiber.App, content, platformID string) (*http.Response, previewThreadResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"content": content, "platform_id": platformID})
	req := httptest.NewRequest("POST", "/api/posts/thread/preview", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, -1)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	var out previewThreadResponse
	if resp.StatusCode == http.StatusOK {
		_ = json.NewDecoder(resp.Body).Decode(&out)
	}
	return resp, out
}

func TestPreviewThread_ManualSplitValid(t *testing.T) {
	app := previewApp()
	resp, out := doPreview(t, app, "root\n---\nreply", "xplat")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.Limit != 280 {
		t.Errorf("limit = %d, want 280", out.Limit)
	}
	if len(out.Segments) != 2 || out.Segments[0].Content != "root" || out.Segments[1].Content != "reply" {
		t.Fatalf("segments = %+v, want [root reply]", out.Segments)
	}
	if out.Segments[0].CharCount != 4 || out.Segments[1].CharCount != 5 {
		t.Errorf("char counts wrong: %+v", out.Segments)
	}
	if !out.Valid || len(out.Errors) != 0 {
		t.Errorf("want valid with no errors, got valid=%v errors=%+v", out.Valid, out.Errors)
	}
}

func TestPreviewThread_TooFewSegments(t *testing.T) {
	app := previewApp()
	resp, out := doPreview(t, app, "only one", "xplat")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if len(out.Segments) != 1 {
		t.Errorf("segments = %d, want 1", len(out.Segments))
	}
	if out.Valid {
		t.Error("want valid=false for a single-message thread")
	}
	found := false
	for _, e := range out.Errors {
		if e.Rule == "thread_segment_count" {
			found = true
		}
	}
	if !found {
		t.Errorf("want thread_segment_count error, got %+v", out.Errors)
	}
}

func TestPreviewThread_OverLimitSegment(t *testing.T) {
	app := previewApp()
	resp, out := doPreview(t, app, "root\n---\n"+strings.Repeat("a", 281), "xplat")
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if out.Valid {
		t.Error("want valid=false for an over-limit segment")
	}
	found := false
	for _, e := range out.Errors {
		if e.Rule == "max_content_chars" {
			found = true
		}
	}
	if !found {
		t.Errorf("want max_content_chars error, got %+v", out.Errors)
	}
}

func TestPreviewThread_MissingPlatform(t *testing.T) {
	app := previewApp()
	resp, _ := doPreview(t, app, "root\n---\nreply", "")
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Errorf("missing platform_id: status = %d, want 400", resp.StatusCode)
	}
}

func TestPreviewThread_UnknownPlatform(t *testing.T) {
	app := previewApp()
	resp, _ := doPreview(t, app, "root\n---\nreply", "nope")
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown platform: status = %d, want 404", resp.StatusCode)
	}
}
