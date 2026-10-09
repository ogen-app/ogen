package queues

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"strings"
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

// SubmitPostToZernioQueue is the River queue name.
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
	// Tenants gates publishing on the owning tenant's lifecycle status:
	// a suspended/deleted tenant's scheduled posts are not published. Nil = no gate.
	Tenants TenantStatusReader
	// Notifier drops a "post failed to publish" notification on a terminal
	// submit failure. Nil is a no-op.
	Notifier *notify.Service
	// Members lists the workspace to fan a publish outcome across (a
	// failure is workspace business, not the author's private problem). Nil falls
	// back to notifying just the author.
	Members memberLister
	// PollLeadTime is how far in advance of scheduled_at we begin
	// polling. Defaults to 30s when zero.
	PollLeadTime time.Duration
}

// Work is the River entrypoint; it delegates to Process.
func (p *SubmitPostProcessor) Work(ctx context.Context, job *river.Job[SubmitPostTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	// Background jobs span tenants (interim until per-tenant, PR4).
	ctx = tenantctx.WithSystem(ctx)
	return p.Process(ctx, job.Args)
}

// Timeout is the per-attempt context deadline.
func (p *SubmitPostProcessor) Timeout(*river.Job[SubmitPostTask]) time.Duration {
	return 30 * time.Second
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &SubmitPostProcessor{Deps: d.Zernio, Tenants: d.Tenants, Notifier: d.Notifier, Members: d.Users})
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
	ctx = tenantctx.With(ctx, post.TenantID)
	if post.Status != models.PostStatusScheduled {
		// User cancelled or reconciliation moved this post; abort quietly. The
		// poll task does the same check.
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"submit aborted: post no longer Scheduled", `{"reason":"status_changed"}`)
		return nil
	}
	// A suspended/deleted tenant publishes nothing. The post stays Scheduled so
	// it resumes if the tenant is reactivated.
	active, err := tenantIsActive(ctx, p.Tenants, post.TenantID)
	if err != nil {
		return fmt.Errorf("submit: tenant status %s: %w", post.TenantID, err)
	}
	if !active {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"submit aborted: tenant not active", `{"reason":"tenant_not_active"}`)
		return nil
	}
	// A manual retry of a previously-failed post still carries the prior
	// attempt's PublisherPostID: Zernio's retry endpoint reuses that job
	// identity instead of creating a duplicate post. When that post no
	// longer exists, retryExisting clears the id and we fall through to a
	// fresh create.
	if post.PublisherPostID != "" {
		if done, err := p.retryExisting(ctx, post); done {
			return err
		}
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
	if p.Deps.ProfileID == nil {
		return p.terminal(ctx, post, "integration_disabled", "Zernio integration is not configured")
	}
	profileID, perr := p.Deps.ProfileID(ctx)
	if perr != nil || profileID == "" {
		return p.terminal(ctx, post, "no_profile", "Zernio profile id not yet bootstrapped")
	}
	accountID, aerr := p.resolveAccountID(ctx, post, profileID, supported.ZernioID)
	if accountID == "" {
		// resolveAccountID either wrote a terminal PostLog (aerr == nil, no
		// retry) or hit a transient error River should retry.
		return aerr
	}

	variant, mediaItems, err := p.buildVariant(ctx, post, supported.ZernioID, accountID)
	if err != nil {
		return err
	}
	req := p.buildRequest(ctx, post, variant, mediaItems)
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioSubmit, post.Status, post.Status,
		"calling Zernio POST /posts", logs.MarshalCapped(req))

	apiStart := time.Now()
	job, submitErr := p.Deps.Client.Submit(ctx, req)
	jobs.ObserveZernioCall(time.Since(apiStart))
	if submitErr != nil {
		return p.handleSubmitErr(ctx, post, req, accountID, submitErr)
	}
	jobs.ZernioSubmitSucceeded.Add(1)
	return p.persistSuccess(ctx, post, job, accountID)
}

