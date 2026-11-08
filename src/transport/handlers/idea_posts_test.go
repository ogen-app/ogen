package handlers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"slices"
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

type fakeDrafts struct {
	got     compose.DraftInput
	max     int
	planErr error
	runErr  error
}

func (f *fakeDrafts) PlanDraft(_ context.Context, in compose.DraftInput, maxPosts int) (*compose.DraftPlan, error) {
	f.got, f.max = in, maxPosts
	if f.planErr != nil {
		return nil, f.planErr
	}
	return &compose.DraftPlan{Jobs: make([]compose.DraftJob, 2)}, nil
}

func (f *fakeDrafts) RunDraft(_ context.Context, _ *compose.DraftPlan, onPost func(compose.DraftedPost)) (*compose.DraftResult, error) {
	p := compose.DraftedPost{Written: compose.Written{PostID: "p1", PlatformID: "linkedin"}, IdeaIDs: []string{"i1"}}
	onPost(p)
	if f.runErr != nil {
		return nil, f.runErr
	}
	return &compose.DraftResult{Posts: []compose.DraftedPost{p}}, nil
}

func TestDraftIdeas(t *testing.T) {
	drafts := &fakeDrafts{}
	app := fiber.New()
	auth := func(c *fiber.Ctx) error {
		c.Locals(tenantctx.Key, "tenant-1")
		c.Locals("session", &models.Session{TenantID: "tenant-1", UserID: "user-1"})
		return c.Next()
	}
	up := true
	handlers.NewIdeaPostsHandler(nil, linkRepo{}, knownIdeas{}, knownPosts{}, nil, auth).
		WithDraft(drafts, 5, func() bool { return up }).Register(app)

	status, body := call(t, app, "POST", "/api/ideas/draft", `{"idea_ids":["i1","i2"],"platform_ids":["linkedin"],"together":true,"instruction":"punchier"}`)
	if status != 200 {
		t.Fatalf("status = %d: %s", status, body)
	}
	frames := parseSSEStream(strings.NewReader(body))
	if len(frames) != 2 || frames[0].event != "post" || frames[1].event != "complete" {
		t.Fatalf("events = %+v", frames)
	}
	var post compose.DraftedPost
	if err := json.Unmarshal([]byte(frames[0].data), &post); err != nil || post.PostID != "p1" || !slices.Equal(post.IdeaIDs, []string{"i1"}) {
		t.Fatalf("post event = %s (%v)", frames[0].data, err)
	}
	var done compose.DraftResult
	if err := json.Unmarshal([]byte(frames[1].data), &done); err != nil || len(done.Posts) != 1 || done.Posts[0].PostID != "p1" {
		t.Fatalf("complete event = %s (%v)", frames[1].data, err)
	}
	if !drafts.got.Together || drafts.got.Instruction != "punchier" || drafts.got.Actor != "user-1" || drafts.max != 5 {
		t.Fatalf("input = %+v, max %d", drafts.got, drafts.max)
	}

	// A request that can't run is a plain 400 before the stream.
	drafts.planErr = compose.ErrNoPlatform
	if status, _ := call(t, app, "POST", "/api/ideas/draft", `{"idea_ids":["i1"]}`); status != 400 {
		t.Fatalf("plan refusal = %d", status)
	}
	// A writer failure ends the stream with an error, after what was written.
	drafts.planErr, drafts.runErr = nil, errors.New("writer down")
	_, body = call(t, app, "POST", "/api/ideas/draft", `{"idea_ids":["i1"]}`)
	frames = parseSSEStream(strings.NewReader(body))
	if len(frames) != 2 || frames[0].event != "post" || frames[1].event != "error" {
		t.Fatalf("events = %+v", frames)
	}
	var failed struct {
		Message string `json:"message"`
		Code    int    `json:"code"`
	}
	if err := json.Unmarshal([]byte(frames[1].data), &failed); err != nil || failed.Code != 500 || failed.Message != "writer down" {
		t.Fatalf("error event = %s (%v)", frames[1].data, err)
	}
	up = false
	if status, _ := call(t, app, "POST", "/api/ideas/draft", `{"idea_ids":["i1"]}`); status != 503 {
		t.Fatalf("writer unavailable = %d", status)
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
