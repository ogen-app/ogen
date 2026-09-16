package queues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"time"

	"github.com/riverqueue/river"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/domain/platforms"
	"github.com/ogen-app/ogen/src/infra/publishers/zernio"
	"github.com/ogen-app/ogen/src/infra/vendors"
	"github.com/ogen-app/ogen/src/jobs"
	"github.com/ogen-app/ogen/src/kernel/activity"
	"github.com/ogen-app/ogen/src/kernel/logging"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
	"github.com/ogen-app/ogen/src/usecase/accountselect"
	"github.com/ogen-app/ogen/src/usecase/notify"
	"github.com/ogen-app/ogen/src/usecase/post_actions/logs"
	"github.com/ogen-app/ogen/src/usecase/settings"
)

// SubmitPostToZernioQueue is the River queue name (CON-69 §3).
const SubmitPostToZernioQueue = "submit_post_to_zernio"

// SubmitPostTask carries the Ogen post id; the worker re-loads the
// row so we never persist stale field copies in the queue payload.
type SubmitPostTask struct {
	PostID string `json:"post_id"`
}

// Kind implements river.JobArgs.
func (SubmitPostTask) Kind() string { return SubmitPostToZernioQueue }

// InsertOpts sets per-kind defaults: 5 total attempts (4 retries) per
// CON-69 §6. Retry backoff is River's default exponential-with-jitter (the
// PRD notes exponential is preferable to backlite's fixed 30s). The
// per-attempt timeout lives on the worker (submitPostWorker.Timeout).
func (SubmitPostTask) InsertOpts() river.InsertOpts {
	return river.InsertOpts{MaxAttempts: 5}
}

// SubmitPostProcessor is the River worker for submit_post_to_zernio. It
// implements river.Worker directly; Process is the test seam Work delegates to.
type SubmitPostProcessor struct {
	river.WorkerDefaults[SubmitPostTask]
	Deps ZernioDeps
	// Tenants gates publishing on the owning tenant's lifecycle status (CON-190):
	// a suspended/deleted tenant's scheduled posts are not published. Nil = no gate.
	Tenants TenantStatusReader
	// Notifier drops a "post failed to publish" notification to the post's
	// creator on a terminal submit failure (CON-242). Nil is a no-op.
	Notifier *notify.Service
	// PollLeadTime is how far in advance of scheduled_at we begin
	// polling. Defaults to 30s when zero.
	PollLeadTime time.Duration
}

// Work is the River entrypoint; it delegates to Process.
func (p *SubmitPostProcessor) Work(ctx context.Context, job *river.Job[SubmitPostTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// CON-97: background jobs span tenants (interim until per-tenant, PR4).
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *SubmitPostProcessor) Timeout(*river.Job[SubmitPostTask]) time.Duration {
	return 30 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &SubmitPostProcessor{Deps: d.Zernio, Tenants: d.Tenants, Notifier: d.Notifier})
	})
}

