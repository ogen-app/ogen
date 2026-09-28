package handlers

import (
	"context"

	"github.com/ogen-app/ogen/src/kernel/background"
)

// backgroundTasks tracks the best-effort work handlers spawn after responding
// (embeddings, alt text, password-reset mail, throttle attribution).
var backgroundTasks background.Group

// DrainBackground waits for handler background tasks to finish, or for ctx to
// end. The server calls it on shutdown, before the job queue stops.
func DrainBackground(ctx context.Context) error {
	return backgroundTasks.Wait(ctx)
}
