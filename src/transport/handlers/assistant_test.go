package handlers_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/viewcontext"
	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/viewresolve"
)

type threadRepo struct {
	repository.AssistantMessageRepository
	kind models.AssistantScopeKind
	id   string
}

func (r *threadRepo) ListRecent(_ context.Context, kind models.AssistantScopeKind, id string, _ int) ([]models.AssistantMessage, error) {
	r.kind, r.id = kind, id
	return nil, nil
}

type assistantFixture struct {
	app      *fiber.App
	got      assistant.Request
	user     string
	resolved *viewcontext.ViewContext
	resolve  error
	threads  *threadRepo
}

func newAssistantFixture() *assistantFixture {
	f := &assistantFixture{threads: &threadRepo{}}
	f.app = fiber.New()
	auth := func(c *fiber.Ctx) error {
		c.Locals(tenantctx.Key, "tenant-1")
		c.Locals(logging.UserIDKey, "user-1")
		c.Locals("session", &models.Session{TenantID: "tenant-1", UserID: "user-1"})
		return c.Next()
	}
	run := func(ctx context.Context, req assistant.Request, onEvent assistant.OnEventFunc) (*assistant.Response, error) {
		f.got = req
		f.user, _ = logging.UserIDFrom(ctx)
		resp := &assistant.Response{Explanation: "They share pricing.", Action: "answered"}
		onEvent(assistant.SSEEventExplanationDelta, assistant.DeltaEventPayload{Delta: "They"})
		onEvent(assistant.SSEEventComplete, resp)
		return resp, nil
	}
	resolve := func(_ context.Context, v *viewcontext.ViewContext) (*viewresolve.Resolved, error) {
		f.resolved = v
		if f.resolve != nil {
			return nil, f.resolve
		}
		return &viewresolve.Resolved{View: v, Summary: "On " + string(v.Screen)}, nil
	}
	handlers.NewAssistantHandler(run, func() bool { return true }, resolve, f.threads, nil, auth).Register(f.app)
	return f
}

func (f *assistantFixture) post(t *testing.T, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("POST", "/api/assistant", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestAssistantTurn_Streams(t *testing.T) {
	f := newAssistantFixture()
	status, body := f.post(t, `{"instruction":"what do these share?","viewContext":{"version":1,"screen":"campaign.ideas",
		"scope":{"kind":"campaign","id":"c1"},"selection":{"kind":"idea","ids":["i1","i2"]}}}`)
	if status != fiber.StatusOK {
		t.Fatalf("status = %d: %s", status, body)
	}
	names, _ := sseEvents(t, strings.NewReader(body))
	if strings.Join(names, ",") != "explanation_delta,complete" {
		t.Fatalf("events = %v", names)
	}
	if f.got.Instruction != "what do these share?" || f.got.View.View.Scope.ID != "c1" || f.got.View.Summary != "On campaign.ideas" {
		t.Fatalf("request = %+v", f.got)
	}
	if f.user != "user-1" {
		t.Fatalf("the turn ran as %q", f.user)
	}
}

func TestAssistantTurn_Refusals(t *testing.T) {
	cases := map[string]struct {
		body    string
		resolve error
		status  int
		message string
	}{
		"no instruction":          {`{"viewContext":{"version":1,"screen":"ideas","scope":{"kind":"workspace"}}}`, nil, 400, ""},
		"no view":                 {`{"instruction":"hi"}`, nil, 400, ""},
		"bad view":                {`{"instruction":"hi","viewContext":{"version":1,"screen":"billing","scope":{"kind":"workspace"}}}`, nil, 400, "viewContext.screen"},
		"scope gone":              {`{"instruction":"hi","viewContext":{"version":1,"screen":"post","scope":{"kind":"post","id":"p9"}}}`, viewresolve.ErrScopeNotFound, 404, "post not found"},
		"filter the list rejects": {`{"instruction":"hi","viewContext":{"version":1,"screen":"ideas","scope":{"kind":"workspace"}}}`, &viewcontext.InvalidError{Field: "filter", Reason: "unknown pile"}, 400, "viewContext.filter"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAssistantFixture()
			f.resolve = tc.resolve
			status, body := f.post(t, tc.body)
			if status != tc.status || !strings.Contains(body, tc.message) {
				t.Fatalf("status = %d, body = %s; want %d with %q", status, body, tc.status, tc.message)
			}
		})
	}
}

func TestAssistantThread(t *testing.T) {
	cases := map[string]struct {
		scope  string
		status int
		kind   models.AssistantScopeKind
		id     string
	}{
		"workspace":         {"workspace", 200, models.AssistantScopeWorkspace, ""},
		"campaign":          {"campaign:c1", 200, models.AssistantScopeCampaign, "c1"},
		"post":              {"post:p1", 200, models.AssistantScopePost, "p1"},
		"workspace with id": {"workspace:x", 400, "", ""},
		"campaign no id":    {"campaign", 400, "", ""},
		"unknown":           {"team:t1", 400, "", ""},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := newAssistantFixture()
			resp, err := f.app.Test(httptest.NewRequest("GET", "/api/assistant/threads/"+tc.scope+"/messages", nil), 5000)
			if err != nil {
				t.Fatal(err)
			}
			if resp.StatusCode != tc.status {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.status)
			}
			if tc.status != 200 {
				return
			}
			var msgs []models.AssistantMessage
			if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil || msgs == nil {
				t.Fatalf("body = %v (%v); want []", msgs, err)
			}
			if f.threads.kind != tc.kind || f.threads.id != tc.id {
				t.Fatalf("read thread %s/%s", f.threads.kind, f.threads.id)
			}
		})
	}
}
