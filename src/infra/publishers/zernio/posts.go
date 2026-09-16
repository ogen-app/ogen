package zernio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// JobStatus is Zernio's post-level status enum. The webhook-only
// states (cancelled, recycled) are listed for completeness; this
// integration is polling-only and does not see them.
type JobStatus string

const (
	JobStatusDraft     JobStatus = "draft"
	JobStatusScheduled JobStatus = "scheduled"
	JobStatusPublished JobStatus = "published"
	JobStatusFailed    JobStatus = "failed"
	JobStatusPartial   JobStatus = "partial"
)

// IsTerminal reports whether s indicates the job will not progress on
// its own — the publisher loop should stop polling and resolve the
// owning Post accordingly.
func (s JobStatus) IsTerminal() bool {
	switch s {
	case JobStatusPublished, JobStatusFailed, JobStatusPartial:
		return true
	}
	return false
}

// PlatformVariant is one (platform, account) pairing in a submit
// request. AccountID is the connected social account on Zernio's side.
type PlatformVariant struct {
	Platform  string `json:"platform"`
	AccountID string `json:"accountId"`
	// PlatformSpecificData carries per-platform extras — today only a native
	// thread's ordered messages (CON-284). A pointer so it (and its
	// platformSpecificData key) is omitted entirely for ordinary posts, keeping
	// their payload byte-identical to before.
	PlatformSpecificData *PlatformSpecificData `json:"platformSpecificData,omitempty"`
}

// PlatformSpecificData is the per-variant extras bag. Serialised as
// platformSpecificData; only ThreadItems is populated so far.
type PlatformSpecificData struct {
	ThreadItems []ThreadItem `json:"threadItems,omitempty"`
}

// ThreadItem is one message of a native thread (CON-284). Item 0 is the root;
// the rest become ordered replies — Zernio owns the reply-chaining, so Ogen
// sends the whole chain in one submit. Content is the message text; MediaItems
// carries that message's media in the same {url,type,altText?} shape as the
// top-level SubmitRequest.MediaItems.
type ThreadItem struct {
	Content    string           `json:"content"`
	MediaItems []map[string]any `json:"mediaItems,omitempty"`
}

// PlatformOutcome is one row of the per-platform status returned in
// any post envelope. ErrorMessage is populated only when Status is
// failed for that platform; PlatformPostURL appears once published.
type PlatformOutcome struct {
	Platform        string `json:"platform"`
	AccountID       string `json:"accountId"`
	Status          string `json:"status"`
	ErrorMessage    string `json:"error,omitempty"`
	PlatformPostURL string `json:"platformPostUrl,omitempty"`
}

// UnmarshalJSON tolerates Zernio returning `accountId` as either a bare
// ObjectId string or a populated account sub-document ({"_id": "...", ...}).
// Which shape arrives depends on whether the upstream query populates the
// reference — the same ambiguity handled for Account.profileId — so we project
// down to the string ID either way. Without this, a populated account crashes
// the whole submit flow at the FindByContent dedupe-recovery decode.
func (o *PlatformOutcome) UnmarshalJSON(data []byte) error {
	// Avoid recursion via a type alias that drops UnmarshalJSON.
	type outcomeWire struct {
		Platform        string          `json:"platform"`
		AccountID       json.RawMessage `json:"accountId"`
		Status          string          `json:"status"`
		ErrorMessage    string          `json:"error"`
		PlatformPostURL string          `json:"platformPostUrl"`
	}
	var w outcomeWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	o.Platform = w.Platform
	o.Status = w.Status
	o.ErrorMessage = w.ErrorMessage
	o.PlatformPostURL = w.PlatformPostURL
	id, err := decodeRefID(w.AccountID)
	if err != nil {
		return fmt.Errorf("zernio: platformOutcome.accountId: %w", err)
	}
	o.AccountID = id
	return nil
}

