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

	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

type answerFn = func(context.Context, campaign_assistant.AnswerProposalRequest, campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error)

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
	var got campaign_assistant.AnswerProposalRequest
	var user string
	app := proposalApp(func(ctx context.Context, req campaign_assistant.AnswerProposalRequest, onEvent campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error) {
		got = req
		user, _ = logging.UserIDFrom(ctx)
		resp := &campaign_assistant.CampaignAssistantResponse{Action: "posts_generated", Explanation: "Done."}
		onEvent(campaign_assistant.SSEEventGeneratePostsComplete, map[string]int{"postCount": 10})
		onEvent(campaign_assistant.SSEEventProposalResolved, map[string]string{"id": req.ProposalID, "status": "confirmed"})
		onEvent(campaign_assistant.SSEEventComplete, resp)
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
		"not found": {campaign_assistant.ErrProposalNotFound, fiber.StatusNotFound},
		"conflict":  {&campaign_assistant.ProposalConflictError{Msg: "already answered"}, fiber.StatusConflict},
		"ai error":  {&campaign_assistant.AIError{Msg: "busy"}, fiber.StatusBadGateway},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			app := proposalApp(func(context.Context, campaign_assistant.AnswerProposalRequest, campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error) {
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
			var payload campaign_assistant.ErrorEventPayload
			if err := json.Unmarshal([]byte(data[0]), &payload); err != nil || payload.Code != tc.code {
				t.Fatalf("error payload = %s (%v), want code %d", data[0], err, tc.code)
			}
		})
	}
}

func TestDismissProposal(t *testing.T) {
	var got campaign_assistant.AnswerProposalRequest
	app := proposalApp(func(_ context.Context, req campaign_assistant.AnswerProposalRequest, _ campaign_assistant.OnEventFunc) (*campaign_assistant.CampaignAssistantResponse, error) {
		got = req
		if req.ProposalID == "gone" {
			return nil, campaign_assistant.ErrProposalNotFound
		}
		return &campaign_assistant.CampaignAssistantResponse{Action: "answered", Explanation: "OK, I've dropped that."}, nil
	})

	resp, err := app.Test(httptest.NewRequest("POST", "/api/assistant/proposals/prop-1/dismiss", nil), 5000)
	if err != nil {
		t.Fatal(err)
	}
	var body campaign_assistant.CampaignAssistantResponse
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
