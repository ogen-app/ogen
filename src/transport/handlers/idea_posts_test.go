package handlers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/post_actions/compose"
)

type linkRepo struct{ repository.PostIdeaRepository }

func (linkRepo) Link(context.Context, bun.IDB, string, []string) error { return nil }
func (linkRepo) IdeasOfPost(context.Context, string) ([]models.PostSourceIdea, error) {
	return []models.PostSourceIdea{{ID: "i1", Title: "Pricing"}}, nil
}
func (linkRepo) PostsOfIdea(context.Context, string) ([]models.IdeaPostRef, error) {
	return []models.IdeaPostRef{{ID: "p1", Status: models.PostStatusPlanned}}, nil
}

type knownIdeas struct{ repository.IdeaRepository }

func (knownIdeas) GetByID(_ context.Context, id string) (*models.Idea, error) {
	if id == "i1" {
		return &models.Idea{ID: id}, nil
	}
	return nil, sql.ErrNoRows
}

type knownPosts struct{ repository.PostRepository }

func (knownPosts) GetByID(_ context.Context, id string) (*models.Post, error) {
	if id == "p1" {
		return &models.Post{ID: id}, nil
	}
	return nil, sql.ErrNoRows
}

func ideaPostsApp(compile func(context.Context, compose.Input) (*compose.Result, error)) *fiber.App {
	app := fiber.New()
	auth := func(c *fiber.Ctx) error {
		c.Locals(tenantctx.Key, "tenant-1")
		c.Locals("session", &models.Session{TenantID: "tenant-1", UserID: "user-1"})
		return c.Next()
	}
	handlers.NewIdeaPostsHandler(compile, linkRepo{}, knownIdeas{}, knownPosts{}, nil, auth).Register(app)
	return app
}

func call(t *testing.T, app *fiber.App, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestCompileIdeas(t *testing.T) {
	var got compose.Input
	app := ideaPostsApp(func(_ context.Context, in compose.Input) (*compose.Result, error) {
		got = in
		if in.CampaignID == "mixed" {
			return nil, compose.ErrMixedCampaigns
		}
		return &compose.Result{
			Post:  &models.Post{ID: "p9", Status: models.PostStatusPlanned},
			Ideas: []models.Idea{{ID: "i1", Title: "Pricing"}, {ID: "i2", Title: "Story"}},
		}, nil
	})

	status, body := call(t, app, "POST", "/api/ideas/compile", `{"idea_ids":["i1","i2"],"platform_id":"linkedin","target_date":"2026-12-01"}`)
	if status != fiber.StatusCreated {
		t.Fatalf("status = %d: %s", status, body)
	}
	var res struct {
		Post  models.Post             `json:"post"`
		Ideas []models.PostSourceIdea `json:"ideas"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil || res.Post.Status != models.PostStatusPlanned || len(res.Ideas) != 2 {
		t.Fatalf("body = %s (%v)", body, err)
	}
	if got.Actor != "user-1" || got.PlatformID != "linkedin" || got.TargetDate.String() != "2026-12-01" || got.NoteOrigin != models.PostNoteOriginManual {
		t.Fatalf("input = %+v", got)
	}

	if status, _ := call(t, app, "POST", "/api/ideas/compile", `{"idea_ids":["i1"],"campaign_id":"mixed"}`); status != fiber.StatusBadRequest {
		t.Fatalf("a validation error is %d, want 400", status)
	}
	if status, _ := call(t, app, "POST", "/api/ideas/compile", `{"idea_ids":[]}`); status != fiber.StatusBadRequest {
		t.Fatalf("no ideas is %d, want 400", status)
	}
	if status, _ := call(t, app, "POST", "/api/ideas/compile", `{"idea_ids":["i1"],"target_date":"December"}`); status != fiber.StatusBadRequest {
		t.Fatalf("a bad date is %d, want 400", status)
	}
}

func TestIdeaPostLinks(t *testing.T) {
	app := ideaPostsApp(nil)
	if status, body := call(t, app, "GET", "/api/ideas/i1/posts", ""); status != 200 || !strings.Contains(body, `"planned"`) {
		t.Fatalf("posts of idea = %d %s", status, body)
	}
	if status, body := call(t, app, "GET", "/api/posts/p1/ideas", ""); status != 200 || !strings.Contains(body, `"Pricing"`) {
		t.Fatalf("ideas of post = %d %s", status, body)
	}
	if status, _ := call(t, app, "GET", "/api/ideas/nope/posts", ""); status != 404 {
		t.Fatalf("unknown idea = %d", status)
	}
	if status, _ := call(t, app, "GET", "/api/posts/nope/ideas", ""); status != 404 {
		t.Fatalf("unknown post = %d", status)
	}
}
