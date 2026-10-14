package queues

import (
	"context"
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

// Work is the River entrypoint: it runs the job's steps in-process.
func (p *SubmitPostProcessor) Work(ctx context.Context, job *river.Job[SubmitPostTask]) error {
	ctx = WithJobRequestID(ctx, job.JobRow)
	return p.run(ctx, job.Args, job.Attempt, job.MaxAttempts)
}

const (
	// submitBaseTimeout bounds an attempt's API calls and DB work.
	submitBaseTimeout = 30 * time.Second
	// minUploadBytesPerSec is the slowest upload throughput an attempt allows
	// for; the attempt's deadline grows with the attachments' total size.
	minUploadBytesPerSec = 1 << 20
	// zernioMediaRetention is how long Zernio holds an upload in temporary
	// storage; a post must publish within it. A day is kept back as margin.
	zernioMediaRetention = 6 * 24 * time.Hour
	// mediaSourceTTL is how long the signed download link of an attachment
	// handed to a media transfer stays valid.
	mediaSourceTTL = time.Hour
)

// Timeout disables River's per-attempt deadline: run sets its own, sized
// to the post's media.
func (p *SubmitPostProcessor) Timeout(*river.Job[SubmitPostTask]) time.Duration {
	return -1
}

func init() {
	register(func(w *river.Workers, d Deps) {
		river.AddWorker(w, &SubmitPostProcessor{Deps: d.Zernio, Tenants: d.Tenants, Notifier: d.Notifier, Members: d.Users})
	})
}

// Process runs one submit attempt in-process. Returns a non-nil error to ask
// River to retry; returns nil for both success and terminal failure (Post
// moved to Failed when the failure is terminal).
func (p *SubmitPostProcessor) Process(ctx context.Context, task SubmitPostTask) error {
	return p.run(ctx, task, 0, 0)
}

// run runs the steps in-process. The attempt's deadline is submitBaseTimeout
// plus the time the post's media takes to upload at minUploadBytesPerSec.
func (p *SubmitPostProcessor) run(ctx context.Context, task SubmitPostTask, attempt, maxAttempts int) error {
	ctx, cancel := context.WithTimeout(ctx, submitBaseTimeout+p.uploadBudget(ctx, task.PostID))
	defer cancel()
	return runLocal(ctx, p.step, SubmitPostToZernioQueue, task, attempt, maxAttempts,
		localActions{zernio: p.Deps.Client, storage: p.Deps.Storage})
}

// uploadBudget is the extra time the post's attachments may take to upload.
// A listing failure leaves it at zero; the attempt then fails at the base
// deadline and River retries.
func (p *SubmitPostProcessor) uploadBudget(ctx context.Context, postID string) time.Duration {
	if p.Deps.Storage == nil || p.Deps.PostAttachmentRepo == nil {
		return 0
	}
	listCtx, cancel := context.WithTimeout(ctx, submitBaseTimeout)
	defer cancel()
	atts, err := p.Deps.PostAttachmentRepo.ListByPostID(listCtx, postID)
	if err != nil {
		return 0
	}
	var total int64
	for i := range atts {
		total += atts[i].SizeBytes
	}
	return time.Duration(total/minUploadBytesPerSec) * time.Second
}

// Submit phases. A submit retries a prior Zernio post or creates a fresh one:
// each attachment Zernio doesn't still hold is presigned and transferred, then
// the post is submitted. A dedupe 409 pages through Zernio's recent posts for
// the earlier copy, and a post that left Scheduled meanwhile has its new
// Zernio post withdrawn.
const (
	submitPhaseRetry    = "retry"
	submitPhasePresign  = "presign"
	submitPhasePut      = "put"
	submitPhaseSubmit   = "submit"
	submitPhaseDedupe   = "dedupe"
	submitPhaseWithdraw = "withdraw"
)

type submitState struct {
	Phase string `json:"phase"`
	// TenantID lets the steps after a Zernio post may exist go on when the
	// Ogen post is gone, so that Zernio post is withdrawn.
	TenantID  string         `json:"tenant_id,omitempty"`
	AccountID string         `json:"account_id,omitempty"`
	Uploads   []submitUpload `json:"uploads,omitempty"`
	// Next is the upload in progress; UploadURL and PublicURL its presign.
	Next      int    `json:"next,omitempty"`
	UploadURL string `json:"upload_url,omitempty"`
	PublicURL string `json:"public_url,omitempty"`
	// Content is what was submitted, for the dedupe search, and Pages the
	// search's pages so far.
	Content string          `json:"content,omitempty"`
	Pages   []zernio.Result `json:"pages,omitempty"`
	// Withdraw is the Zernio post to withdraw.
	Withdraw string `json:"withdraw,omitempty"`
}

// submitUpload is one attachment the post carries to Zernio. URL is its
// Zernio public URL once uploaded, or a recorded upload Zernio still holds.
type submitUpload struct {
	AttachmentID string `json:"attachment_id"`
	Segment      int    `json:"segment"`
	MediaType    string `json:"media_type"`
	MimeType     string `json:"mime_type"`
	Key          string `json:"key"`
	Size         int64  `json:"size"`
	AltText      string `json:"alt_text,omitempty"`
	URL          string `json:"url,omitempty"`
}

// settled ends the job once an outcome is recorded, or retries it when
// recording failed.
func settled(err error) (StepOutput, error) {
	if err != nil {
		return retry("submit", err)
	}
	return done("settled")
}

func (p *SubmitPostProcessor) step(ctx context.Context, in StepInput) (StepOutput, error) {
	var task SubmitPostTask
	if err := json.Unmarshal(in.Args, &task); err != nil {
		return done("skipped_bad_args")
	}
	var st submitState
	if err := decodeState(in.State, &st); err != nil {
		return done("skipped_bad_state")
	}
	// Background jobs span tenants (interim until per-tenant, PR4).
	ctx = tenantctx.WithSystem(ctx)
	post, err := p.Deps.PostRepo.GetByID(ctx, task.PostID)
	if err != nil {
		if !zernioPostMayExist(st.Phase) {
			return retry("submit: load post "+task.PostID, err)
		}
		// Deleted mid-flight (or unreadable): carry on with what the state
		// holds. The guarded write that follows finds no scheduled post and
		// withdraws the Zernio post, or fails and retries.
		post = &models.Post{ID: task.PostID, Status: models.PostStatusScheduled}
		post.TenantID = st.TenantID
	}
	ctx = tenantctx.With(ctx, post.TenantID)
	if st.Phase == "" {
		return p.start(ctx, post)
	}
	if zernioDisabled(in.Result) {
		return settled(p.terminal(ctx, post, "integration_disabled", "Zernio integration is not configured"))
	}
	switch st.Phase {
	case submitPhaseRetry:
		return p.retried(ctx, post, in.Result)
	case submitPhasePresign:
		return p.presigned(ctx, post, &st, in.Result)
	case submitPhasePut:
		return p.put(ctx, post, &st, in.Result)
	case submitPhaseSubmit:
		return p.submitted(ctx, post, &st, in.Result)
	case submitPhaseDedupe:
		st.Pages = append(st.Pages, zernioResult(in.Result))
		return p.dedupe(ctx, post, &st)
	case submitPhaseWithdraw:
		return p.withdrawn(ctx, post, &st, in.Result)
	default:
		return done("skipped_bad_state")
	}
}

// zernioPostMayExist reports whether a Zernio post for this submit may exist
// by the given phase, so a vanished Ogen post must not stop the job.
func zernioPostMayExist(phase string) bool {
	switch phase {
	case submitPhaseRetry, submitPhaseSubmit, submitPhaseDedupe, submitPhaseWithdraw:
		return true
	default:
		return false
	}
}

// start checks the post can still go out, then retries its prior Zernio post
// or begins a fresh one.
func (p *SubmitPostProcessor) start(ctx context.Context, post *models.Post) (StepOutput, error) {
	if post.Status != models.PostStatusScheduled {
		// User cancelled or reconciliation moved this post; abort quietly. The
		// poll task does the same check.
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"submit aborted: post no longer Scheduled", `{"reason":"status_changed"}`)
		return done("aborted_status_changed")
	}
	// A suspended/deleted tenant publishes nothing. The post stays Scheduled so
	// it resumes if the tenant is reactivated.
	active, err := tenantIsActive(ctx, p.Tenants, post.TenantID)
	if err != nil {
		return retry("submit: tenant status "+post.TenantID, err)
	}
	if !active {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventTaskSucceeded, post.Status, post.Status,
			"submit aborted: tenant not active", `{"reason":"tenant_not_active"}`)
		return done("aborted_tenant_not_active")
	}
	// A manual retry of a previously-failed post still carries the prior
	// attempt's PublisherPostID: Zernio's retry endpoint reuses that job
	// identity instead of creating a duplicate post. When that post no
	// longer exists, the id is cleared and a fresh post is created.
	if post.PublisherPostID != "" {
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioRetry, post.Status, post.Status,
			"calling Zernio POST /posts/:id/retry", publisherPostIDPayload(post.PublisherPostID))
		call, err := zernio.Plan(func(c *zernio.Client) error { _, err := c.Retry(ctx, post.PublisherPostID); return err })
		if err != nil {
			return StepOutput{}, err
		}
		return nextZernio(submitState{Phase: submitPhaseRetry, TenantID: post.TenantID}, call)
	}
	return p.create(ctx, post)
}

