package models

import (
	"slices"
	"strings"
	"time"

	"github.com/uptrace/bun"
)

// PublisherZernio is the posts.publisher marker stamped on posts
// published through the Zernio adapter. It is the canonical
// source for the marker value; publishers/zernio.PublisherID aliases it.
// Lives in models so the repository layer can filter on it without an
// import cycle (the zernio package imports repository, not vice-versa).
const PublisherZernio = "zernio"

// PostStatus represents the lifecycle state of a post.
type PostStatus string

const (
	PostStatusDraft                     PostStatus = "draft"
	PostStatusReadyForPublish           PostStatus = "ready_for_publish"
	PostStatusScheduled                 PostStatus = "scheduled"
	PostStatusScheduledForManualPublish PostStatus = "scheduled_for_manual_publishing"
	PostStatusFailed                    PostStatus = "failed"
	PostStatusPublished                 PostStatus = "published"
	PostStatusNotPublished              PostStatus = "not_published"
)

// ValidPostTransitions defines the allowed state-machine edges.
// The key is the current status; the value lists statuses it may move to.
//
// Scheduled → ReadyForPublish and Scheduled → Draft support
// user-initiated cancellation of a scheduled post before Zernio publishes
// it.
//
// Scheduled → ScheduledForManualPublish is the direct edge the
// convert-to-manual flow lands on after cancelling the Zernio job, so
// turning off a platform's auto-publish allowlist never detours a
// scheduled post through ready_for_publish (leaving it unscheduled) to
// reach manual publishing. ScheduledForManualPublish → Draft lets a
// manually-scheduled post be moved straight to drafts (channel removal /
// post-type switch-off) without writing a misleading → not_published row
// to the audit log.
//
// Failed → Draft and NotPublished → Draft reopen a post for editing:
// neither status holds a live copy outside Ogen (the submission failed, or
// never left), and going back to draft, not just ready_for_publish, is the
// point: these are precisely the states a post reaches because its content
// needed changing.
var ValidPostTransitions = map[PostStatus][]PostStatus{
	PostStatusDraft:                     {PostStatusReadyForPublish},
	PostStatusReadyForPublish:           {PostStatusScheduled, PostStatusScheduledForManualPublish, PostStatusDraft},
	PostStatusScheduled:                 {PostStatusFailed, PostStatusPublished, PostStatusReadyForPublish, PostStatusDraft, PostStatusScheduledForManualPublish},
	PostStatusScheduledForManualPublish: {PostStatusPublished, PostStatusNotPublished, PostStatusDraft},
	PostStatusFailed:                    {PostStatusReadyForPublish, PostStatusDraft},
	PostStatusNotPublished:              {PostStatusReadyForPublish, PostStatusScheduledForManualPublish, PostStatusDraft},
}

// IsSubmitted reports whether a copy of the post already exists outside
// Ogen — Zernio holds the submission (scheduled) or the social network
// holds the post (published). It is the one named seam CON-251 introduces
// for "the record is locked": the body/title/media/platform/sources freeze
// (editing them would silently diverge Ogen's record from what actually
// goes, or went, out) and the assistant drops to read-only.
//
// It is deliberately NOT isPublished (scheduled locks too, because Zernio
// snapshots the content at schedule time) and NOT isTerminalStatus (which
// would silently mislock the next status anyone adds). A future approval
// flow, channel freeze or client sign-off plugs in here without touching a
// dozen call sites. scheduled_for_manual_publishing is excluded on purpose:
// nobody holds a copy yet, so nothing is locked.
func (s PostStatus) IsSubmitted() bool {
	return s == PostStatusScheduled || s == PostStatusPublished
}

// CanTransition reports whether moving from the current status to next is allowed.
func (s PostStatus) CanTransition(next PostStatus) bool {
	if s == next {
		return true
	}
	return slices.Contains(ValidPostTransitions[s], next)
}

// PostTypeThread is the platform_post_type slug that marks a post as a native
// thread — an ordered chain of messages (root + replies) published as one unit
// on X or Threads. It is the single trigger for the thread path;
// there is no separate boolean. Defined here (not in the platforms package) so
// models, handlers, and the submit worker can key off one constant without an
// import cycle.
const PostTypeThread = "thread"

// ContentFormat is the rhetorical shape of a post (how-to, explainer, ...). It
// is a fixed vocabulary shared with the UI and the writing prompts, not a
// tenant-editable library: the bare label already tells a model the recipe.
type ContentFormat string

const (
	ContentFormatHowTo        ContentFormat = "how-to"
	ContentFormatExplainer    ContentFormat = "explainer"
	ContentFormatListicle     ContentFormat = "listicle"
	ContentFormatStory        ContentFormat = "story"
	ContentFormatDigest       ContentFormat = "digest"
	ContentFormatOpinion      ContentFormat = "opinion"
	ContentFormatQuestion     ContentFormat = "question"
	ContentFormatAnnouncement ContentFormat = "announcement"
)

