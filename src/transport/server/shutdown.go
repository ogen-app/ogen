package server

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/ogen-app/ogen/src/transport/handlers"
)

// shutdownStage orders the API server's shutdown hooks. Fiber runs OnShutdown
// hooks in registration order, so the stages are registered lowest first and,
// within a stage, in the order they were added. The order is load-bearing:
// each stage stops something the stages before it may still be using.
type shutdownStage int

const (
	// stageDrainHandlers waits for handler background tasks, which may enqueue
	// jobs or call gRPC clients that later stages shut down.
	stageDrainHandlers shutdownStage = iota
	// stageIntegrations stops the Zernio sync worker and closes the video gRPC
	// client, which only request handlers use.
	stageIntegrations
	// stageJobs stops River, draining the jobs that are still running.
	stageJobs
	// stageJobClients closes the gRPC clients River jobs call (pdf, documents,
	// audio, image). It must follow stageJobs: a draining ingest job would
	// otherwise have its in-flight RPC aborted by the connection close.
	stageJobClients
	// stageRecorders drains the usage and activity recorders. It must come
	// last: request handlers, the Zernio worker and River jobs all call
	// Record(), and a recorder that drains while they run silently drops the
	// events they queue afterwards.
	stageRecorders

	numShutdownStages
)

// shutdownPlan collects shutdown hooks by stage while the server is wired and
// registers them on the app in stage order once wiring is complete.
type shutdownPlan struct {
	stages [numShutdownStages][]func() error
}

// add schedules fn to run in the given stage, after the hooks already added to
// that stage.
func (p *shutdownPlan) add(stage shutdownStage, fn func() error) {
	p.stages[stage] = append(p.stages[stage], fn)
}

// ordered returns every hook in the order it must run.
func (p *shutdownPlan) ordered() []func() error {
	var out []func() error
	for _, hooks := range p.stages {
		out = append(out, hooks...)
	}
	return out
}

// register installs the hooks on app in their required order.
func (p *shutdownPlan) register(app *fiber.App) {
	for _, fn := range p.ordered() {
		app.Hooks().OnShutdown(fn)
	}
}

// drainHandlerBackground waits up to 10s for handler background tasks.
func drainHandlerBackground() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return handlers.DrainBackground(ctx)
}

// closeWithin returns a hook that calls closeFn with a context bounded by d.
func closeWithin(d time.Duration, closeFn func(context.Context) error) func() error {
	return func() error {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		defer cancel()
		return closeFn(ctx)
	}
}