// Process runs one submit attempt. Returns a non-nil error to ask
// River to retry; returns nil for both success and terminal
// failure (Post moved to Failed inside this method when the failure
// is terminal).
func (p *SubmitPostProcessor) Process(ctx context.Context, task SubmitPostTask) error {
	post, err := p.Deps.PostRepo.GetByID(ctx, task.PostID)
	if err != nil {
		return fmt.Errorf("submit: load post %s: %w", task.PostID, err)
	}
	// Scope the rest of the job to the owning tenant (CON-97 PR4).
	ctx = tenantctx.With(ctx, post.TenantID)
	if post.Status != models.PostStatusScheduled {
		// User cancelled or reconciliation moved this post; abort
		// quietly. The poll task does the same check.
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"submit aborted: post no longer Scheduled", `{"reason":"status_changed"}`)
		return nil
	}
	// Don't publish for a suspended/deleted tenant (CON-190). The post stays
	// Scheduled, so it resumes if the tenant is reactivated (or the reconcile
	// sweep — also tenant-gated — leaves it alone).
	if active, aerr := tenantIsActive(ctx, p.Tenants, post.TenantID); aerr != nil {
		return fmt.Errorf("submit: tenant status %s: %w", post.TenantID, aerr)
	} else if !active {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"submit aborted: tenant not active", `{"reason":"tenant_not_active"}`)
		return nil
	}
	// Idempotency / manual retry path (CON-69 §10):
	//   - On a fresh submit, PublisherPostID is empty and we POST /posts.
	//   - On a manual retry of a previously-failed Post (the user
	//     moved Failed→ReadyForPublish, then ReadyForPublish→Scheduled),
	//     PublisherPostID is still set from the prior attempt. Calling
	//     Zernio's /posts/:id/retry endpoint reuses the same job
	//     identity so we don't create a duplicate Zernio post and
	//     can keep polling against the same id.
	if post.PublisherPostID != "" {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioRetry, post.Status, post.Status,
			"calling Zernio POST /posts/:id/retry", `{"publisher_post_id":"`+post.PublisherPostID+`"}`)
		job, retryErr := p.Deps.Client.Retry(ctx, post.PublisherPostID)
		if retryErr != nil {
			if zernio.IsTerminalAPIError(retryErr) {
				return p.terminal(ctx, post, "zernio_retry_rejected", retryErr.Error())
			}
			appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskRetried, post.Status, post.Status,
				"transient Zernio retry error; River will retry", `{"error":"`+retryErr.Error()+`"}`)
			return retryErr
		}
		return p.persistSuccess(ctx, post, job, "")
	}

	platform := post.Platform
	if platform == nil {
		return p.terminal(ctx, post, "missing_platform", "post has no platform set")
	}

	supported := zernio.LookupSupportedBySqid(platform.ID)
	if supported == nil {
		return p.terminal(ctx, post, "unsupported_platform",
			fmt.Sprintf("platform %s (%s) is not Zernio-supported", platform.Name, platform.ID))
	}

	// CON-150: resolve which same-platform account this post publishes to.
	// An explicit selection (post.SocialAccountID) is validated and used
	// verbatim; otherwise auto-select when the platform has exactly one
	// active account, and require an explicit choice when it has more.
	if p.Deps.ProfileID == nil {
		return p.terminal(ctx, post, "integration_disabled", "Zernio integration is not configured")
	}
	profileID, perr := p.Deps.ProfileID(ctx)
	if perr != nil || profileID == "" {
		return p.terminal(ctx, post, "no_profile", "Zernio profile id not yet bootstrapped")
	}
	accountID, aerr := p.resolveAccountID(ctx, post, profileID, supported.ZernioID)
	if accountID == "" {
		// resolveAccountID has either written a terminal PostLog (aerr == nil,
		// so this returns nil and River does not retry) or hit a transient
		// error River should retry (aerr != nil). An account id is never "".
		return aerr
	}

	when := time.Now().UTC().Add(time.Minute)
	if post.ScheduledAt != nil {
		when = post.ScheduledAt.UTC()
	}
	// The instant (ScheduledFor) is the source of truth and stays UTC;
	// the workspace timezone (CON-78) is echoed so Zernio renders the
	// schedule in the operator's zone. Defaults to UTC when unset.
	_, tzName := settings.WorkspaceTimezone(ctx, p.Deps.SettingRepo)

	// CON-122/CON-284: upload the post's attachments to Zernio and reference
	// them as mediaItems. A thread carries its media inside per-segment
	// threadItems (built below); an ordinary post carries a flat top-level
	// mediaItems array. Either upload failure is transient (network / Zernio
	// media endpoint) so River retries; media isn't lost because the post
	// stays Scheduled.
	variant := zernio.PlatformVariant{Platform: supported.ZernioID, AccountID: accountID}
	var mediaItems []map[string]any
	if post.IsThread() {
		threadItems, tErr := p.buildThreadItems(ctx, post)
		if tErr != nil {
			jobs.ZernioSubmitRetried.Add(1)
			appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskRetried, post.Status, post.Status,
				"transient error uploading thread media to Zernio; River will retry", `{"error":"`+tErr.Error()+`"}`)
			return tErr
		}
		variant.PlatformSpecificData = &zernio.PlatformSpecificData{ThreadItems: threadItems}
	} else {
		items, mediaErr := p.buildMediaItems(ctx, post)
		if mediaErr != nil {
			jobs.ZernioSubmitRetried.Add(1)
			appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskRetried, post.Status, post.Status,
				"transient error uploading media to Zernio; River will retry", `{"error":"`+mediaErr.Error()+`"}`)
			return mediaErr
		}
		mediaItems = items
	}

	// CON-284 R2: post.Content is the full thread body; Zernio's top-level content
	// must be just the root message. Derive it from the segments, falling back to
	// the body for a non-thread post (or the defensive empty-segment case).
	topContent := post.Content
	if post.IsThread() && len(post.ThreadSegments) > 0 {
		topContent = post.ThreadSegments.RootContent()
	}
	// CON-126: none of the networks render Markdown — Zernio publishes the content
	// string verbatim — so flatten it to the plain text a caption actually shows
	// before it leaves Ogen, otherwise `**bold**` and `[text](url)` land literally
	// on X/Threads. The editor's Markdown source (posts.content) is untouched; only
	// this outbound copy is flattened. Thread segments are flattened the same way in
	// buildThreadItems, so every message that ships is plain text.
	topContent = platforms.FlattenSocialText(topContent)

	req := zernio.SubmitRequest{
		// For a thread, top-level Content must be the ROOT segment. As of CON-284
		// R2 post.Content is the full delimited thread body (the canonical draft),
		// so we take the root from the derived segments (topContent above), then
		// flatten it — keeping the CON-129 dedupe-recovery invariant intact
		// (recovery matches on req.Content, the flattened root; FindByContent
		// flattens nothing itself, so both sides agree). The chain rides in
		// Platforms[0].PlatformSpecificData.ThreadItems and top-level MediaItems
		// stays empty. Zernio publishes from threadItems when present; confirm
		// against docs.zernio.com during rollout that it does NOT also post the
		// top-level content (if it does, blank it here).
		Content:      topContent,
		Platforms:    []zernio.PlatformVariant{variant},
		ScheduledFor: when,
		Timezone:     tzName,
		MediaItems:   mediaItems,
		// CON-148 §6.6: pass the title through for platforms that need it
		// explicitly (YouTube video). omitempty drops it for the common case.
		Title: post.Title,
	}

	appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioSubmit, post.Status, post.Status,
		"calling Zernio POST /posts", logs.MarshalCapped(req))

	apiStart := time.Now()
	job, submitErr := p.Deps.Client.Submit(ctx, req)
	jobs.ObserveZernioCall(time.Since(apiStart))
	if submitErr != nil {
		// 24h dedupe recovery (CON-129). Search the whole dedupe window across all
		// statuses, matching on the content we actually submitted — req.Content is
		// the flattened body (CON-126), and FindByContent matches it verbatim, so
		// both sides compare the same string.
		if errors.Is(submitErr, zernio.ErrDuplicateContent) {
			recovered, ferr := p.Deps.Client.FindByContent(ctx, req.Content, 24*time.Hour)
			switch {
			case ferr != nil:
				// Couldn't query Zernio to recover — transient; retry, which 409s
				// again and re-attempts recovery rather than dead-ending the post.
				jobs.ZernioSubmitRetried.Add(1)
				appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskRetried, post.Status, post.Status,
					"transient error locating dedupe match; River will retry", `{"error":"`+ferr.Error()+`"}`)
				return ferr
			case recovered != nil && !recovered.Status.IsTerminal():
				// Still-pending earlier job (almost always this post's own prior
				// attempt): adopt it and keep polling. Count it like the main
				// success path (persistSuccess doesn't, so this is exactly once).
				jobs.ZernioSubmitSucceeded.Add(1)
				appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioSubmit, post.Status, post.Status,
					"recovered pending Zernio job after 409 dedupe", logs.MarshalCapped(recovered))
				return p.persistSuccess(ctx, post, recovered, accountID)
			case recovered != nil:
				// Match is terminal (published/failed/partial): the content already
				// ran on Zernio in this window. Don't adopt a finished job — fail
				// with the cause named so the user can act.
				jobs.ZernioSubmitFailed.Add(1)
				return p.terminal(ctx, post, "zernio_duplicate_content",
					fmt.Sprintf("identical content was already %s on Zernio within the last 24h (job %s); edit the content to submit again",
						recovered.Status, recovered.ID))
			default:
				// Duplicate per Zernio, but no matching job located (content
				// normalisation mismatch, or beyond the search window). Name it.
				jobs.ZernioSubmitFailed.Add(1)
				return p.terminal(ctx, post, "zernio_duplicate_content",
					"Zernio rejected this as duplicate content submitted within the last 24h; edit the content or wait for the dedupe window to pass")
			}
		}
		if zernio.IsTerminalAPIError(submitErr) {
			jobs.ZernioSubmitFailed.Add(1)
			return p.terminal(ctx, post, "zernio_rejected", submitErr.Error())
		}
		// Transient — let River retry per InsertOpts.
		jobs.ZernioSubmitRetried.Add(1)
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskRetried, post.Status, post.Status,
			"transient Zernio error; River will retry", `{"error":"`+submitErr.Error()+`"}`)
		return submitErr
	}

	jobs.ZernioSubmitSucceeded.Add(1)
	return p.persistSuccess(ctx, post, job, accountID)
}