// ContentFormats lists the vocabulary in the UI picker's order.
var ContentFormats = []ContentFormat{
	ContentFormatHowTo,
	ContentFormatExplainer,
	ContentFormatListicle,
	ContentFormatStory,
	ContentFormatDigest,
	ContentFormatOpinion,
	ContentFormatQuestion,
	ContentFormatAnnouncement,
}

// IsValid reports whether f is one of the known formats. The empty string is
// not: "no format" is NULL, never "".
func (f ContentFormat) IsValid() bool {
	return slices.Contains(ContentFormats, f)
}

// PostTypeLinkPost is the platform_post_type slug for a link post: the post
// carries a target URL in CTAUrl, and the network builds a preview card from
// that page's Open Graph tags. The URL publishes as part of the message (see
// AppendLink) because the publisher has no separate link field.
const PostTypeLinkPost = "link-post"

// PostCTAType represents the call-to-action type attached to a post. It is
// derived from CTAUrl (see CTATypeFor) rather than authored.
type PostCTAType string

const (
	CTATypeLink PostCTAType = "link"
	CTATypeNone PostCTAType = "none"
)

// CTATypeFor derives the CTA type from the post's link: a post carrying a URL
// has a link CTA, any other has none.
func CTATypeFor(ctaURL string) PostCTAType {
	if strings.TrimSpace(ctaURL) != "" {
		return CTATypeLink
	}
	return CTATypeNone
}

type Post struct {
	bun.BaseModel `bun:"table:posts,alias:po" swaggerignore:"true"`
	TenantScoped  // tenant_id column + central scoping hooks

	ID         string `bun:"id,pk"                                        json:"id"`
	CampaignID string `bun:"campaign_id,notnull"                          json:"campaign_id"`
	// platform_id and platform_post_type are nullable so draft posts can
	// exist without a platform chosen up front. They become
	// required when the post moves out of "draft" — enforced by the
	// posts handler, not by the schema.
	//
	// `nullzero` makes bun send NULL (not "") for empty values, which is
	// required for platform_id because the FK to platforms can't match
	// an empty string. We use it on platform_post_type too for symmetry.
	PlatformID       string `bun:"platform_id,nullzero"                         json:"platform_id"`
	PlatformPostType string `bun:"platform_post_type,nullzero"                  json:"platform_post_type"`
	// SocialAccountID names which same-platform account this post
	// publishes to. NULL when unspecified — the submit worker
	// then auto-selects the platform's single account, or fails
	// `account_selection_required` when the platform has more than one.
	// `nullzero` sends NULL (not "") so the FK to social_accounts holds.
	SocialAccountID string `bun:"social_account_id,nullzero" json:"social_account_id"`
	Title           string `bun:"title"                                        json:"title"`
	Content         string `bun:"content,notnull"                              json:"content"`
	// ThreadSegments holds the ordered messages of a threaded post,
	// used only when PlatformPostType == PostTypeThread: index 0 is the root,
	// 1..N-1 the ordered replies. Empty [] for every other post. Content
	// mirrors segment 0 so thread-unaware readers (quality CON-184, listings,
	// campaign summaries CON-152, analytics title) keep working; writes that
	// set segments restamp Content from the root. Per-segment media is carried
	// by PostAttachment.SegmentIndex, not here.
	ThreadSegments ThreadSegments `bun:"thread_segments,notnull,type:jsonb"           json:"thread_segments"`
	MediaURLs      StringSlice    `bun:"media_urls,notnull,type:jsonb"                json:"media_urls"`
	ScheduledAt    *time.Time     `bun:"scheduled_at"                                 json:"scheduled_at"`
	PublishedAt    *time.Time     `bun:"published_at"                                 json:"published_at"`
	Status         PostStatus     `bun:"status,notnull"                               json:"status"`
	// Publisher integration fields (CON-69 §6/§7, generalized to
	// publisher-agnostic names in CON-93 §14).
	// Publisher marks which publisher adapter owns the external identity
	// (matches publishers.Publisher.ID(), e.g. "zernio"); empty until a
	// post is published through a publisher.
	// PublisherPostID is the id the publisher assigns when a Post is
	// submitted; the UNIQUE index in the migration prevents accidental
	// double-submit and is the join key into publisher analytics.
	// PublisherStatus mirrors the publisher's enum verbatim so the polling
	// task can short-circuit on already-handled terminal states.
	// PublishedResults carries the publisher's per-platform outcomes (URLs,
	// platform IDs) once the post resolves.
	// FailureReason distinguishes a publisher-reported failure from
	// reconciliation timeout — see CON-69 §8.
	Publisher        string `bun:"publisher,notnull,default:''"                json:"publisher,omitempty"`
	PublisherPostID  string `bun:"publisher_post_id,nullzero"                  json:"publisher_post_id,omitempty"`
	PublisherStatus  string `bun:"publisher_status,notnull,default:''"         json:"publisher_status,omitempty"`
	PublishedResults string `bun:"published_results,notnull,default:''"        json:"published_results,omitempty"`
	// PublishedURL is the platform permalink for the live post, kept
	// as a first-class field so the front-end can render "View post" off the
	// post it already has, without an analytics round-trip. Set from
	// the publisher's canonical URL on publish/verify, or accepted on PUT for
	// the Zernio skip path. `nullzero` sends NULL (not "") when unset; a URL
	// with an empty PublisherPostID is a user-supplied (unverified) link.
	PublishedURL        string      `bun:"published_url,nullzero"                       json:"published_url,omitempty"`
	FailureReason       string      `bun:"failure_reason,notnull,default:''"           json:"failure_reason,omitempty"`
	CTAType             PostCTAType `bun:"cta_type,notnull"                             json:"cta_type"`
	CTAUrl              string      `bun:"cta_url,notnull"                              json:"cta_url"`
	TargetAudienceNotes string      `bun:"target_audience_notes,notnull"                json:"target_audience_notes"`
	// Brand bindings: this post's own voice/audience. Nullable —
	// resolution falls back to the campaign's, then the workspace default /
	// legacy prose. content_plan/draft_post stamp the voice; both are overridable
	// via PUT /api/posts/:id or the targeted /:id/brand. FK ON DELETE SET NULL.
	BrandVoiceID        *string     `bun:"brand_voice_id"                               json:"brand_voice_id"`
	BrandAudienceID     *string     `bun:"brand_audience_id"                            json:"brand_audience_id"`
	UsedAssetIDs        StringSlice `bun:"used_asset_ids,notnull,type:jsonb"           json:"used_asset_ids"`
	CampaignTypePhaseID *string     `bun:"campaign_type_phase_id"                       json:"campaign_type_phase_id"`
	// ContentFormat is the post's rhetorical shape, independent of its
	// container (PlatformPostType). Nil means no format, the normal case.
	ContentFormat *ContentFormat `bun:"content_format"                               json:"content_format"`
	CreatedBy     string         `bun:"created_by,notnull"                           json:"created_by"`
	CreatedAt     time.Time      `bun:"created_at,notnull,default:current_timestamp" json:"created_at"`
	UpdatedAt     time.Time      `bun:"updated_at,notnull,default:current_timestamp" json:"updated_at"`
	// ClonedFromPostID links a clone back to the Post it was duplicated
	// from. Nil for posts created directly. `nullzero` sends
	// NULL (not "") so the lineage is a clean "has a source / does not".
	ClonedFromPostID *string `bun:"cloned_from_post_id,nullzero" json:"cloned_from_post_id,omitempty"`

	// Hydrated relations — not stored in the database.
	Campaign          *Campaign          `bun:"-" json:"campaign"`
	Platform          *Platform          `bun:"-" json:"platform"`
	SocialAccount     *SocialAccount     `bun:"-" json:"social_account,omitempty"`
	UsedAssets        []Asset            `bun:"-" json:"used_assets"`
	CampaignTypePhase *CampaignTypePhase `bun:"-" json:"campaign_type_phase,omitempty"`
	// CoverURL is the public URL of the picture a post card leads with — its
	// first drawable attachment (see PostAttachmentRepository.CoverKeysByPostIDs).
	// Minted per response by the posts handler and never accepted on writes, so
	// unlike media_urls it cannot round-trip back into a PUT.
	CoverURL *string `bun:"-" json:"cover_url,omitempty"`
}