// SubmitRequest is the body for POST /api/v1/posts. Either ScheduledFor
// (with TZ) or PublishNow must be set; we always go through ScheduledFor
// because the queue dispatch path computes the timestamp explicitly.
type SubmitRequest struct {
	Content      string            `json:"content"`
	Platforms    []PlatformVariant `json:"platforms"`
	ScheduledFor time.Time         `json:"scheduledFor,omitzero"`
	Timezone     string            `json:"timezone,omitempty"`
	PublishNow   bool              `json:"publishNow,omitzero"`
	IsDraft      bool              `json:"isDraft,omitzero"`
	// MediaItems carries opaque media descriptors (URLs to S3 objects,
	// as Zernio expects). Populated by the queue handler from the Post's
	// PostAttachment rows.
	MediaItems []map[string]any `json:"mediaItems,omitempty"`
	// Title carries the post title for platforms that need one explicitly —
	// principally YouTube video (CON-148 §6.6). Omitted when empty; platforms
	// that derive a title from the caption ignore it. SPIKE-PENDING: the exact
	// Zernio shape (top-level vs per-PlatformVariant, plus description/tags/
	// privacy/thumbnail) is unconfirmed against docs.zernio.com — this is the
	// minimal field; extend once the §6.6 spike lands.
	Title string `json:"title,omitempty"`
}

// PostEnvelope mirrors Zernio's standard `{post: {...}}` response
// envelope shape (matches the convention used by profiles.go).
type PostEnvelope struct {
	Post Job `json:"post"`
}

// listEnvelope mirrors the `{posts: [...], pagination: {...}}` shape.
type listEnvelope struct {
	Posts      []Job `json:"posts"`
	Pagination struct {
		Page  int `json:"page"`
		Limit int `json:"limit"`
		Total int `json:"total"`
		Pages int `json:"pages"`
	} `json:"pagination"`
}

// Job is the canonical Zernio post object as returned by create / get
// / list. Field names mirror Zernio's exactly; we keep the verbose
// names rather than renaming so debugging against their docs stays
// straightforward.
type Job struct {
	ID           string            `json:"_id"`
	Status       JobStatus         `json:"status"`
	Content      string            `json:"content"`
	ScheduledFor *time.Time        `json:"scheduledFor,omitempty"`
	PublishedAt  *time.Time        `json:"publishedAt,omitempty"`
	Timezone     string            `json:"timezone,omitempty"`
	Platforms    []PlatformOutcome `json:"platforms"`
}

// ErrAlreadyPublished is returned by Cancel when Zernio reports the
// underlying job has already published — cancel is a no-op in that
// case and the next poll will land Published on Ogen's side.
var ErrAlreadyPublished = errors.New("zernio: job already published, cancel no-op")

// ErrDuplicateContent is returned by Submit when Zernio rejects the
// request as a same-content repeat within its 24h dedupe window. The
// caller is expected to recover by calling FindByContent to locate
// the existing job ID.
var ErrDuplicateContent = errors.New("zernio: duplicate content within 24h dedupe window")

// IsTerminalAPIError reports whether err is a Zernio HTTP error that
// the queue layer should NOT retry — 4xx (validation, auth, business
// rejection) other than 429 (rate limit, transient).
func IsTerminalAPIError(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok {
		return false
	}
	if apiErr.Status == http.StatusTooManyRequests {
		return false
	}
	return apiErr.Status >= 400 && apiErr.Status < 500
}

// IsTransientAPIError reports whether err is something the queue
// layer should retry: 5xx, 429, or anything that isn't an APIError
// (typically network/timeout).
func IsTransientAPIError(err error) bool {
	apiErr, ok := errors.AsType[*APIError](err)
	if !ok {
		return err != nil
	}
	return apiErr.Status >= 500 || apiErr.Status == http.StatusTooManyRequests
}

// Submit creates a Zernio post. Returns the freshly created Job with
// its ID populated. On 409 (24h dedupe) returns ErrDuplicateContent
// so the queue handler can recover the existing job ID via
// FindByContent.
func (c *Client) Submit(ctx context.Context, req SubmitRequest) (*Job, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	var env PostEnvelope
	err := c.do(ctx, http.MethodPost, "/posts", nil, req, &env)
	if err != nil {
		if apiErr, ok := errors.AsType[*APIError](err); ok && apiErr.Status == http.StatusConflict {
			return nil, fmt.Errorf("%w: %s", ErrDuplicateContent, apiErr.Message)
		}
		return nil, err
	}
	return &env.Post, nil
}

