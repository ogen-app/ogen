package platforms

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/models"
)

// PostTypeRule captures the structural requirements for one post-type
// slug. Slugs are stable across platforms (the seed at
// `database/migrations/20240107000002_seed_platforms.up.sql` reuses the
// same identifiers everywhere), so the table is keyed by slug rather
// than by (platform, slug).
//
// `MaxAttachments == -1` means "unbounded by this rule" — the
// per-platform attachment validator still applies its own cap from
// ImageConstraints.MaxAttachmentsPerPost. `AllowedKinds == nil` means
// the rule does not restrict attachment kinds (only counts).
type PostTypeRule struct {
	RequiresContent bool
	AllowedKinds    []string
	MinAttachments  int
	MaxAttachments  int
}

// postTypeRules holds the v1 rule table. A slug present in
// `Platform.PostTypes` but absent from this map is whitelist-only:
// validation passes, and the platform handles the rest server-side.
var postTypeRules = map[string]PostTypeRule{
	"text-post": {MinAttachments: 0, MaxAttachments: 0},

	"image-post": {AllowedKinds: []string{KindImage}, MinAttachments: 1, MaxAttachments: -1},
	"carousel":   {AllowedKinds: []string{KindImage}, MinAttachments: 2, MaxAttachments: -1},

	"video": {AllowedKinds: []string{KindVideo}, MinAttachments: 1, MaxAttachments: 1},
	"reel":  {AllowedKinds: []string{KindVideo}, MinAttachments: 1, MaxAttachments: 1},
	"short": {AllowedKinds: []string{KindVideo}, MinAttachments: 1, MaxAttachments: 1},

	"poll":   {RequiresContent: true, MinAttachments: 0, MaxAttachments: 0},
	"thread": {RequiresContent: true, MinAttachments: 0, MaxAttachments: -1},

	"article":        {RequiresContent: true, MinAttachments: 0, MaxAttachments: -1},
	"long-form-post": {RequiresContent: true, MinAttachments: 0, MaxAttachments: -1},
	"newsletter":     {RequiresContent: true, MinAttachments: 0, MaxAttachments: -1},

	"story": {AllowedKinds: []string{KindImage, KindVideo}, MinAttachments: 1, MaxAttachments: 1},
}

// RuleFor returns the structural rule for a post-type slug. The second
// return is false for whitelist-only slugs (live-video, event, etc.).
func RuleFor(slug string) (PostTypeRule, bool) {
	r, ok := postTypeRules[slug]
	return r, ok
}

