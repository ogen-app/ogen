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
	"github.com/ogen-app/ogen/src/transport/handlers"
	"github.com/ogen-app/ogen/src/usecase/assistantundo"
)

func undoApp(undo func(context.Context, string, string) (*models.AssistantAction, error)) *fiber.App {
	app := fiber.New()
	auth := func(c *fiber.Ctx) error {
		c.Locals("session", &models.Session{UserID: "user-1", TenantID: "tenant-1"})
		return c.Next()
	}
	handlers.NewAssistantActionsHandler(undo, nil, auth).Register(app)
	return app
}

func TestUndoAssistantChange(t *testing.T) {
	var gotID, gotBy string
	app := undoApp(func(_ context.Context, id, by string) (*models.AssistantAction, error) {
		gotID, gotBy = id, by
		switch id {
		case "gone":
			return nil, assistantundo.ErrNotFound
		case "twice":
			return nil, assistantundo.ErrAlreadyUndone
		case "edited":
			return nil, assistantundo.ErrChangedSince
		case "retagged":
			return nil, &assistantundo.ChangedSinceError{Changed: 4, Of: 14}
		}
		return &models.AssistantAction{ID: id, Kind: models.AssistantActionCampaignDates, ScopeKind: models.AssistantScopeCampaign, EntityID: "c1"}, nil
	})

	resp, err := app.Test(httptest.NewRequest("POST", "/api/assistant/actions/act-1/undo", nil), 5000)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil || resp.StatusCode != fiber.StatusOK {
		t.Fatalf("status = %d, err = %v", resp.StatusCode, err)
	}
	if body["undone"] != true || body["kind"] != "campaign_dates" || body["entityId"] != "c1" || gotID != "act-1" || gotBy != "user-1" {
		t.Fatalf("body = %v, id = %q, by = %q", body, gotID, gotBy)
	}

	for id, code := range map[string]int{"gone": fiber.StatusNotFound, "twice": fiber.StatusConflict, "edited": fiber.StatusConflict} {
		resp, err := app.Test(httptest.NewRequest("POST", "/api/assistant/actions/"+id+"/undo", nil), 5000)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != code {
			t.Fatalf("undo %s = %d, want %d", id, resp.StatusCode, code)
		}
	}

	// A conflict says why, and for several entities how many moved.
	for id, want := range map[string]string{
		"twice":    `"reason":"already_undone"`,
		"edited":   `"reason":"changed_since"`,
		"retagged": `"reason":"changed_since","changed":4,"of":14`,
	} {
		resp, err := app.Test(httptest.NewRequest("POST", "/api/assistant/actions/"+id+"/undo", nil), 5000)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		if resp.StatusCode != fiber.StatusConflict || !strings.Contains(string(b), want) {
			t.Fatalf("undo %s = %d %s; want %s", id, resp.StatusCode, b, want)
		}
	}
}

func TestUndoAssistantChange_Unavailable(t *testing.T) {
	resp, err := undoApp(nil).Test(httptest.NewRequest("POST", "/api/assistant/actions/a/undo", nil), 5000)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != fiber.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
}
