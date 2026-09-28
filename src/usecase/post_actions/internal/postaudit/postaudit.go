// Package postaudit holds the audit and eventing plumbing shared by the
// post actions (clone, restore) that the REST handlers and the Post
// Assistant tools both call.
package postaudit

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/eventhub"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
)

// Trigger identifies which entry point initiated a post action; it is
// recorded in the audit payload and decides the version creator role.
const (
	TriggerAPI       = "api"
	TriggerAssistant = "assistant"
)

// Version creator roles. post_versions.creator is a role enum, not a
// user id.
const (
	CreatorUser      = "user"
	CreatorAssistant = "assistant"
)

// CreatorFor maps a trigger to the post_versions.creator role.
func CreatorFor(trigger string) string {
	if trigger == TriggerAssistant {
		return CreatorAssistant
	}
	return CreatorUser
}

// NotFound replaces sql.ErrNoRows with notFound and passes any other
// error through unchanged.
func NotFound(err, notFound error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return notFound
	}
	return err
}

// NewLog builds a post_logs entry with a fresh id and a sanitized,
// size-capped JSON payload.
func NewLog(postID string, event models.PostLogEventType, actor, summary string, payload map[string]any) (*models.PostLog, error) {
	logID, err := models.NewID()
	if err != nil {
		return nil, err
	}
	raw, _ := json.Marshal(payload)
	return &models.PostLog{
		ID:        logID,
		PostID:    postID,
		EventType: event,
		Actor:     actor,
		Summary:   summary,
		Payload:   logs.SanitizeAndCap(string(raw)),
	}, nil
}

// Publish emits a best-effort event on the post's entity topic. A nil
// hub disables eventing; failures are dropped because the DB write has
// already committed. eventType is the dotted bus wire type, distinct
// from the persisted post_logs event names.
func Publish(hub eventhub.Hub, postID, eventType, actor string, payload map[string]any) {
	if hub == nil {
		return
	}
	evID, err := models.NewID()
	if err != nil {
		return
	}
	_ = hub.Publish(context.Background(), eventhub.Event{
		ID:      evID,
		Topic:   "entity:post:" + postID,
		Type:    eventType,
		UserID:  actor,
		Payload: payload,
	})
}
