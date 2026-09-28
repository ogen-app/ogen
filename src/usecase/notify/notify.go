// Package notify is the write-side of the notification center: a tiny
// service producers call to drop a persistent, per-user notification and wake
// any live SSE stream. It is the single choke point between domain events
// (publish outcomes, connection expiry, finished jobs) and the inbox, so
// producers never touch the repository or the event bus directly.
package notify

import (
	"context"
	"expvar"
	"log/slog"
	"time"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/kernel/logging"
)

// StreamTopic is the eventhub topic the notification SSE stream subscribes to.
// Notifications are published per-user (Event.UserID), so the hub's
// authorization filter guarantees a notification only ever reaches its own
// recipient's connections.
const StreamTopic = "notification"

// Counters exposed at /debug/vars.
var (
	Emitted           = expvar.NewInt("ogen_notifications_emitted")
	Cleaned           = expvar.NewInt("ogen_notifications_cleaned")
	StreamConnections = expvar.NewInt("ogen_notifications_stream_connections")
	StreamReplayed    = expvar.NewInt("ogen_notifications_stream_replayed")
)

// Spec is a producer's description of one notification. Level defaults to info
// when empty/unknown. Everything but Type/Title is optional.
type Spec struct {
	Level      models.NotificationLevel
	Type       string
	Title      string
	Body       string
	EntityType string
	EntityID   string
	ActionURL  string
	Data       map[string]any
	// DedupeKey, when set, collapses repeats for the same user while the prior
	// one is still unread (partial unique index). Leave empty to always insert.
	DedupeKey string
	// ExpiresAt fades the notification out of the inbox at a set time; nil never
	// expires (retention still reaps it once read).
	ExpiresAt *time.Time
}

// Service persists notifications and wakes live streams. A nil *Service is a
// valid no-op, so a producer can hold a possibly-unwired notifier without
// guarding every call site (mirrors the activity Recorder).
type Service struct {
	repo repository.NotificationRepository
	hub  eventhub.Hub
}

func New(repo repository.NotificationRepository, hub eventhub.Hub) *Service {
	return &Service{repo: repo, hub: hub}
}

// Emit writes one notification for userID under the ctx's tenant, then publishes
// it to the user's live stream. A dedupe collision is a silent no-op. The insert
// is durable; a hub-publish failure is logged, not returned (the client replays
// it on reconnect). The ctx MUST carry the recipient's tenant (request auth or
// tenantctx.With in a job) — the row's tenant_id is stamped from it.
func (s *Service) Emit(ctx context.Context, userID string, spec Spec) error {
	if s == nil || s.repo == nil || userID == "" {
		return nil
	}
	level := spec.Level
	if !level.Valid() {
		level = models.NotificationLevelInfo
	}
	id, err := models.NewID()
	if err != nil {
		return err
	}
	n := &models.Notification{
		ID:         id,
		UserID:     userID,
		Level:      level,
		Type:       spec.Type,
		Title:      spec.Title,
		Body:       spec.Body,
		EntityType: spec.EntityType,
		EntityID:   spec.EntityID,
		ActionURL:  spec.ActionURL,
		Data:       models.JSONMap(spec.Data),
		DedupeKey:  spec.DedupeKey,
		ExpiresAt:  spec.ExpiresAt,
	}
	inserted, err := s.repo.Insert(ctx, n)
	if err != nil {
		return err
	}
	if !inserted {
		return nil // collapsed duplicate
	}
	Emitted.Add(1)
	s.publish(ctx, n)
	return nil
}

// EmitToUsers fans one spec out to several recipients (e.g. all workspace
// owners), one row each. It never aborts on a single recipient's failure — the
// first error is returned after attempting them all.
func (s *Service) EmitToUsers(ctx context.Context, userIDs []string, spec Spec) error {
	if s == nil || s.repo == nil {
		return nil
	}
	var firstErr error
	for _, uid := range userIDs {
		if err := s.Emit(ctx, uid, spec); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// publish wakes the recipient's live SSE stream. Best-effort: the row is already
// durable, so a publish failure only means the client sees it on its next
// reconnect replay rather than instantly.
func (s *Service) publish(ctx context.Context, n *models.Notification) {
	if s.hub == nil {
		return
	}
	// Topic is the shared StreamTopic; per-user delivery is enforced by the hub's
	// UserID authorization filter. Publish derives TenantID from the ctx.
	if err := s.hub.Publish(ctx, eventhub.Event{
		Topic:   StreamTopic,
		Type:    StreamTopic,
		UserID:  n.UserID,
		Payload: n,
	}); err != nil {
		slog.WarnContext(ctx, "notification stream publish failed (best-effort)",
			logging.AttrComponent, "notify", logging.AttrError, err)
	}
}