// IsThread reports whether this post publishes as a native thread — an ordered
// chain of messages rather than a single one. Keyed off the post type
// alone; there is no separate boolean.
func (p *Post) IsThread() bool {
	return p.PlatformPostType == PostTypeThread
}

// SnapshotContent renders the post's content for a CON-251 "what went out"
// PostVersion. As of CON-284 R2, Content IS the canonical thread body — the whole
// ordered chain (with "---" delimiters) lives there, and thread_segments is merely
// derived from it — so the body is already self-contained and injective (distinct
// threads have distinct bodies). The snapshot is Content, plus a link post's
// URL appended the way the publisher appends it, since the URL is part of what
// went out.
func (p *Post) SnapshotContent() string {
	return p.AppendLink(p.Content)
}

// IsLinkPost reports whether this post publishes as a link post.
func (p *Post) IsLinkPost() bool {
	return p.PlatformPostType == PostTypeLinkPost
}

// AppendLink returns text with a link post's URL added as its last paragraph —
// the message the network receives, since the URL travels inside it and the
// network unfurls it into a card. Text that already contains the URL is
// returned unchanged, as is the text of any other post type (a URL kept from
// a type switch does not publish). An empty text yields the bare URL.
func (p *Post) AppendLink(text string) string {
	link := strings.TrimSpace(p.CTAUrl)
	if !p.IsLinkPost() || link == "" || strings.Contains(text, link) {
		return text
	}
	if strings.TrimSpace(text) == "" {
		return link
	}
	return strings.TrimRight(text, " \t\n") + "\n\n" + link
}