// ValidatePostType enforces the whitelist plus per-type structural
// rules used by the Draft → ReadyForPublish gate (CON-74). A nil
// platform, or an empty post type, short-circuits to nil; callers
// rely on the upstream required-field check to surface those.
func ValidatePostType(post *models.Post, p *models.Platform, atts []models.PostAttachment) []ValidationError {
	if p == nil || post == nil || post.PlatformPostType == "" {
		return nil
	}

	if _, ok := p.PostTypes[post.PlatformPostType]; !ok {
		return []ValidationError{{
			Platform: p.ID,
			Rule:     RulePostTypeUnknown,
			Expected: strings.Join(sortedSlugs(p.PostTypes), ","),
			Actual:   post.PlatformPostType,
			Message:  fmt.Sprintf("post type %q is not supported on %s", post.PlatformPostType, p.Name),
		}}
	}

	rule, ok := postTypeRules[post.PlatformPostType]
	if !ok {
		return nil
	}

	// CON-284: a threaded post is validated per segment (count + per-segment
	// char limit + per-segment media + segment_index integrity), not as one
	// whole message. Its structure lives in ThreadSegments + the attachments'
	// segment_index, which the single-message checks below can't express — so
	// the thread path replaces them entirely.
	if post.PlatformPostType == models.PostTypeThread {
		return validateThread(post, p, atts)
	}

	var errs []ValidationError

	if rule.RequiresContent && strings.TrimSpace(post.Content) == "" {
		errs = append(errs, ValidationError{
			Platform: p.ID,
			Rule:     RuleRequiresContent,
			Expected: "non-empty content",
			Actual:   "empty",
			Message:  fmt.Sprintf("post type %q requires non-empty content", post.PlatformPostType),
		})
	}

	if len(atts) < rule.MinAttachments {
		errs = append(errs, ValidationError{
			Platform: p.ID,
			Rule:     RuleMinAttachments,
			Expected: fmt.Sprintf(">= %d", rule.MinAttachments),
			Actual:   strconv.Itoa(len(atts)),
			Message:  fmt.Sprintf("post type %q requires at least %d attachment(s); got %d", post.PlatformPostType, rule.MinAttachments, len(atts)),
		})
	}

	if rule.MaxAttachments != -1 && len(atts) > rule.MaxAttachments {
		errs = append(errs, ValidationError{
			Platform: p.ID,
			Rule:     RuleMaxAttachments,
			Expected: fmt.Sprintf("<= %d", rule.MaxAttachments),
			Actual:   strconv.Itoa(len(atts)),
			Message:  fmt.Sprintf("post type %q allows at most %d attachment(s); got %d", post.PlatformPostType, rule.MaxAttachments, len(atts)),
		})
	}

	if len(rule.AllowedKinds) > 0 {
		for i := range atts {
			kind := AttachmentKind(atts[i].MimeType)
			if !contains(rule.AllowedKinds, kind) {
				errs = append(errs, ValidationError{
					Platform:     p.ID,
					AttachmentID: atts[i].ID,
					Rule:         RuleAttachmentKind,
					Expected:     strings.Join(rule.AllowedKinds, ","),
					Actual:       kind,
					Message:      fmt.Sprintf("post type %q does not allow %s attachments", post.PlatformPostType, kindLabel(kind)),
				})
			}
		}
	}

	// CON-148: a video post type on a platform that requires a title (YouTube)
	// can't publish untitled. Only fires for video post types so image/text
	// posts are unaffected.
	if contains(rule.AllowedKinds, KindVideo) && p.VideoConstraints.RequiresVideoTitle && strings.TrimSpace(post.Title) == "" {
		errs = append(errs, ValidationError{
			Platform: p.ID,
			Rule:     RuleRequiresVideoTitle,
			Expected: "non-empty title",
			Actual:   "empty",
			Message:  fmt.Sprintf("%s requires a title for %s posts", p.Name, post.PlatformPostType),
		})
	}

	// CON-91: enforce the same char limits the composer surfaces client-side,
	// so a client bug can't slip an over-length post past the publish gate.
	// Counts are runes (code points), matching what the character counter
	// shows. A zero limit means unbounded → skip.
	if limit := p.TextConstraints.ContentLimitFor(post.PlatformPostType); limit > 0 {
		if n := utf8.RuneCountInString(post.Content); n > limit {
			errs = append(errs, ValidationError{
				Platform: p.ID,
				Rule:     RuleMaxContentChars,
				Expected: fmt.Sprintf("<= %d characters", limit),
				Actual:   strconv.Itoa(n),
				Message:  fmt.Sprintf("%s post allows up to %d characters; content is %d", p.Name, limit, n),
			})
		}
	}
	if limit := p.TextConstraints.MaxTitleChars; limit > 0 {
		if n := utf8.RuneCountInString(post.Title); n > limit {
			errs = append(errs, ValidationError{
				Platform: p.ID,
				Rule:     RuleMaxTitleChars,
				Expected: fmt.Sprintf("<= %d characters", limit),
				Actual:   strconv.Itoa(n),
				Message:  fmt.Sprintf("%s title allows up to %d characters; title is %d", p.Name, limit, n),
			})
		}
	}

	return errs
}

// MaxThreadSegments is the built-in default cap on how many messages one thread
// post may carry (CON-284 §6.5). CON-292 made it operator-controllable via
// platform_global_limits — validateThread reads the live GlobalLimits() value;
// this const is the fallback default, mirrored by
// models.DefaultPlatformGlobalLimits().MaxThreadSegments and the migration seed.
const MaxThreadSegments = 25