// retried settles Zernio's answer to the retry of the prior post. A 404
// means no Zernio post exists, so creating one cannot duplicate it.
func (p *SubmitPostProcessor) retried(ctx context.Context, post *models.Post, res *StepResult) (StepOutput, error) {
	var job *zernio.Job
	retryErr := zernio.Decode(zernioResult(res), func(c *zernio.Client) error {
		var err error
		job, err = c.Retry(ctx, post.PublisherPostID)
		return err
	})
	switch {
	case retryErr == nil:
		return p.persistSuccess(ctx, post, job, "")
	case zernio.IsStatus(retryErr, http.StatusNotFound):
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioRetry, post.Status, post.Status,
			"Zernio post no longer exists; submitting a fresh post", publisherPostIDPayload(post.PublisherPostID))
		held := post.PublisherPostID
		post.PublisherPostID = ""
		post.PublisherStatus = ""
		post.UpdatedAt = time.Now().UTC()
		ok, err := p.Deps.PostRepo.UpdateSubmission(ctx, post, held, "publisher_post_id", "publisher_status", "updated_at")
		if err != nil {
			return retry("submit: clear stale publisher_post_id", err)
		}
		if !ok {
			return settled(p.movedOn(ctx, post))
		}
		return p.create(ctx, post)
	case zernio.IsTerminalAPIError(retryErr):
		return settled(p.terminal(ctx, post, "zernio_retry_rejected", retryErr.Error()))
	default:
		return settled(p.transient(ctx, post, "transient Zernio retry error; River will retry", retryErr))
	}
}