// retryExisting re-drives a prior Zernio job through POST /posts/:id/retry.
// It reports false when the Zernio post is gone (404): the stale identity has
// been cleared and the caller should create a fresh post. A 404 means no
// Zernio post exists, so creating one cannot duplicate it.
func (p *SubmitPostProcessor) retryExisting(ctx context.Context, post *models.Post) (bool, error) {
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioRetry, post.Status, post.Status,
		"calling Zernio POST /posts/:id/retry", publisherPostIDPayload(post.PublisherPostID))
	job, retryErr := p.Deps.Client.Retry(ctx, post.PublisherPostID)
	switch {
	case retryErr == nil:
		return true, p.persistSuccess(ctx, post, job, "")
	case zernio.IsStatus(retryErr, http.StatusNotFound):
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioRetry, post.Status, post.Status,
			"Zernio post no longer exists; submitting a fresh post", publisherPostIDPayload(post.PublisherPostID))
		held := post.PublisherPostID
		post.PublisherPostID = ""
		post.PublisherStatus = ""
		post.UpdatedAt = time.Now().UTC()
		ok, err := p.Deps.PostRepo.UpdateSubmission(ctx, post, held, "publisher_post_id", "publisher_status", "updated_at")
		if err != nil {
			return true, fmt.Errorf("submit: clear stale publisher_post_id: %w", err)
		}
		if !ok {
			return true, p.movedOn(ctx, post)
		}
		return false, nil
	case zernio.IsTerminalAPIError(retryErr):
		return true, p.terminal(ctx, post, "zernio_retry_rejected", retryErr.Error())
	default:
		return true, p.transient(ctx, post, "transient Zernio retry error; River will retry", retryErr)
	}
}

// buildVariant uploads the post's attachments to Zernio and returns the
// platform variant plus the top-level mediaItems. A thread carries its media
// inside per-segment threadItems; an ordinary post carries a flat mediaItems
// array. The post title rides in the variant's platformSpecificData, the only
// place Zernio reads it. An upload failure is transient: the post stays Scheduled and River
// retries.
func (p *SubmitPostProcessor) buildVariant(ctx context.Context, post *models.Post, zernioPlatform, accountID string) (zernio.PlatformVariant, []map[string]any, error) {
	variant := zernio.PlatformVariant{Platform: zernioPlatform, AccountID: accountID}
	var data zernio.PlatformSpecificData
	// Only platforms with a title limit take a title; for the rest a stray
	// title would be noise in their platformSpecificData.
	if post.Platform.TextConstraints.MaxTitleChars > 0 {
		data.Title = strings.TrimSpace(post.Title)
	}
	var items []map[string]any
	if post.IsThread() {
		threadItems, err := p.buildThreadItems(ctx, post)
		if err != nil {
			return variant, nil, p.transient(ctx, post, "transient error uploading thread media to Zernio; River will retry", err)
		}
		data.ThreadItems = threadItems
	} else {
		var err error
		items, err = p.buildMediaItems(ctx, post)
		if err != nil {
			return variant, nil, p.transient(ctx, post, "transient error uploading media to Zernio; River will retry", err)
		}
	}
	// A first comment without a delay rides the publish request; a delayed
	// one is posted by PostFirstCommentProcessor once the post is live.
	if post.SendsFirstComment() && post.FirstCommentDelayMinutes == 0 {
		data.FirstComment = platforms.FlattenSocialText(post.FirstComment)
	}
	if data.Title != "" || len(data.ThreadItems) > 0 || data.FirstComment != "" {
		variant.PlatformSpecificData = &data
	}
	return variant, items, nil
}

// buildRequest assembles the submit request. ScheduledFor is the UTC source of
// truth; the workspace timezone is echoed so Zernio renders the schedule in the
// operator's zone.
func (p *SubmitPostProcessor) buildRequest(ctx context.Context, post *models.Post, variant zernio.PlatformVariant, mediaItems []map[string]any) zernio.SubmitRequest {
	when := time.Now().UTC().Add(time.Minute)
	if post.ScheduledAt != nil {
		when = post.ScheduledAt.UTC()
	}
	_, tzName := settings.WorkspaceTimezone(ctx, p.Deps.SettingRepo)

	// post.Content is the full delimited thread body; Zernio's top-level
	// content must be just the root message, and the chain rides in
	// ThreadItems. Networks render no Markdown, so the outbound copy is
	// flattened to the plain text a caption shows; the dedupe recovery matches
	// on this same flattened string. A link post's URL rides in the message —
	// Zernio has no separate link field, and the network unfurls it into a card.
	content := platforms.OutboundText(post)
	if post.IsThread() && len(post.ThreadSegments) > 0 {
		content = platforms.FlattenSocialText(post.ThreadSegments.RootContent())
	}
	return zernio.SubmitRequest{
		Content:      content,
		Platforms:    []zernio.PlatformVariant{variant},
		ScheduledFor: when,
		Timezone:     tzName,
		MediaItems:   mediaItems,
	}
}

// handleSubmitErr settles a failed submit: dedupe recovery for a 409, terminal
// for a rejected request, otherwise a River retry.
func (p *SubmitPostProcessor) handleSubmitErr(ctx context.Context, post *models.Post, req zernio.SubmitRequest, accountID string, err error) error {
	if errors.Is(err, zernio.ErrDuplicateContent) {
		return p.recoverDuplicate(ctx, post, req.Content, accountID)
	}
	if zernio.IsTerminalAPIError(err) {
		jobs.ZernioSubmitFailed.Add(1)
		return p.terminal(ctx, post, "zernio_rejected", err.Error())
	}
	return p.transient(ctx, post, "transient Zernio error; River will retry", err)
}