// validateThread runs the CON-284 per-segment publish rules for a thread post,
// replacing the whole-post content/attachment checks that can't express a
// thread's structure:
//   - segment count is 2..MaxThreadSegments (a single message is an ordinary post);
//   - each segment carries non-empty content within the platform's per-segment
//     char limit (X 280 / Threads 500 — "thread" has no per-type override, so the
//     platform default applies);
//   - every attachment names a valid segment via segment_index, and the media
//     within each segment obeys the platform's kind/count rules.
func validateThread(post *models.Post, p *models.Platform, atts []models.PostAttachment) []ValidationError {
	var errs []ValidationError
	segs := post.ThreadSegments
	n := len(segs)
	// Operator-controlled ceiling (CON-292); defaults to MaxThreadSegments.
	maxSeg := GlobalLimits().MaxThreadSegments

	switch {
	case n < 2:
		errs = append(errs, ValidationError{
			Platform: p.ID,
			Rule:     RuleThreadSegmentCount,
			Expected: ">= 2 messages",
			Actual:   strconv.Itoa(n),
			Message:  "a thread needs at least 2 messages",
		})
	case n > maxSeg:
		errs = append(errs, ValidationError{
			Platform: p.ID,
			Rule:     RuleThreadSegmentCount,
			Expected: fmt.Sprintf("<= %d messages", maxSeg),
			Actual:   strconv.Itoa(n),
			Message:  fmt.Sprintf("a thread allows at most %d messages; got %d", maxSeg, n),
		})
	}

	limit := p.TextConstraints.ContentLimitFor(post.PlatformPostType)
	for i := range segs {
		seg := i
		if strings.TrimSpace(segs[i].Content) == "" {
			errs = append(errs, ValidationError{
				Platform: p.ID,
				Rule:     RuleRequiresContent,
				Expected: "non-empty content",
				Actual:   "empty",
				Message:  fmt.Sprintf("thread message %d is empty", i+1),
				Segment:  &seg,
			})
			continue
		}
		if limit > 0 {
			if c := utf8.RuneCountInString(segs[i].Content); c > limit {
				errs = append(errs, ValidationError{
					Platform: p.ID,
					Rule:     RuleMaxContentChars,
					Expected: fmt.Sprintf("<= %d characters", limit),
					Actual:   strconv.Itoa(c),
					Message:  fmt.Sprintf("thread message %d allows up to %d characters; content is %d", i+1, limit, c),
					Segment:  &seg,
				})
			}
		}
	}

	errs = append(errs, validateThreadAttachments(p, atts, n)...)
	return errs
}

// validateThreadAttachments checks segment_index integrity on every attachment
// of a thread post, then runs the platform's media rules within each segment
// (reusing ValidatePostAttachments scoped to the segment's media). Iterates
// segments in order so the returned errors are stable.
func validateThreadAttachments(p *models.Platform, atts []models.PostAttachment, segCount int) []ValidationError {
	var errs []ValidationError
	bySegment := map[int][]models.PostAttachment{}
	for i := range atts {
		att := atts[i]
		if att.SegmentIndex == nil {
			errs = append(errs, ValidationError{
				Platform:     p.ID,
				AttachmentID: att.ID,
				Rule:         RuleThreadSegmentIndex,
				Expected:     "a segment_index",
				Actual:       "null",
				Message:      "attachment on a thread post must name its segment (segment_index)",
			})
			continue
		}
		idx := *att.SegmentIndex
		if idx < 0 || idx >= segCount {
			seg := idx
			errs = append(errs, ValidationError{
				Platform:     p.ID,
				AttachmentID: att.ID,
				Rule:         RuleThreadSegmentIndex,
				Expected:     fmt.Sprintf("segment_index in [0, %d]", max(segCount-1, 0)),
				Actual:       strconv.Itoa(idx),
				Message:      fmt.Sprintf("attachment segment_index %d is out of range for a %d-message thread", idx, segCount),
				Segment:      &seg,
			})
			continue
		}
		bySegment[idx] = append(bySegment[idx], att)
	}
	for idx := 0; idx < segCount; idx++ {
		segAtts := bySegment[idx]
		if len(segAtts) == 0 {
			continue
		}
		seg := idx
		for _, e := range ValidatePostAttachments(segAtts, p) {
			e.Segment = &seg
			errs = append(errs, e)
		}
	}
	return errs
}

// ValidatePublishReadiness is the single publish-gate entry point shared by the
// REST create/update paths, the schedule service, and the assistant's readiness
// advisor. For an ordinary post it runs the whole-post media rules
// (ValidateForPublish) plus the per-post-type rules (ValidatePostType); for a
// thread post (CON-284) it runs ONLY the per-segment gate, because the
// whole-post media caps would wrongly count every segment's media against one
// per-post limit. Returns the per-platform error map (keyed by platform.ID);
// empty when the post passes or platform is nil.
func ValidatePublishReadiness(post *models.Post, platform *models.Platform, atts []models.PostAttachment) map[string][]ValidationError {
	if platform == nil {
		return map[string][]ValidationError{}
	}
	if post != nil && post.IsThread() {
		out := map[string][]ValidationError{}
		if errs := ValidatePostType(post, platform, atts); len(errs) > 0 {
			out[platform.ID] = errs
		}
		return out
	}
	out := ValidateForPublish(atts, []*models.Platform{platform})
	if typeErrs := ValidatePostType(post, platform, atts); len(typeErrs) > 0 {
		out[platform.ID] = append(out[platform.ID], typeErrs...)
	}
	return out
}