// create resolves where the post goes, then starts on its media.
func (p *SubmitPostProcessor) create(ctx context.Context, post *models.Post) (StepOutput, error) {
	platform := post.Platform
	if platform == nil {
		return settled(p.terminal(ctx, post, "missing_platform", "post has no platform set"))
	}
	supported := zernio.LookupSupportedBySqid(platform.ID)
	if supported == nil {
		return settled(p.terminal(ctx, post, "unsupported_platform",
			fmt.Sprintf("platform %s (%s) is not Zernio-supported", platform.Name, platform.ID)))
	}
	if p.Deps.ProfileID == nil {
		return settled(p.terminal(ctx, post, "integration_disabled", "Zernio integration is not configured"))
	}
	profileID, perr := p.Deps.ProfileID(ctx)
	if perr != nil || profileID == "" {
		return settled(p.terminal(ctx, post, "no_profile", "Zernio profile id not yet bootstrapped"))
	}
	accountID, aerr := p.resolveAccountID(ctx, post, profileID, supported.ZernioID)
	if accountID == "" {
		// resolveAccountID either wrote a terminal PostLog (aerr == nil, no
		// retry) or hit a transient error River should retry.
		return settled(aerr)
	}
	uploads, err := p.planUploads(ctx, post)
	if err != nil {
		return settled(p.transient(ctx, post, "transient error uploading media to Zernio; River will retry", err))
	}
	return p.nextUpload(ctx, post, &submitState{TenantID: post.TenantID, AccountID: accountID, Uploads: uploads})
}

