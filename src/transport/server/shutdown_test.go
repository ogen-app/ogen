package server

import (
	"slices"
	"testing"

	"github.com/gofiber/fiber/v2"
)

func TestShutdownPlanRunsStagesInOrder(t *testing.T) {
	var ran []string
	hook := func(name string) func() error {
		return func() error {
			ran = append(ran, name)
			return nil
		}
	}

	// Added out of stage order, the way server.New's phases add them.
	var plan shutdownPlan
	plan.add(stageDrainHandlers, hook("drain"))
	plan.add(stageRecorders, hook("usage"))
	plan.add(stageRecorders, hook("activity"))
	plan.add(stageIntegrations, hook("zernio"))
	plan.add(stageIntegrations, hook("pdf"))
	plan.add(stageJobClients, hook("audio"))
	plan.add(stageJobClients, hook("image"))
	plan.add(stageJobs, hook("river"))

	app := fiber.New()
	plan.register(app)
	// Fiber runs the shutdown hooks even when the app never listened.
	_ = app.Shutdown()

	want := []string{"drain", "zernio", "pdf", "river", "audio", "image", "usage", "activity"}
	if !slices.Equal(ran, want) {
		t.Fatalf("shutdown order = %v, want %v", ran, want)
	}
}

func TestShutdownPlanEmpty(t *testing.T) {
	var plan shutdownPlan
	if got := plan.ordered(); len(got) != 0 {
		t.Fatalf("ordered() on an empty plan = %d hooks, want 0", len(got))
	}
}