// resolveAccountID picks the Zernio accountId this post publishes to
// (CON-150). It returns a non-empty id on success. On failure it returns
// "" plus either nil — a terminal PostLog was written and River must not
// retry — or a transient error River should retry. That distinction rides
// the error value, mirroring terminal()'s "always nil" contract, so the
// caller can branch on the empty id alone.
func (p *SubmitPostProcessor) resolveAccountID(ctx context.Context, post *models.Post, profileID, zernioPlatform string) (string, error) {
	res, err := accountselect.Resolve(ctx, p.Deps.SocialAccountRepo, profileID, post, zernioPlatform)
	if err != nil {
		// A repository failure is transient — return it (retryable), never terminal.
		return "", fmt.Errorf("submit: resolve account: %w", err)
	}
	// The worker is the authoritative backstop: it resolves the account to
	// publish with, or terminal-fails the job for every rule violation.
	switch res.Outcome {
	case accountselect.Resolved:
		return res.AccountID, nil
	case accountselect.Unavailable:
		return "", p.terminal(ctx, post, "account_unavailable",
			fmt.Sprintf("selected account %s is not connected", post.SocialAccountID))
	case accountselect.PlatformMismatch:
		return "", p.terminal(ctx, post, "account_platform_mismatch",
			fmt.Sprintf("selected account %s is a %s account, not %s", res.Account.ID, res.Account.Platform, zernioPlatform))
	case accountselect.NoAccount:
		return "", p.terminal(ctx, post, "no_account_connected",
			fmt.Sprintf("no active Zernio account connected for platform %s", zernioPlatform))
	default: // Ambiguous
		return "", p.terminal(ctx, post, "account_selection_required",
			fmt.Sprintf("%d %s accounts connected; select one before publishing", len(res.Candidates), zernioPlatform))
	}
}