// planUploads lists the attachments the post carries, in position order, each
// placed on its thread segment. Nil deps (Storage/PostAttachmentRepo) or no
// attachments ⇒ none (a text-only post). Attachments whose MIME type Zernio
// doesn't accept are skipped, not fatal; so is a thread attachment whose
// segment_index is out of range (defence in depth; the gate range-checks it
// first). A NULL segment_index puts the attachment on the root message.
func (p *SubmitPostProcessor) planUploads(ctx context.Context, post *models.Post) ([]submitUpload, error) {
	if p.Deps.Storage == nil || p.Deps.PostAttachmentRepo == nil {
		return nil, nil
	}
	atts, err := p.Deps.PostAttachmentRepo.ListByPostID(ctx, post.ID)
	if err != nil {
		return nil, fmt.Errorf("submit: list attachments: %w", err)
	}
	thread := post.IsThread()
	var uploads []submitUpload
	now := time.Now()
	for i := range atts {
		att := &atts[i]
		seg := 0
		if thread && att.SegmentIndex != nil {
			seg = *att.SegmentIndex
		}
		if thread && (seg < 0 || seg >= len(post.ThreadSegments)) {
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
		u := submitUpload{
			AttachmentID: att.ID, Segment: seg, MediaType: mediaType, MimeType: att.MimeType,
			Key: att.S3Key, Size: att.SizeBytes, AltText: att.AltText,
		}
		// Reuse the previous upload while Zernio still holds it at publish time.
		if reusableUpload(att, post, now) {
			u.URL = att.PublisherMediaURL
		}
		uploads = append(uploads, u)
	}
	return uploads, nil
}

// nextUpload presigns the next attachment not yet on Zernio, or submits the
// post once they all are.
func (p *SubmitPostProcessor) nextUpload(ctx context.Context, post *models.Post, st *submitState) (StepOutput, error) {
	for st.Next < len(st.Uploads) && st.Uploads[st.Next].URL != "" {
		st.Next++
	}
	if st.Next == len(st.Uploads) {
		return p.submit(ctx, post, st)
	}
	u := st.Uploads[st.Next]
	call, err := zernio.Plan(func(c *zernio.Client) error { _, err := c.PresignMedia(ctx, path.Base(u.Key), u.MimeType); return err })
	if err != nil {
		return StepOutput{}, err
	}
	st.Phase = submitPhasePresign
	return nextZernio(st, call)
}

// presigned streams the attachment to the upload URL Zernio handed out.
func (p *SubmitPostProcessor) presigned(ctx context.Context, post *models.Post, st *submitState, res *StepResult) (StepOutput, error) {
	u := st.Uploads[st.Next]
	var presign *zernio.MediaPresign
	err := zernio.Decode(zernioResult(res), func(c *zernio.Client) error {
		var err error
		presign, err = c.PresignMedia(ctx, path.Base(u.Key), u.MimeType)
		return err
	})
	if err != nil {
		return settled(p.uploadFailed(ctx, post, fmt.Errorf("submit: presign media %s: %w", u.AttachmentID, err)))
	}
	source, err := p.Deps.Storage.PresignedGetURL(ctx, u.Key, mediaSourceTTL)
	if err != nil {
		return settled(p.uploadFailed(ctx, post, fmt.Errorf("submit: download attachment %s: %w", u.AttachmentID, err)))
	}
	st.Phase, st.UploadURL, st.PublicURL = submitPhasePut, presign.UploadURL, presign.PublicURL
	raw, err := encodeState(st)
	if err != nil {
		return StepOutput{}, err
	}
	return StepOutput{State: raw, Transfer: &MediaTransfer{
		SourceURL: source, UploadURL: presign.UploadURL, ContentType: u.MimeType, Size: u.Size, storageKey: u.Key,
	}}, nil
}

// put records a finished upload for the next attempt, then moves on.
func (p *SubmitPostProcessor) put(ctx context.Context, post *models.Post, st *submitState, res *StepResult) (StepOutput, error) {
	u := &st.Uploads[st.Next]
	if err := transferErr(res); err != nil {
		return settled(p.uploadFailed(ctx, post, fmt.Errorf("submit: upload media %s: %w", u.AttachmentID, err)))
	}
	u.URL = st.PublicURL
	if err := p.Deps.PostAttachmentRepo.SetPublisherMedia(ctx, u.AttachmentID, u.URL, time.Now().UTC()); err != nil {
		slog.WarnContext(ctx, "failed to record Zernio media upload; a retry will upload it again",
			logging.AttrComponent, "jobs.submit", "post_id", post.ID, "attachment_id", u.AttachmentID, logging.AttrError, err)
	}
	st.Next++
	st.UploadURL, st.PublicURL = "", ""
	return p.nextUpload(ctx, post, st)
}

// transferErr is a media transfer's failure, if any.
func transferErr(res *StepResult) error {
	switch {
	case res == nil:
		return errors.New("no result")
	case res.Err != "":
		return errors.New(res.Err)
	case res.Status >= 400:
		return &zernio.APIError{Status: res.Status, Message: strings.TrimSpace(string(res.Body))}
	default:
		return nil
	}
}

// uploadFailed is transient: the post stays Scheduled and River retries.
func (p *SubmitPostProcessor) uploadFailed(ctx context.Context, post *models.Post, err error) error {
	msg := "transient error uploading media to Zernio; River will retry"
	if post.IsThread() {
		msg = "transient error uploading thread media to Zernio; River will retry"
	}
	return p.transient(ctx, post, msg, err)
}

// submit sends the post with its uploaded media.
func (p *SubmitPostProcessor) submit(ctx context.Context, post *models.Post, st *submitState) (StepOutput, error) {
	if post.Platform == nil || zernio.LookupSupportedBySqid(post.Platform.ID) == nil {
		return settled(p.terminal(ctx, post, "unsupported_platform", "post platform changed while submitting"))
	}
	req := p.buildRequest(ctx, post, p.buildVariant(post, st), mediaItems(post, st.Uploads))
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioSubmit, post.Status, post.Status,
		"calling Zernio POST /posts", logs.MarshalCapped(req))
	call, err := zernio.Plan(func(c *zernio.Client) error { _, err := c.Submit(ctx, req); return err })
	if err != nil {
		return StepOutput{}, err
	}
	st.Phase, st.Content = submitPhaseSubmit, req.Content
	st.Uploads = nil
	return nextZernio(st, call)
}