// recoverDuplicate handles Zernio's 24h dedupe 409 by searching the whole
// window, across all statuses, for the content actually submitted.
func (p *SubmitPostProcessor) recoverDuplicate(ctx context.Context, post *models.Post, content, accountID string) error {
	recovered, err := p.Deps.Client.FindByContent(ctx, content, 24*time.Hour)
	switch {
	case err != nil:
		// Retrying 409s again and re-attempts recovery rather than dead-ending
		// the post.
		return p.transient(ctx, post, "transient error locating dedupe match; River will retry", err)
	case recovered != nil && !recovered.Status.IsTerminal():
		// A still-pending earlier job (almost always this post's own prior
		// attempt): adopt it and keep polling. Counted here because
		// persistSuccess doesn't count.
		jobs.ZernioSubmitSucceeded.Add(1)
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioSubmit, post.Status, post.Status,
			"recovered pending Zernio job after 409 dedupe", logs.MarshalCapped(recovered))
		return p.persistSuccess(ctx, post, recovered, accountID)
	case recovered != nil:
		// The content already ran to a terminal state in this window; adopting a
		// finished job would be wrong, so fail with the cause named.
		jobs.ZernioSubmitFailed.Add(1)
		return p.terminal(ctx, post, "zernio_duplicate_content",
			fmt.Sprintf("identical content was already %s on Zernio within the last 24h (job %s); edit the content to submit again",
				recovered.Status, recovered.ID))
	default:
		// Duplicate per Zernio, but no matching job located (normalisation
		// mismatch, or beyond the search window).
		jobs.ZernioSubmitFailed.Add(1)
		return p.terminal(ctx, post, "zernio_duplicate_content",
			"Zernio rejected this as duplicate content submitted within the last 24h; edit the content or wait for the dedupe window to pass")
	}
}

// transient counts a retried submit, logs it and returns err so River retries.
func (p *SubmitPostProcessor) transient(ctx context.Context, post *models.Post, msg string, err error) error {
	jobs.ZernioSubmitRetried.Add(1)
	p.logRetried(ctx, post, msg, err)
	return err
}

func (p *SubmitPostProcessor) logRetried(ctx context.Context, post *models.Post, msg string, err error) {
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskRetried, post.Status, post.Status, msg, errPayload(err))
}

// resolveAccountID picks the Zernio accountId this post publishes to.
// It returns a non-empty id on success. On failure it returns
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

