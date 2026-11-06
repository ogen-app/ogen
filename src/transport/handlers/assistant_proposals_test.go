package handlers_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/genkit/flows/assistant"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

type answerFn = func(context.Context, assistant.AnswerRequest, assistant.OnEventFunc) (*assistant.Response, error)

// proposalApp mounts the proposal endpoints behind a stand-in auth that sets
// the tenant and user the real middleware would.
func proposalApp(answer answerFn) *fiber.App {
	app := fiber.New()
	auth := func(c *fiber.Ctx) error {
		c.Locals(tenantctx.Key, "tenant-1")
		c.Locals(logging.UserIDKey, "user-1")
		return c.Next()
	}
	handlers.NewAssistantProposalsHandler(answer, nil, auth).Register(app)
	return app
}

// sseEvents reads "event:" names and their "data:" lines from a stream.
func sseEvents(t *testing.T, body io.Reader) (names, data []string) {
	t.Helper()
	sc := bufio.NewScanner(body)
	for sc.Scan() {
		line := sc.Text()
		if v, ok := strings.CutPrefix(line, "event: "); ok {
			names = append(names, v)
		}
		if v, ok := strings.CutPrefix(line, "data: "); ok {
			data = append(data, v)
		}
	}
	return names, data
}

func TestConfirmProposal_StreamsTheWork(t *testing.T) {
	var got assistant.AnswerRequest
	var user string
	app := proposalApp(func(ctx context.Context, req assistant.AnswerRequest, onEvent assistant.OnEventFunc) (*assistant.Response, error) {
		got = req
		user, _ = logging.UserIDFrom(ctx)
		resp := &assistant.Response{Action: "answered", Explanation: "Done."}
		onEvent("generate_posts_complete", map[string]int{"ideaCount": 10})
		onEvent(assistant.SSEEventProposalResolved, map[string]string{"id": req.ProposalID, "status": "confirmed"})
		onEvent(assistant.SSEEventComplete, resp)
		return resp, nil
	})

	req := httptest.NewRequest("POST", "/api/assistant/proposals/prop-1/confirm", strings.NewReader(`{"optionId":"fill"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req, 5000)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	names, _ := sseEvents(t, resp.Body)
	if strings.Join(names, ",") != "generate_posts_complete,proposal_resolved,complete" {
		t.Fatalf("events = %v", names)
	}
	if got.ProposalID != "prop-1" || got.OptionID != "fill" || !got.Confirm {
		t.Fatalf("request = %+v", got)
	}
	if user != "user-1" {
		t.Fatalf("detached context user = %q, want user-1", user)
	}
}

func TestConfirmProposal_ErrorCodes(t *testing.T) {
	cases := map[string]struct {
		err  error
		code int
	}{
		"not found": {assistant.ErrProposalNotFound, fiber.StatusNotFound},
		"conflict":  {&assistant.ProposalConflictError{Msg: "already answered"}, fiber.StatusConflict},
		"ai error":  {&assistant.AIError{Msg: "busy"}, fiber.StatusBadGateway},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app := proposalApp(func(context.Context, assistant.AnswerRequest, assistant.OnEventFunc) (*assistant.Response, error) {
				return nil, tc.err
			})
			resp, err := app.Test(httptest.NewRequest("POST", "/api/assistant/proposals/prop-1/confirm", nil), 5000)
			if err != nil {
				t.Fatal(err)
			}
			names, data := sseEvents(t, resp.Body)
			if len(names) != 1 || names[0] != "error" {
				t.Fatalf("events = %v", names)
			}
			var payload struct {
				Code int `json:"code"`
			}
			if err := json.Unmarshal([]byte(data[0]), &payload); err != nil || payload.Code != tc.code {
				t.Fatalf("error payload = %s (%v), want code %d", data[0], err, tc.code)
			}
		})
	}
}

func TestDismissProposal(t *testing.T) {
	var got assistant.AnswerRequest
	app := proposalApp(func(_ context.Context, req assistant.AnswerRequest, _ assistant.OnEventFunc) (*assistant.Response, error) {
		got = req
		if req.ProposalID == "gone" {
			return nil, assistant.ErrProposalNotFound
		}
		return &assistant.Response{Action: "answered", Explanation: "OK, I've dropped that."}, nil
	})

	resp, err := app.Test(httptest.NewRequest("POST", "/api/assistant/proposals/prop-1/dismiss", nil), 5000)
	if err != nil {
		t.Fatal(err)
	}
	var body assistant.Response
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != fiber.StatusOK || body.Action != "answered" || got.Confirm {
		t.Fatalf("status = %d, body = %+v, request = %+v, err = %v", resp.StatusCode, body, got, err)
	}

	resp, err = app.Test(httptest.NewRequest("POST", "/api/assistant/proposals/gone/dismiss", nil), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("dismiss of a missing offer = %d, want 404", resp.StatusCode)
	}
}

func TestProposalEndpoints_UnavailableWithoutAnswer(t *testing.T) {
	app := proposalApp(nil)
	for _, path := range []string{"/api/assistant/proposals/p/confirm", "/api/assistant/proposals/p/dismiss"} {
		resp, err := app.Test(httptest.NewRequest("POST", path, nil), 5000)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != fiber.StatusServiceUnavailable {
			t.Fatalf("%s = %d, want 503", path, resp.StatusCode)
		}
	}
}