// submitted settles Zernio's answer to the submit: dedupe recovery for a
// 409, terminal for a rejected request, otherwise a River retry.
func (p *SubmitPostProcessor) submitted(ctx context.Context, post *models.Post, st *submitState, res *StepResult) (StepOutput, error) {
	var job *zernio.Job
	err := zernio.Decode(zernioResult(res), func(c *zernio.Client) error {
		var err error
		job, err = c.Submit(ctx, zernio.SubmitRequest{})
		return err
	})
	switch {
	case err == nil:
		jobs.ZernioSubmitSucceeded.Add(1)
		return p.persistSuccess(ctx, post, job, st.AccountID)
	case errors.Is(err, zernio.ErrDuplicateContent):
		st.Phase, st.Pages = submitPhaseDedupe, nil
		return p.dedupe(ctx, post, st)
	case zernio.IsTerminalAPIError(err):
		jobs.ZernioSubmitFailed.Add(1)
		return settled(p.terminal(ctx, post, "zernio_rejected", err.Error()))
	default:
		return settled(p.transient(ctx, post, "transient Zernio error; River will retry", err))
	}
}

// buildVariant returns the platform variant. A thread carries its media
// inside per-segment threadItems; an ordinary post carries a flat mediaItems
// array (see mediaItems). The post title rides in the variant's
// platformSpecificData, the only place Zernio reads it.
func (p *SubmitPostProcessor) buildVariant(post *models.Post, st *submitState) zernio.PlatformVariant {
	variant := zernio.PlatformVariant{Platform: zernio.LookupSupportedBySqid(post.Platform.ID).ZernioID, AccountID: st.AccountID}
	var data zernio.PlatformSpecificData
	// Only platforms with a title limit take a title; for the rest a stray
	// title would be noise in their platformSpecificData.
	if post.Platform.TextConstraints.MaxTitleChars > 0 {
		data.Title = strings.TrimSpace(post.Title)
	}
	if post.IsThread() {
		data.ThreadItems = threadItems(post, st.Uploads)
	}
	// A first comment without a delay rides the publish request; a delayed
	// one is posted by PostFirstCommentProcessor once the post is live.
	if post.SendsFirstComment() && post.FirstCommentDelayMinutes == 0 {
		data.FirstComment = platforms.FlattenSocialText(post.FirstComment)
	}
	if data.Title != "" || len(data.ThreadItems) > 0 || data.FirstComment != "" {
		variant.PlatformSpecificData = &data
	}
	return variant
}