func sortedSlugs(m models.PostTypeMap) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ResolvedPostTypeRule is the JSON-friendly projection of a
// PostTypeRule with platform-specific caps already applied. A nil
// MaxAttachments means "unbounded by this rule" — the UI should treat
// it as no upper limit. AllowedKinds is always a non-nil slice so
// clients can iterate without a nil check.
type ResolvedPostTypeRule struct {
	RequiresContent bool     `json:"requires_content"`
	AllowedKinds    []string `json:"allowed_kinds"`
	MinAttachments  int      `json:"min_attachments"`
	MaxAttachments  *int     `json:"max_attachments"`
	// MaxContentChars is the body-text ceiling for this post type, resolved
	// from the platform's TextConstraints (per-post-type override, else the
	// platform default). nil means unbounded — the UI shows no counter cap.
	// Counts are Unicode code points, matching the server-side check (CON-91).
	MaxContentChars *int `json:"max_content_chars"`
	// Segmented marks a post type whose composer authors an ordered list of
	// messages rather than one body (CON-284: the "thread" type). MaxContentChars
	// then carries the PER-SEGMENT limit (X 280 / Threads 500), and the UI renders
	// the segmented composer with a counter per message. false for every other type.
	Segmented bool `json:"segmented"`
}

// PostTypeRuleView is one entry in the platform-scoped rules response.
// Rule is nil for whitelist-only slugs (e.g. live-video, event) — the
// platform accepts them but Ogen enforces no structural rules.
type PostTypeRuleView struct {
	Slug          string                `json:"slug"`
	Label         string                `json:"label"`
	WhitelistOnly bool                  `json:"whitelist_only"`
	Rule          *ResolvedPostTypeRule `json:"rule"`
}

// ResolvePostTypeRules returns the per-slug rules for a platform with
// MaxAttachments sentinels resolved against the platform's
// ImageConstraints / PDFConstraints caps. The slice is sorted by slug
// for stable client rendering.
func ResolvePostTypeRules(p *models.Platform) []PostTypeRuleView {
	if p == nil {
		return []PostTypeRuleView{}
	}
	slugs := sortedSlugs(p.PostTypes)
	out := make([]PostTypeRuleView, 0, len(slugs))
	for _, slug := range slugs {
		view := PostTypeRuleView{
			Slug:  slug,
			Label: p.PostTypes[slug],
		}
		rule, ok := postTypeRules[slug]
		if !ok {
			view.WhitelistOnly = true
			out = append(out, view)
			continue
		}
		view.Rule = &ResolvedPostTypeRule{
			RequiresContent: rule.RequiresContent,
			AllowedKinds:    ensureKinds(rule.AllowedKinds),
			MinAttachments:  rule.MinAttachments,
			MaxAttachments:  resolveMaxAttachments(rule, p),
			MaxContentChars: resolveMaxContentChars(slug, p),
			Segmented:       slug == models.PostTypeThread,
		}
		out = append(out, view)
	}
	return out
}

func ensureKinds(k []string) []string {
	if k == nil {
		return []string{}
	}
	return append([]string(nil), k...)
}

func resolveMaxAttachments(r PostTypeRule, p *models.Platform) *int {
	if r.MaxAttachments != -1 {
		v := r.MaxAttachments
		return &v
	}
	if contains(r.AllowedKinds, KindImage) && p.ImageConstraints.MaxAttachmentsPerPost > 0 {
		v := p.ImageConstraints.MaxAttachmentsPerPost
		return &v
	}
	if contains(r.AllowedKinds, KindPDF) && p.PDFConstraints.MaxAttachmentsPerPost > 0 {
		v := p.PDFConstraints.MaxAttachmentsPerPost
		return &v
	}
	return nil
}

// resolveMaxContentChars projects the platform's text limit for one slug
// into the nil-means-unbounded pointer shape the client expects.
func resolveMaxContentChars(slug string, p *models.Platform) *int {
	if limit := p.TextConstraints.ContentLimitFor(slug); limit > 0 {
		return &limit
	}
	return nil
}

func kindLabel(kind string) string {
	if kind == "" {
		return "unknown"
	}
	return kind
}
