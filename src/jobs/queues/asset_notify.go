package queues

import (
	"context"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/usecase/notify"
)

// assetCreatorLookup resolves an asset's creator — the recipient for its
// ingest-completion notification. Both the URL and PDF worker's asset deps
// satisfy it (CON-242).
type assetCreatorLookup interface {
	CreatorOf(ctx context.Context, id string) (string, error)
}

// notifyAssetStatus drops a notification to an asset's creator when ingestion
// reaches a TERMINAL status (CON-242). Non-terminal statuses (processing) are
// ignored, so one call inside each worker's setStatus covers every terminal
// path with no per-call-site wiring. Best-effort: a nil notifier or a
// creator-lookup miss is a silent no-op — ingestion never depends on it. The
// ctx is already tenant-scoped by the worker, so the row lands in the right
// tenant and the creator lookup is correctly isolated.
//
// kind is the asset's models.AssetType*: a URL asset (CON-222) emits the
// distinct url_asset.crawled/url_asset.failed types the feed renders with link
// wording (CON-285 FR10); every other kind keeps asset.ready/asset.ingest_failed.
func notifyAssetStatus(ctx context.Context, n *notify.Service, creators assetCreatorLookup, assetID, status, label, kind string) {
	if n == nil {
		return
	}
	isURL := kind == models.AssetTypeURL
	var (
		level            models.NotificationLevel
		typ, title, body string
	)
	switch status {
	case models.AssetStatusReady:
		level = models.NotificationLevelSuccess
		if isURL {
			typ, title, body = "url_asset.crawled", "Link ready", "We finished reading your "+label+"."
		} else {
			typ, title, body = "asset.ready", "Asset ready", "Your "+label+" finished processing."
		}
	case models.AssetStatusPartial:
		level = models.NotificationLevelWarning
		if isURL {
			typ, title, body = "url_asset.crawled", "Link ready", "We read your "+label+", with some parts skipped."
		} else {
			typ, title, body = "asset.ready", "Asset ready", "Your "+label+" processed, with some parts skipped."
		}
	case models.AssetStatusFailed:
		level = models.NotificationLevelError
		if isURL {
			typ, title, body = "url_asset.failed", "Couldn't read link", "We couldn't read your "+label+"."
		} else {
			typ, title, body = "asset.ingest_failed", "Asset processing failed", "We couldn't process your "+label+"."
		}
	default:
		return // non-terminal (processing/pending): nothing to announce
	}
	createdBy, err := creators.CreatorOf(ctx, assetID)
	if err != nil || createdBy == "" {
		return
	}
	_ = n.Emit(ctx, createdBy, notify.Spec{
		Level:      level,
		Type:       typ,
		Title:      title,
		Body:       body,
		EntityType: "asset",
		EntityID:   assetID,
		ActionURL:  "/assets/" + assetID,
		Data:       map[string]any{"kind": kind},
		DedupeKey:  typ + ":" + assetID,
	})
}