// Status fetches a single Zernio post by id.
func (c *Client) Status(ctx context.Context, jobID string) (*Job, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	if jobID == "" {
		return nil, errors.New("zernio: jobID is required")
	}
	var env PostEnvelope
	if err := c.do(ctx, http.MethodGet, "/posts/"+url.PathEscape(jobID), nil, nil, &env); err != nil {
		return nil, err
	}
	return &env.Post, nil
}

// Cancel removes a scheduled Zernio post via DELETE — Zernio's
// documented cancellation path. Returns ErrAlreadyPublished when the
// job has already crossed the publish boundary (404 or a 409 with the
// "already published" message); the caller treats that as a no-op and
// lets the next poll resolve Published.
func (c *Client) Cancel(ctx context.Context, jobID string) error {
	if c == nil {
		return errors.New("zernio: client is disabled")
	}
	if jobID == "" {
		return errors.New("zernio: jobID is required")
	}
	err := c.do(ctx, http.MethodDelete, "/posts/"+url.PathEscape(jobID), nil, nil, nil)
	if err == nil {
		return nil
	}
	if apiErr, ok := errors.AsType[*APIError](err); ok {
		switch apiErr.Status {
		case http.StatusNotFound:
			// Job is gone — either already published or never existed.
			// Either way, cancel is a no-op from Ogen's perspective.
			return ErrAlreadyPublished
		case http.StatusConflict:
			return ErrAlreadyPublished
		}
	}
	return err
}

// Retry asks Zernio to reattempt a failed publish. Used by the manual
// retry path (CON-69 §10) when an Ogen Post is being re-promoted out
// of Failed state and a Zernio job already exists for it.
func (c *Client) Retry(ctx context.Context, jobID string) (*Job, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	if jobID == "" {
		return nil, errors.New("zernio: jobID is required")
	}
	var env PostEnvelope
	if err := c.do(ctx, http.MethodPost, "/posts/"+url.PathEscape(jobID)+"/retry", nil, nil, &env); err != nil {
		return nil, err
	}
	return &env.Post, nil
}

// FindByContent recovers the existing Zernio job after a 24h-dedupe 409 by
// listing posts within the dedupe window and matching on exact content, so the
// owning Ogen Post still ends up with a job_id to reason about.
//
// It does NOT filter by status: Zernio dedupes identical content across the
// window regardless of what became of the earlier job (scheduled, published,
// failed, cancelled), so the recovery search must span every status too —
// otherwise a post whose earlier job has left `scheduled` dead-ends
// (CON-129). The returned Job carries its Status so the caller can decide
// whether to adopt (still pending) or report (already terminal). It paginates
// so a busy workspace's older match isn't missed past the first page.
//
// Returns (nil, nil) if no match is found within the lookback window. Callers
// should pass the exact content they submitted — which is the Markdown-flattened
// body (CON-126); this function matches it verbatim, so both sides compare the
// same string.
func (c *Client) FindByContent(ctx context.Context, content string, lookback time.Duration) (*Job, error) {
	if c == nil {
		return nil, errors.New("zernio: client is disabled")
	}
	if lookback <= 0 {
		lookback = 24 * time.Hour
	}
	// maxDedupePages bounds the scan so recovery on a very busy workspace can't
	// page unbounded; the dedupe window keeps the candidate set small anyway.
	const maxDedupePages = 20
	dateFrom := time.Now().UTC().Add(-lookback).Format(time.RFC3339)
	for page := 1; page <= maxDedupePages; page++ {
		q := url.Values{}
		q.Set("dateFrom", dateFrom)
		q.Set("limit", "100")
		q.Set("sortBy", "created-desc")
		q.Set("page", strconv.Itoa(page))

		var env listEnvelope
		if err := c.do(ctx, http.MethodGet, "/posts", q, nil, &env); err != nil {
			return nil, err
		}
		for i := range env.Posts {
			if env.Posts[i].Content == content {
				return &env.Posts[i], nil
			}
		}
		if env.Pagination.Pages == 0 || page >= env.Pagination.Pages {
			break
		}
	}
	return nil, nil
}
