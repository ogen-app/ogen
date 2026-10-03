// Package platforms holds the per-platform attachment validator used
// by both the soft pre-check that surfaces warnings on attachment
// mutations and the hard validation that runs immediately before a
// Zernio publish call.
//
// Constraint values themselves live on the `platforms` row — see
// models.Platform.ImageConstraints. This package consumes those rules;
// it does not own them.
package platforms

import "strings"

// Validation rule identifiers. They are stable strings so the frontend
// and audit log can switch on them without reading the human message.
const (
	RuleMaxFileSize         = "max_file_size_bytes"
	RuleAllowedFormat       = "allowed_formats"
	RuleAnimatedGIF         = "animated_gif_supported"
	RuleMaxAttachmentsCount = "max_attachments_per_post"
	RuleMaxPages            = "max_pages"             // PDF page-count cap
	RuleAttachmentMix       = "attachment_kind_mix"   // Image+PDF on the same post
	RulePDFNotSupported     = "pdf_not_supported"     // Platform has no PDF rules
	RuleMaxDuration         = "max_duration_seconds"  // Video length ceiling
	RuleMinDuration         = "min_duration_seconds"  // Video length floor (Reels/Shorts)
	RuleMaxResolution       = "max_resolution"        // Video frame-size cap
	RuleAspectRatio         = "allowed_aspect_ratios" // Video aspect ratio
	RuleVideoNotSupported   = "video_not_supported"   // Platform has no video rules
	RulePostTypeUnknown     = "post_type_unknown"     // Slug not in Platform.PostTypes
	RuleRequiresContent     = "requires_content"      // Post type needs non-empty content
	RuleMinAttachments      = "min_attachments"       // Too few attachments for the type
	RuleMaxAttachments      = "max_attachments"       // Too many attachments for the type
	RuleAttachmentKind      = "attachment_kind"       // Wrong attachment kind for the type
	RuleRequiresVideoTitle  = "requires_video_title"  // Platform needs a title for video (YouTube)
	RuleRequiresLink        = "requires_link"         // Post type needs a link (cta_url)
	RuleInvalidLink         = "invalid_link"          // Link is not an absolute http(s) URL
	RuleMaxContentChars     = "max_content_chars"     // Body text exceeds the platform/post-type cap
	RuleMaxTitleChars       = "max_title_chars"       // Title exceeds the platform cap
	RuleThreadSegmentCount  = "thread_segment_count"  // A thread needs 2..MaxThreadSegments messages
	RuleThreadSegmentIndex  = "thread_segment_index"  // Attachment segment_index missing / out of range / set off-thread
)

// Attachment kinds, returned by AttachmentKind. PDF, image, and video
// attachments take different validation paths.
const (
	KindImage = "image"
	KindPDF   = "pdf"
	KindVideo = "video"
)

// AttachmentKind returns the kind label for the given MIME type.
// Unknown MIMEs return an empty string; the validator treats that as
// "unclassified" and skips kind-specific rules.
func AttachmentKind(mime string) string {
	switch mime {
	case "image/jpeg", "image/png", "image/webp", "image/gif":
		return KindImage
	case "application/pdf":
		return KindPDF
	}
	if strings.HasPrefix(mime, "video/") {
		return KindVideo
	}
	return ""
}