func (p *SubmitPostProcessor) persistSuccess(ctx context.Context, post *models.Post, job *zernio.Job, accountID string) error {
	post.Publisher = zernio.PublisherID
	post.PublisherPostID = job.ID
	post.PublisherStatus = string(job.Status)
	if err := p.Deps.PostRepo.Update(ctx, post); err != nil {
		return fmt.Errorf("submit: persist publisher_post_id: %w", err)
	}
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioSubmit, post.Status, post.Status,
		"Zernio submit succeeded; polling scheduled", logs.MarshalCapped(map[string]any{
			"publisher":         zernio.PublisherID,
			"publisher_post_id": job.ID,
			"publisher_status":  job.Status,
		}))
	p.recordPublish(ctx, post, accountID)
	p.Deps.ActivityRecorder.Record(ctx, activity.CategoryPublish, "publish_submitted",
		activity.WithEntity("post", post.ID),
		activity.WithSource(activity.SourceJob),
		activity.WithStatus("submitted"),
	)
	p.enqueuePoll(ctx, post)
	return nil
}

// buildMediaItems uploads each of the post's attachments to Zernio (presign →
// PUT bytes → publicUrl) and returns the mediaItems array for the submit
// request, in position order, carrying altText (CON-122). Nil deps
// (Storage/PostAttachmentRepo) or no attachments ⇒ nil (a text-only post).
// Attachments whose MIME type Zernio doesn't accept are skipped, not fatal.
func (p *SubmitPostProcessor) buildMediaItems(ctx context.Context, post *models.Post) ([]map[string]any, error) {
	if p.Deps.Storage == nil || p.Deps.PostAttachmentRepo == nil {
		return nil, nil
	}
	atts, err := p.Deps.PostAttachmentRepo.ListByPostID(ctx, post.ID)
	if err != nil {
		return nil, fmt.Errorf("submit: list attachments: %w", err)
	}
	if len(atts) == 0 {
		return nil, nil
	}
	items := make([]map[string]any, 0, len(atts))
	for i := range atts {
		att := atts[i]
		mediaType := zernio.MediaType(att.MimeType)
		if mediaType == "" {
			slog.WarnContext(ctx, "skipping attachment: unsupported media type for Zernio",
				logging.AttrComponent, "jobs.submit", "post_id", post.ID, "attachment_id", att.ID, "mime", att.MimeType)
			continue
		}
		item, err := p.uploadMedia(ctx, &att, mediaType)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

// buildThreadItems builds the ordered per-segment payload for a native thread
// (CON-284): one ThreadItem per thread_segment (item 0 = root), each carrying
// its own text and its own media. Attachments are grouped by segment_index and
// uploaded to Zernio (presign → PUT) exactly like buildMediaItems, preserving
// position order within a segment. Nil storage/repo ⇒ a text-only thread.
//
// CON-284 R2: segment_index is optional — a NULL index means the attachment
// belongs to the root message (segment 0), the whole-post default of the
// delimited-body flow. Only a non-NULL, out-of-range index can't be placed, so it
// is skipped with a warning (defence in depth; the gate range-checks it first).
func (p *SubmitPostProcessor) buildThreadItems(ctx context.Context, post *models.Post) ([]zernio.ThreadItem, error) {
	items := make([]zernio.ThreadItem, len(post.ThreadSegments))
	for i := range post.ThreadSegments {
		// CON-126: flatten Markdown per segment so each message publishes as plain
		// text (Zernio ships the content verbatim). VisibleLen counts this same
		// flattened form, so a segment that passed the per-message gate fits here.
		items[i] = zernio.ThreadItem{Content: platforms.FlattenSocialText(post.ThreadSegments[i].Content)}
	}
	if p.Deps.Storage == nil || p.Deps.PostAttachmentRepo == nil {
		return items, nil
	}
	atts, err := p.Deps.PostAttachmentRepo.ListByPostID(ctx, post.ID)
	if err != nil {
		return nil, fmt.Errorf("submit: list attachments: %w", err)
	}
	for i := range atts {
		att := atts[i]
		seg := 0 // NULL segment_index → root message (segment 0)
		if att.SegmentIndex != nil {
			seg = *att.SegmentIndex
		}
		if seg < 0 || seg >= len(items) {
			slog.WarnContext(ctx, "skipping thread attachment with out-of-range segment_index",
				logging.AttrComponent, "jobs.submit", "post_id", post.ID, "attachment_id", att.ID)
			continue
		}
		mediaType := zernio.MediaType(att.MimeType)
		if mediaType == "" {
			slog.WarnContext(ctx, "skipping attachment: unsupported media type for Zernio",
				logging.AttrComponent, "jobs.submit", "post_id", post.ID, "attachment_id", att.ID, "mime", att.MimeType)
			continue
		}
		item, err := p.uploadMedia(ctx, &att, mediaType)
		if err != nil {
			return nil, err
		}
		items[seg].MediaItems = append(items[seg].MediaItems, item)
	}
	return items, nil
}

// uploadMedia streams one attachment from our storage to Zernio (presign + PUT)
// and returns its mediaItems descriptor {url, type, altText?}.
func (p *SubmitPostProcessor) uploadMedia(ctx context.Context, att *models.PostAttachment, mediaType string) (map[string]any, error) {
	rc, err := p.Deps.Storage.Download(ctx, att.S3Key)
	if err != nil {
		return nil, fmt.Errorf("submit: download attachment %s: %w", att.ID, err)
	}
	defer rc.Close()

	presign, err := p.Deps.Client.PresignMedia(ctx, path.Base(att.S3Key), att.MimeType)
	if err != nil {
		return nil, fmt.Errorf("submit: presign media %s: %w", att.ID, err)
	}
	if err := p.Deps.Client.UploadMedia(ctx, presign.UploadURL, att.MimeType, rc, att.SizeBytes); err != nil {
		return nil, fmt.Errorf("submit: upload media %s: %w", att.ID, err)
	}

	item := map[string]any{"url": presign.PublicURL, "type": mediaType}
	if att.AltText != "" {
		item["altText"] = att.AltText
	}
	return item, nil
}

// recordPublish emits a CON-86 publish/schedule usage event (one post unit)
// carrying platform + social_account_id. The operation is schedule when the
// post has a future ScheduledAt, else publish. accountID is "" on the manual-
// retry path (not re-resolved there). Nil recorder = no-op; tenant is in ctx.
func (p *SubmitPostProcessor) recordPublish(ctx context.Context, post *models.Post, accountID string) {
	op := zernio.OpPublish
	if post.ScheduledAt != nil {
		op = zernio.OpSchedule
	}
	platform := ""
	if post.Platform != nil {
		if s := zernio.LookupSupportedBySqid(post.Platform.ID); s != nil {
			platform = s.ZernioID
		}
	}
	p.Deps.Recorder.Record(ctx, zernio.VendorZernio, "zernio_submit", vendors.MeterEvent{
		Model:           platform,
		Operation:       op,
		Usage:           vendors.Usage{vendors.KindPost: 1},
		Platform:        platform,
		SocialAccountID: accountID,
	})
}

func (p *SubmitPostProcessor) enqueuePoll(ctx context.Context, post *models.Post) {
	client, err := river.ClientFromContextSafely[*sql.Tx](ctx)
	if err != nil || client == nil {
		return
	}
	when := time.Now().UTC().Add(p.pollLead())
	if post.ScheduledAt != nil && post.ScheduledAt.After(time.Now()) {
		when = post.ScheduledAt.Add(p.pollLead())
	}
	if _, err := client.Insert(ctx, PollZernioStatusTask{PostID: post.ID}, insertOptsWithRequestID(ctx, &river.InsertOpts{ScheduledAt: when})); err != nil {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskFailed, post.Status, post.Status,
			"failed to enqueue poll_zernio_status", `{"error":"`+err.Error()+`"}`)
	}
}

func (p *SubmitPostProcessor) pollLead() time.Duration {
	if p.PollLeadTime > 0 {
		return p.PollLeadTime
	}
	return 30 * time.Second
}

// terminal marks the Post Failed and writes a terminal PostLog. The
// returned error is always nil — River must NOT retry once we've
// already moved the Post out of Scheduled.
func (p *SubmitPostProcessor) terminal(ctx context.Context, post *models.Post, reason, msg string) error {
	from := post.Status
	post.Status = models.PostStatusFailed
	post.FailureReason = reason + ": " + msg
	if err := p.Deps.PostRepo.Update(ctx, post); err != nil {
		return fmt.Errorf("submit: mark Failed: %w", err)
	}
	to := post.Status
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventStateTransition, from, to,
		"submit terminally failed", logs.MarshalCapped(map[string]any{
			"reason":  reason,
			"message": msg,
		}))
	// CON-242: tell the post's creator it failed to publish.
	emitPublishNotification(ctx, p.Notifier, post, false)
	return nil
}