// mediaItem is one upload's mediaItems descriptor {url, type, altText?}.
func mediaItem(u submitUpload) map[string]any {
	item := map[string]any{"url": u.URL, "type": u.MediaType}
	if u.AltText != "" {
		item["altText"] = u.AltText
	}
	return item
}

// mediaItems is an ordinary post's top-level mediaItems, in position order;
// nil for a thread, whose media rides in its segments.
func mediaItems(post *models.Post, uploads []submitUpload) []map[string]any {
	if post.IsThread() || len(uploads) == 0 {
		return nil
	}
	items := make([]map[string]any, 0, len(uploads))
	for _, u := range uploads {
		items = append(items, mediaItem(u))
	}
	return items
}

// threadItems is a native thread's per-segment payload: one ThreadItem per
// thread_segment (item 0 = root), each with its own text and media in
// position order. Markdown is flattened per segment so each message publishes
// as plain text (Zernio ships the content verbatim); VisibleLen counts this
// same flattened form, so a segment that passed the per-message gate fits.
func threadItems(post *models.Post, uploads []submitUpload) []zernio.ThreadItem {
	items := make([]zernio.ThreadItem, len(post.ThreadSegments))
	for i := range post.ThreadSegments {
		items[i] = zernio.ThreadItem{Content: platforms.FlattenSocialText(post.ThreadSegments[i].Content)}
	}
	for _, u := range uploads {
		items[u.Segment].MediaItems = append(items[u.Segment].MediaItems, mediaItem(u))
	}
	return items
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

// dedupe handles Zernio's 24h dedupe 409 by searching the whole window,
// across all statuses, for the content actually submitted, a page per step.
func (p *SubmitPostProcessor) dedupe(ctx context.Context, post *models.Post, st *submitState) (StepOutput, error) {
	var recovered *zernio.Job
	next, err := zernio.Replay(st.Pages, func(c *zernio.Client) error {
		var err error
		recovered, err = c.FindByContent(ctx, st.Content, 24*time.Hour)
		return err
	})
	switch {
	case next != nil:
		return nextZernio(st, *next)
	case err != nil:
		// Retrying 409s again and re-attempts recovery rather than dead-ending
		// the post.
		return settled(p.transient(ctx, post, "transient error locating dedupe match; River will retry", err))
	case recovered != nil && !recovered.Status.IsTerminal():
		// A still-pending earlier job (almost always this post's own prior
		// attempt): adopt it and keep polling. Counted here because
		// persistSuccess doesn't count.
		jobs.ZernioSubmitSucceeded.Add(1)
		appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioSubmit, post.Status, post.Status,
			"recovered pending Zernio job after 409 dedupe", logs.MarshalCapped(recovered))
		return p.persistSuccess(ctx, post, recovered, st.AccountID)
	case recovered != nil:
		// The content already ran to a terminal state in this window; adopting a
		// finished job would be wrong, so fail with the cause named.
		jobs.ZernioSubmitFailed.Add(1)
		return settled(p.terminal(ctx, post, "zernio_duplicate_content",
			fmt.Sprintf("identical content was already %s on Zernio within the last 24h (job %s); edit the content to submit again",
				recovered.Status, recovered.ID)))
	default:
		// Duplicate per Zernio, but no matching job located (normalisation
		// mismatch, or beyond the search window).
		jobs.ZernioSubmitFailed.Add(1)
		return settled(p.terminal(ctx, post, "zernio_duplicate_content",
			"Zernio rejected this as duplicate content submitted within the last 24h; edit the content or wait for the dedupe window to pass"))
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
func (p *SubmitPostProcessor) persistSuccess(ctx context.Context, post *models.Post, job *zernio.Job, accountID string) (StepOutput, error) {
	held := post.PublisherPostID
	post.Publisher = zernio.PublisherID
	post.PublisherPostID = job.ID
	post.PublisherStatus = string(job.Status)
	post.UpdatedAt = time.Now().UTC()
	ok, err := p.Deps.PostRepo.UpdateSubmission(ctx, post, held,
		"publisher", "publisher_post_id", "publisher_status", "updated_at")
	if err != nil {
		return retry("submit: persist publisher_post_id", err)
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
	return done("submitted")
}

// reusableUpload reports whether att's recorded Zernio upload will still be in
// Zernio's temporary storage when the post publishes (now, or at its scheduled
// time if later).
func reusableUpload(att *models.PostAttachment, post *models.Post, now time.Time) bool {
	if att.PublisherMediaURL == "" || att.PublisherMediaUploadedAt == nil {
		return false
	}
	publishAt := now
	if post.ScheduledAt != nil && post.ScheduledAt.After(now) {
		publishAt = *post.ScheduledAt
	}
	return att.PublisherMediaUploadedAt.Add(zernioMediaRetention).After(publishAt)
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
	when := time.Now().UTC().Add(p.pollLead())
	if post.ScheduledAt != nil && post.ScheduledAt.After(time.Now()) {
		when = post.ScheduledAt.Add(p.pollLead())
	}
	task := PollZernioStatusTask{PostID: post.ID, PublisherPostID: post.PublisherPostID}
	if err := p.Deps.Jobs.enqueue(ctx, task, &river.InsertOpts{ScheduledAt: when}); err != nil {
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
// the submit creating it was in flight.
func (p *SubmitPostProcessor) withdrawLanded(ctx context.Context, post *models.Post, publisherPostID string) (StepOutput, error) {
	task := p.landedWithdrawal(post, publisherPostID)
	appendLog(ctx, p.Deps, post.ID, models.PostLogEventZernioSubmit, models.PostStatusScheduled, models.PostStatusScheduled,
		"submit landed after the post left Scheduled; withdrawing the Zernio post", withdrawPayload(task))
	call, err := zernio.Plan(func(c *zernio.Client) error { return c.Cancel(ctx, publisherPostID) })
	if err != nil {
		return StepOutput{}, err
	}
	return nextZernio(submitState{Phase: submitPhaseWithdraw, TenantID: post.TenantID, Withdraw: publisherPostID}, call)
}

// withdrawn settles the withdrawal. One that failed is queued for retry; if
// even that fails the orphan sweep finds the post later.
func (p *SubmitPostProcessor) withdrawn(ctx context.Context, post *models.Post, st *submitState, res *StepResult) (StepOutput, error) {
	task := p.landedWithdrawal(post, st.Withdraw)
	cancelErr := zernio.Decode(zernioResult(res), func(c *zernio.Client) error { return c.Cancel(ctx, st.Withdraw) })
	_, err := withdraw(ctx, p.Deps, task, cancelErr)
	if err == nil {
		return done("withdrawn_landed")
	}
	if qerr := p.Deps.Jobs.enqueue(ctx, task, nil); qerr != nil {
		slog.ErrorContext(ctx, "could not withdraw or queue the withdrawal of an orphaned Zernio post",
			logging.AttrComponent, "jobs.submit", "post_id", post.ID,
			"publisher_post_id", st.Withdraw, logging.AttrError, errors.Join(err, qerr))
	}
	return done("withdraw_queued")
}

func (p *SubmitPostProcessor) landedWithdrawal(post *models.Post, publisherPostID string) WithdrawZernioPostTask {
	return WithdrawZernioPostTask{
		PublisherPostID: publisherPostID,
		PostID:          post.ID,
		TenantID:        post.TenantID,
		Reason:          WithdrawReasonUnscheduled,
		Actor:           models.ActorSystem,
	}
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
