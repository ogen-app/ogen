package handlers_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/genkit/flows/campaign_assistant"
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/campaign_actions/state"
)

// The campaign's state reads as the assistant reads it, with its starters;
// an unknown campaign is a 404.
func TestCampaignAssistantState(t *testing.T) {
	app := fiber.New()
	auth := func(c *fiber.Ctx) error {
		c.Locals("session", &models.Session{UserID: "user-1", TenantID: "tenant-1"})
		return c.Next()
	}
	handlers.NewCampaignAssistantStateHandler(func(_ context.Context, id string) (*campaign_assistant.CampaignState, error) {
		if id != "c1" {
			return nil, sql.ErrNoRows
		}
		return &campaign_assistant.CampaignState{
			State:    state.Snapshot{Goal: &state.Goal{Target: 20, Planned: 14, Room: 6}},
			Starters: []campaign_assistant.Starter{{Kind: "draft", Label: "Draft 6 posts", Instruction: "Draft 6 posts to reach the campaign's goal."}},
		}, nil
	}, auth).Register(app)

	resp, err := app.Test(httptest.NewRequest("GET", "/api/campaigns/c1/assistant-state", nil), 5000)
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		State struct {
			Goal struct {
				Room int `json:"room"`
			} `json:"goal"`
		} `json:"state"`
		Starters []map[string]string `json:"starters"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, err = %v", resp.StatusCode, err)
	}
	if body.State.Goal.Room != 6 || len(body.Starters) != 1 || body.Starters[0]["label"] != "Draft 6 posts" {
		t.Fatalf("body = %+v", body)
	}

	resp, err = app.Test(httptest.NewRequest("GET", "/api/campaigns/gone/assistant-state", nil), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusNotFound {
		t.Fatalf("unknown campaign = %d", resp.StatusCode)
	}
}