// persistSuccess records the Zernio post on the Ogen post. The write only
// lands while the post is still scheduled under the submission this job
// loaded; if the user unscheduled or deleted it while the submit was in flight,
// the Zernio post just created belongs to nothing and is withdrawn instead.
func (p *SubmitPostProcessor) persistSuccess(ctx context.Context, post *models.Post, job *zernio.Job, accountID string) error {
	held := post.PublisherPostID
	post.Publisher = zernio.PublisherID
	post.PublisherPostID = job.ID
	post.PublisherStatus = string(job.Status)
	post.UpdatedAt = time.Now().UTC()
	ok, err := p.Deps.PostRepo.UpdateSubmission(ctx, post, held,
		"publisher", "publisher_post_id", "publisher_status", "updated_at")
	if err != nil {
		return fmt.Errorf("submit: persist publisher_post_id: %w", err)
	}
	if !ok {
		return p.withdrawLanded(ctx, post, job.ID)
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
// request, in position order, carrying altText. Nil deps
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

// buildThreadItems builds the ordered per-segment payload for a native thread:
// one ThreadItem per thread_segment (item 0 = root), each carrying
// its own text and its own media. Attachments are grouped by segment_index and
// uploaded to Zernio (presign → PUT) exactly like buildMediaItems, preserving
// position order within a segment. Nil storage/repo ⇒ a text-only thread.
//
// segment_index is optional — a NULL index means the attachment
// belongs to the root message (segment 0), the whole-post default of the
// delimited-body flow. Only a non-NULL, out-of-range index can't be placed, so it
// is skipped with a warning (defence in depth; the gate range-checks it first).
func (p *SubmitPostProcessor) buildThreadItems(ctx context.Context, post *models.Post) ([]zernio.ThreadItem, error) {
	items := make([]zernio.ThreadItem, len(post.ThreadSegments))
	for i := range post.ThreadSegments {
		// Flatten Markdown per segment so each message publishes as plain
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
	task := PollZernioStatusTask{PostID: post.ID, PublisherPostID: post.PublisherPostID}
	if _, err := client.Insert(ctx, task, insertOptsWithRequestID(ctx, &river.InsertOpts{ScheduledAt: when})); err != nil {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskFailed, post.Status, post.Status,
			"failed to enqueue poll_zernio_status", errPayload(err))
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
	post.UpdatedAt = time.Now().UTC()
	ok, err := p.Deps.PostRepo.UpdateSubmission(ctx, post, post.PublisherPostID, "status", "failure_reason", "updated_at")
	if err != nil {
		return fmt.Errorf("submit: mark Failed: %w", err)
	}
	if !ok {
		// The user unscheduled or deleted the post meanwhile; failing it
		// would overwrite the status they chose.
		return p.movedOn(ctx, post)
	}
	to := post.Status
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventStateTransition, from, to,
		"submit terminally failed", logs.MarshalCapped(map[string]any{
			"reason":  reason,
			"message": msg,
		}))
	// Tell the whole workspace it failed to publish.
	emitPublishNotification(ctx, p.Notifier, p.Members, post, false)
	return nil
}

// movedOn ends a submit whose write found the post no longer scheduled under
// the submission it loaded.
func (p *SubmitPostProcessor) movedOn(ctx context.Context, post *models.Post) error {
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, models.PostStatusScheduled, models.PostStatusScheduled,
		"submit aborted: post left Scheduled while submitting", `{"reason":"status_changed"}`)
	return nil
}

// withdrawLanded deletes a Zernio post whose Ogen post left scheduled while
// the submit creating it was in flight. A failed withdrawal is queued for
// retry; if even that fails the orphan sweep finds the post later.
func (p *SubmitPostProcessor) withdrawLanded(ctx context.Context, post *models.Post, publisherPostID string) error {
	task := WithdrawZernioPostTask{
		PublisherPostID: publisherPostID,
		PostID:          post.ID,
		TenantID:        post.TenantID,
		Reason:          WithdrawReasonUnscheduled,
		Actor:           models.ActorSystem,
	}
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioSubmit, models.PostStatusScheduled, models.PostStatusScheduled,
		"submit landed after the post left Scheduled; withdrawing the Zernio post", withdrawPayload(task))
	_, err := withdraw(ctx, p.Deps, task)
	if err == nil {
		return nil
	}
	if qerr := enqueueWithdraw(ctx, task); qerr != nil {
		slog.ErrorContext(ctx, "could not withdraw or queue the withdrawal of an orphaned Zernio post",
			logging.AttrComponent, "jobs.submit", "post_id", post.ID,
			"publisher_post_id", publisherPostID, logging.AttrError, errors.Join(err, qerr))
	}
	return nil
}

// memberLister lists the tenant's members from a tenant-scoped context — the
// workspace fan-out target for a publish outcome. repository.UserRepository
// satisfies it; a nil lister falls back to just the author.
type memberLister interface {
	List(ctx context.Context) ([]models.User, error)
}

// emitPublishNotification drops a publish-outcome notification to the whole
// workspace (CON-242 producer, CON-285 recipient rule: post.published /
// post.publish_failed are workspace business — silence when publishing works
// reads as a broken scheduler, and a failure is not the author's private
// problem). Shared by the submit (terminal failure) and poll (published /
// failed) workers. Best-effort — a nil notifier is a no-op and an emit failure
// never affects the job. The dedupe_key collapses repeats for the same
// post+outcome per recipient while still unread (e.g. a manual retry that fails
// again).
func emitPublishNotification(ctx context.Context, n *notify.Service, members memberLister, post *models.Post, success bool) {
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
	_ = n.EmitToUsers(ctx, publishRecipients(ctx, members, post.CreatedBy), spec)
}

// publishRecipients resolves the workspace members to notify for a publish
// outcome. Best-effort: if the member list can't be loaded it falls back to the
// author alone, and the author is always included even if absent from the list
// (e.g. they since left the workspace).
func publishRecipients(ctx context.Context, members memberLister, author string) []string {
	if members == nil {
		return authorOnly(author)
	}
	users, err := members.List(ctx)
	if err != nil || len(users) == 0 {
		return authorOnly(author)
	}
	ids := make([]string, 0, len(users)+1)
	seen := false
	for _, u := range users {
		if u.ID == "" {
			continue
		}
		ids = append(ids, u.ID)
		if u.ID == author {
			seen = true
		}
	}
	if author != "" && !seen {
		ids = append(ids, author)
	}
	return ids
}

func authorOnly(author string) []string {
	if author == "" {
		return nil
	}
	return []string{author}
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

// errPayload encodes err as a {"error": "..."} log payload. Error text often
// carries quotes, so it must be marshalled rather than concatenated.
func errPayload(err error) string {
	return logs.MarshalCapped(map[string]string{"error": err.Error()})
}

// MarshalSubmit is exported so tests can produce a payload byte slice
// matching what backlite would deserialize.
func MarshalSubmit(t SubmitPostTask) ([]byte, error) { return json.Marshal(t) }
