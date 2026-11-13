package handlers_test

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/transport/handlers"
)

// historyRepo returns a fixed thread.
type historyRepo struct {
	repository.AssistantMessageRepository
	msgs []models.AssistantMessage
}

func (r historyRepo) ListRecent(context.Context, models.AssistantScopeKind, string, int) ([]models.AssistantMessage, error) {
	return r.msgs, nil
}

// offerRepo knows a set of offers by id.
type offerRepo struct {
	repository.AssistantProposalRepository
	rows []models.AssistantProposal
}

func (r offerRepo) ListByIDs(_ context.Context, ids []string) ([]models.AssistantProposal, error) {
	return slices.DeleteFunc(slices.Clone(r.rows), func(p models.AssistantProposal) bool { return !slices.Contains(ids, p.ID) }), nil
}

func (r offerRepo) ListReplacing(_ context.Context, ids []string) ([]models.AssistantProposal, error) {
	return slices.DeleteFunc(slices.Clone(r.rows), func(p models.AssistantProposal) bool { return !slices.Contains(ids, p.Replaces) }), nil
}

// A model turn in the history carries the offers it made or answered as
// they stand now: a stale offer names the fresh one that replaced it, a
// pending one past its expiry reads expired, and a retry names its run.
func TestAssistantThread_Offers(t *testing.T) {
	now := time.Now().UTC()
	later, earlier := now.Add(time.Hour), now.Add(-time.Hour)
	threads := historyRepo{msgs: []models.AssistantMessage{
		{ID: "m1", Role: "user", Content: `{"proposals":[{"id":"o1"}]}`},
		{ID: "m2", Role: "model", Content: `{"explanation":"Offered.","proposals":[{"id":"o1"},{"id":"o2"}]}`},
		{ID: "m3", Role: "user", Content: "Stale: Move 5 posts"},
		{ID: "m4", Role: "model", Content: `{"explanation":"Nothing ran.","proposals":[{"id":"o3"}],"resolved":[{"id":"o1","status":"stale"}]}`},
		{ID: "m5", Role: "model", Content: `{"explanation":"Done in part.","proposals":[{"id":"o4"}]}`},
	}}
	offers := offerRepo{rows: []models.AssistantProposal{
		{ID: "o1", Status: models.ProposalPending, ExpiresAt: later},
		{ID: "o2", Status: models.ProposalPending, ExpiresAt: earlier},
		{ID: "o3", Status: models.ProposalPending, ExpiresAt: later, Replaces: "o1"},
		{ID: "o4", Status: models.ProposalPending, ExpiresAt: later, RetryOf: "o0"},
	}}
	app := fiber.New()
	auth := func(c *fiber.Ctx) error { return c.Next() }
	handlers.NewAssistantHandler(nil, nil, nil, threads, nil, auth).WithProposals(offers).Register(app)

	resp, err := app.Test(httptest.NewRequest("GET", "/api/assistant/threads/workspace/messages", nil), 5000)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []models.AssistantMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil || len(msgs) != 5 {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	if msgs[0].Offers != nil || msgs[2].Offers != nil {
		t.Fatalf("a user turn has offers: %+v, %+v", msgs[0].Offers, msgs[2].Offers)
	}
	m2 := msgs[1].Offers
	if len(m2) != 2 || m2[0].ID != "o1" || m2[0].Status != models.ProposalPending || m2[0].ReplacedBy != "o3" ||
		m2[1].ID != "o2" || m2[1].Status != models.ProposalExpired {
		t.Fatalf("m2 offers = %+v", m2)
	}
	m4 := msgs[3].Offers
	if len(m4) != 2 || m4[0].ID != "o3" || m4[0].Replaces != "o1" || m4[1].ID != "o1" || m4[1].ReplacedBy != "o3" {
		t.Fatalf("m4 offers = %+v", m4)
	}
	if m5 := msgs[4].Offers; len(m5) != 1 || m5[0].RetryOf != "o0" {
		t.Fatalf("m5 offers = %+v", m5)
	}
}

// An offer's replacement shows even when the turn that made it isn't in the
// thread, e.g. because its answer failed to save.
func TestAssistantThread_ReplacementWithoutItsTurn(t *testing.T) {
	now := time.Now().UTC()
	threads := historyRepo{msgs: []models.AssistantMessage{
		{ID: "m1", Role: "model", Content: `{"explanation":"Offered.","proposals":[{"id":"o1"}]}`},
	}}
	offers := offerRepo{rows: []models.AssistantProposal{
		{ID: "o1", Status: models.ProposalPending, ExpiresAt: now.Add(time.Hour)},
		{ID: "o2", Status: models.ProposalPending, ExpiresAt: now.Add(time.Hour), Replaces: "o1", CreatedAt: now.Add(-time.Minute)},
		{ID: "o3", Status: models.ProposalPending, ExpiresAt: now.Add(time.Hour), Replaces: "o1", CreatedAt: now},
	}}
	app := fiber.New()
	handlers.NewAssistantHandler(nil, nil, nil, threads, nil, func(c *fiber.Ctx) error { return c.Next() }).WithProposals(offers).Register(app)

	resp, err := app.Test(httptest.NewRequest("GET", "/api/assistant/threads/workspace/messages", nil), 5000)
	if err != nil {
		t.Fatal(err)
	}
	var msgs []models.AssistantMessage
	if err := json.NewDecoder(resp.Body).Decode(&msgs); err != nil || len(msgs) != 1 {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	if o := msgs[0].Offers; len(o) != 1 || o[0].ID != "o1" || o[0].ReplacedBy != "o3" {
		t.Fatalf("offers = %+v; want o1 replaced by the latest, o3", o)
	}
}