// emitPublishNotification drops a publish-outcome notification to the post's
// creator (CON-242). Shared by the submit (terminal failure) and poll
// (published / failed) workers. Best-effort — a nil notifier is a no-op and an
// emit failure never affects the job. The dedupe_key collapses repeats for the
// same post+outcome while still unread (e.g. a manual retry that fails again).
func emitPublishNotification(ctx context.Context, n *notify.Service, post *models.Post, success bool) {
	if n == nil || post == nil {
		return
	}
	platform := "social"
	if post.Platform != nil && post.Platform.Name != "" {
		platform = post.Platform.Name
	}
	spec := notify.Spec{
		EntityType: "post",
		EntityID:   post.ID,
		ActionURL:  "/posts/" + post.ID,
		Data:       map[string]any{"platform": platform},
	}
	if success {
		spec.Level = models.NotificationLevelSuccess
		spec.Type = "post.published"
		spec.Title = "Post published"
		spec.Body = fmt.Sprintf("Your %s post is live.", platform)
		spec.DedupeKey = "post.published:" + post.ID
	} else {
		spec.Level = models.NotificationLevelError
		spec.Type = "post.publish_failed"
		spec.Title = "Post failed to publish"
		spec.Body = fmt.Sprintf("Your %s post couldn't be published.", platform)
		spec.DedupeKey = "post.publish_failed:" + post.ID
		if post.FailureReason != "" {
			spec.Data["failure_reason"] = post.FailureReason
		}
	}
	_ = n.Emit(ctx, post.CreatedBy, spec)
}

// appendLog is a tiny helper that keeps the writes uniform across all
// Zernio queues. Errors are swallowed (PostLog is best-effort) — same
// philosophy as the handler-side helper.
func appendLog(
	ctx context.Context,
	deps ZernioDeps,
	postID string,
	evt models.PostLogEventType,
	from,
	to models.PostStatus,
	summary,
	payload string,
) {
	if deps.PostLogRepo == nil {
		return
	}
	id, _ := models.NewID()
	if id == "" {
		return
	}
	fromCopy := from
	toCopy := to
	_ = deps.PostLogRepo.Append(ctx, &models.PostLog{
		ID:         id,
		PostID:     postID,
		EventType:  evt,
		Actor:      models.ActorSystem,
		FromStatus: &fromCopy,
		ToStatus:   &toCopy,
		Summary:    summary,
		Payload:    logs.SanitizeAndCap(payload),
	})
}

// MarshalSubmit is exported so tests can produce a payload byte slice
// matching what backlite would deserialize.
func MarshalSubmit(t SubmitPostTask) ([]byte, error) { return json.Marshal(t) }
