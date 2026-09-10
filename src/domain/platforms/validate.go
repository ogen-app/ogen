package platforms

import (
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/ogen-app/ogen/src/domain/models"
)

// ValidationError describes a single rule failure for one
// (attachment, platform) pair, exactly per CON-73 §2.4.
type ValidationError struct {
	Platform     string `json:"platform"`
	AttachmentID string `json:"attachment_id"`
	Rule         string `json:"rule"`
	Expected     string `json:"expected"`
	Actual       string `json:"actual"`
	Message      string `json:"message"`
	// Segment names the 0-based thread message a failure belongs to (CON-284),
	// so the composer can highlight the offending message. nil for whole-post
	// failures and every non-thread post.
	Segment *int `json:"segment,omitempty"`
}

// ValidateAttachment runs all rules carried by the platform row for
// one attachment. Returns an empty slice when the attachment passes
// every rule. A nil platform — e.g. a draft post that has not picked
// one yet — short-circuits to nil; callers treat that as "nothing to
// validate yet".
func ValidateAttachment(att *models.PostAttachment, p *models.Platform) []ValidationError {
	if p == nil {
		return nil
	}
	switch AttachmentKind(att.MimeType) {
	case KindImage:
		return validateImageAttachment(att, p)
	case KindPDF:
		return validatePDFAttachment(att, p)
	case KindVideo:
		return validateVideoAttachment(att, p)
	}
	return nil
}

func validateImageAttachment(att *models.PostAttachment, p *models.Platform) []ValidationError {
	if p.ImageConstraints.IsZero() {
		return nil
	}
	c := p.ImageConstraints
	var errs []ValidationError
	if att.SizeBytes > c.MaxFileSizeBytes {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RuleMaxFileSize,
			Expected:     fmt.Sprintf("<= %d", c.MaxFileSizeBytes),
			Actual:       fmt.Sprintf("%d", att.SizeBytes),
			Message:      fmt.Sprintf("file is %d bytes; platform allows up to %d", att.SizeBytes, c.MaxFileSizeBytes),
		})
	}
	format := mimeToFormat(att.MimeType)
	if !slices.Contains(c.AllowedFormats, format) {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RuleAllowedFormat,
			Expected:     strings.Join(c.AllowedFormats, ","),
			Actual:       format,
			Message:      fmt.Sprintf("format %q is not allowed for this platform", format),
		})
	}
	if att.IsAnimated && !c.AnimatedGIFSupported {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RuleAnimatedGIF,
			Expected:     "static",
			Actual:       "animated",
			Message:      "animated GIFs are not supported on this platform",
		})
	}
	return errs
}

func validatePDFAttachment(att *models.PostAttachment, p *models.Platform) []ValidationError {
	// Platforms that haven't opted into PDFs surface a single explicit
	// "not supported" warning rather than a cascade of size/format
	// failures from a zero-valued rule set.
	if p.PDFConstraints.IsZero() {
		return []ValidationError{{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RulePDFNotSupported,
			Expected:     "no PDF attachments",
			Actual:       "pdf",
			Message:      fmt.Sprintf("PDF attachments are not supported on %s", p.Name),
		}}
	}
	c := p.PDFConstraints
	var errs []ValidationError
	if att.SizeBytes > c.MaxFileSizeBytes {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RuleMaxFileSize,
			Expected:     fmt.Sprintf("<= %d", c.MaxFileSizeBytes),
			Actual:       fmt.Sprintf("%d", att.SizeBytes),
			Message:      fmt.Sprintf("file is %d bytes; platform allows up to %d", att.SizeBytes, c.MaxFileSizeBytes),
		})
	}
	format := mimeToFormat(att.MimeType)
	if !slices.Contains(c.AllowedFormats, format) {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RuleAllowedFormat,
			Expected:     strings.Join(c.AllowedFormats, ","),
			Actual:       format,
			Message:      fmt.Sprintf("format %q is not allowed for this platform", format),
		})
	}
	if c.MaxPages > 0 && att.PageCount > c.MaxPages {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RuleMaxPages,
			Expected:     fmt.Sprintf("<= %d", c.MaxPages),
			Actual:       fmt.Sprintf("%d", att.PageCount),
			Message:      fmt.Sprintf("PDF has %d pages; platform allows up to %d", att.PageCount, c.MaxPages),
		})
	}
	return errs
}

// validateVideoAttachment runs the per-platform video rule set (CON-148).
// A platform with no video rules surfaces a single explicit "not
// supported" warning rather than a cascade of failures from a zero-valued
// rule set (mirrors the PDF branch). Duration / resolution / aspect checks
// only fire when video-service actually probed those fields — an unprobed
// upload (graceful degradation) validates on size/format alone.
func validateVideoAttachment(att *models.PostAttachment, p *models.Platform) []ValidationError {
	if p.VideoConstraints.IsZero() {
		return []ValidationError{{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RuleVideoNotSupported,
			Expected:     "no video attachments",
			Actual:       "video",
			Message:      fmt.Sprintf("video attachments are not supported on %s", p.Name),
		}}
	}
	c := p.VideoConstraints
	var errs []ValidationError

	if c.MaxFileSizeBytes > 0 && att.SizeBytes > c.MaxFileSizeBytes {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RuleMaxFileSize,
			Expected:     fmt.Sprintf("<= %d", c.MaxFileSizeBytes),
			Actual:       fmt.Sprintf("%d", att.SizeBytes),
			Message:      fmt.Sprintf("file is %d bytes; platform allows up to %d", att.SizeBytes, c.MaxFileSizeBytes),
		})
	}

	format := mimeToFormat(att.MimeType)
	if len(c.AllowedFormats) > 0 && !slices.Contains(c.AllowedFormats, format) {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: att.ID,
			Rule:         RuleAllowedFormat,
			Expected:     strings.Join(c.AllowedFormats, ","),
			Actual:       format,
			Message:      fmt.Sprintf("format %q is not allowed for this platform", format),
		})
	}

	// Duration is only known when video-service probed the file.
	if att.DurationMs > 0 {
		durationSec := int(att.DurationMs / 1000)
		if c.MaxDurationSeconds > 0 && durationSec > c.MaxDurationSeconds {
			errs = append(errs, ValidationError{
				Platform:     p.ID,
				AttachmentID: att.ID,
				Rule:         RuleMaxDuration,
				Expected:     fmt.Sprintf("<= %ds", c.MaxDurationSeconds),
				Actual:       fmt.Sprintf("%ds", durationSec),
				Message:      fmt.Sprintf("video is %ds long; platform allows up to %ds", durationSec, c.MaxDurationSeconds),
			})
		}
		if c.MinDurationSeconds > 0 && durationSec < c.MinDurationSeconds {
			errs = append(errs, ValidationError{
				Platform:     p.ID,
				AttachmentID: att.ID,
				Rule:         RuleMinDuration,
				Expected:     fmt.Sprintf(">= %ds", c.MinDurationSeconds),
				Actual:       fmt.Sprintf("%ds", durationSec),
				Message:      fmt.Sprintf("video is %ds long; platform requires at least %ds", durationSec, c.MinDurationSeconds),
			})
		}
	}

	// Resolution / aspect are only known when the frame size was probed.
	if att.Width > 0 && att.Height > 0 {
		if (c.MaxWidth > 0 && att.Width > c.MaxWidth) || (c.MaxHeight > 0 && att.Height > c.MaxHeight) {
			errs = append(errs, ValidationError{
				Platform:     p.ID,
				AttachmentID: att.ID,
				Rule:         RuleMaxResolution,
				Expected:     fmt.Sprintf("<= %sx%s", dimLabel(c.MaxWidth), dimLabel(c.MaxHeight)),
				Actual:       fmt.Sprintf("%dx%d", att.Width, att.Height),
				Message:      fmt.Sprintf("video is %dx%d; platform allows up to %sx%s", att.Width, att.Height, dimLabel(c.MaxWidth), dimLabel(c.MaxHeight)),
			})
		}
		if len(c.AllowedAspectRatios) > 0 && !aspectRatioAllowed(att.Width, att.Height, c.AllowedAspectRatios) {
			errs = append(errs, ValidationError{
				Platform:     p.ID,
				AttachmentID: att.ID,
				Rule:         RuleAspectRatio,
				Expected:     strings.Join(c.AllowedAspectRatios, ","),
				Actual:       fmt.Sprintf("%dx%d", att.Width, att.Height),
				Message:      fmt.Sprintf("video aspect ratio %dx%d is not allowed on this platform", att.Width, att.Height),
			})
		}
	}

	return errs
}

// ValidatePostAttachments runs rules for every attachment plus the
// post-level count rules (per kind) and the mixed-kind warning. A nil
// platform returns nil. A platform that defines no image, PDF, or video
// rules only surfaces explicit not-supported warnings.
func ValidatePostAttachments(atts []models.PostAttachment, p *models.Platform) []ValidationError {
	if p == nil {
		return nil
	}
	hasImageRules := !p.ImageConstraints.IsZero()
	hasPDFRules := !p.PDFConstraints.IsZero()
	hasVideoRules := !p.VideoConstraints.IsZero()
	if !hasImageRules && !hasPDFRules && !hasVideoRules {
		// No structured rules at all: still surface the explicit
		// not-supported warning for any PDF/video the post carries —
		// image-only platforms with zero image rules are out of scope
		// and stay silent.
		var errs []ValidationError
		for i := range atts {
			switch AttachmentKind(atts[i].MimeType) {
			case KindPDF:
				errs = append(errs, validatePDFAttachment(&atts[i], p)...)
			case KindVideo:
				errs = append(errs, validateVideoAttachment(&atts[i], p)...)
			}
		}
		return errs
	}

	var errs []ValidationError

	images, pdfs, videos := splitByKind(atts)

	if len(images) > 0 && len(pdfs) > 0 {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: "",
			Rule:         RuleAttachmentMix,
			Expected:     "images or PDF, not both",
			Actual:       fmt.Sprintf("%d image(s) + %d pdf(s)", len(images), len(pdfs)),
			Message:      "mixing image and PDF attachments on a single post is not supported",
		})
	}
	// A video mixed with a PDF is never a supported combination. Image +
	// video is deliberately *not* flagged here — the `story` post type
	// allows it, and the post-type rule owns that decision (CON-148 §10).
	if len(videos) > 0 && len(pdfs) > 0 {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: "",
			Rule:         RuleAttachmentMix,
			Expected:     "video or PDF, not both",
			Actual:       fmt.Sprintf("%d video(s) + %d pdf(s)", len(videos), len(pdfs)),
			Message:      "mixing video and PDF attachments on a single post is not supported",
		})
	}
	if hasImageRules && len(images) > p.ImageConstraints.MaxAttachmentsPerPost {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: "",
			Rule:         RuleMaxAttachmentsCount,
			Expected:     fmt.Sprintf("<= %d images", p.ImageConstraints.MaxAttachmentsPerPost),
			Actual:       fmt.Sprintf("%d images", len(images)),
			Message:      fmt.Sprintf("post has %d image attachments; platform allows up to %d", len(images), p.ImageConstraints.MaxAttachmentsPerPost),
		})
	}
	if hasPDFRules && len(pdfs) > p.PDFConstraints.MaxAttachmentsPerPost {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: "",
			Rule:         RuleMaxAttachmentsCount,
			Expected:     fmt.Sprintf("<= %d pdf(s)", p.PDFConstraints.MaxAttachmentsPerPost),
			Actual:       fmt.Sprintf("%d pdf(s)", len(pdfs)),
			Message:      fmt.Sprintf("post has %d PDF attachments; platform allows up to %d", len(pdfs), p.PDFConstraints.MaxAttachmentsPerPost),
		})
	}
	if hasVideoRules && len(videos) > p.VideoConstraints.MaxAttachmentsPerPost {
		errs = append(errs, ValidationError{
			Platform:     p.ID,
			AttachmentID: "",
			Rule:         RuleMaxAttachmentsCount,
			Expected:     fmt.Sprintf("<= %d video(s)", p.VideoConstraints.MaxAttachmentsPerPost),
			Actual:       fmt.Sprintf("%d video(s)", len(videos)),
			Message:      fmt.Sprintf("post has %d video attachments; platform allows up to %d", len(videos), p.VideoConstraints.MaxAttachmentsPerPost),
		})
	}

	for i := range atts {
		errs = append(errs, ValidateAttachment(&atts[i], p)...)
	}
	return errs
}

// ValidateForPublish runs the publish-time hard check across every
// target platform. The future Zernio publish path uses the returned
// map to skip platforms whose value is non-empty while still
// proceeding with platforms whose value is empty (CON-73 §2.4).
// Platforms with neither image nor PDF rules are absent from the
// returned map.
func ValidateForPublish(atts []models.PostAttachment, ps []*models.Platform) map[string][]ValidationError {
	out := map[string][]ValidationError{}
	for _, p := range ps {
		if p == nil {
			continue
		}
		if p.ImageConstraints.IsZero() && p.PDFConstraints.IsZero() && p.VideoConstraints.IsZero() {
			continue
		}
		out[p.ID] = ValidatePostAttachments(atts, p)
	}
	return out
}

func splitByKind(atts []models.PostAttachment) (images, pdfs, videos []models.PostAttachment) {
	for i := range atts {
		switch AttachmentKind(atts[i].MimeType) {
		case KindImage:
			images = append(images, atts[i])
		case KindPDF:
			pdfs = append(pdfs, atts[i])
		case KindVideo:
			videos = append(videos, atts[i])
		}
	}
	return
}

func mimeToFormat(mime string) string {
	switch mime {
	case "image/jpeg":
		return "jpeg"
	case "image/png":
		return "png"
	case "image/webp":
		return "webp"
	case "image/gif":
		return "gif"
	case "application/pdf":
		return "pdf"
	case "video/mp4":
		return "mp4"
	case "video/quicktime":
		return "mov"
	case "video/webm":
		return "webm"
	case "video/x-msvideo":
		return "avi"
	case "video/x-matroska":
		return "mkv"
	case "video/mpeg":
		return "mpeg"
	case "video/3gpp":
		return "3gp"
	}
	// Fall back to the subtype for any other video/* container so an
	// unmapped-but-declared format is compared by its short name.
	if strings.HasPrefix(mime, "video/") {
		return strings.TrimPrefix(mime, "video/")
	}
	return mime
}

// aspectRatioAllowed reports whether a w×h frame matches one of the
// allowed "a:b" ratios within a small tolerance. The tolerance absorbs
// real-world encodings that are a few pixels off an exact ratio (e.g.
// 1920×1088 for nominal 16:9) so validation doesn't false-positive on
// otherwise-conformant video.
func aspectRatioAllowed(w, h int, allowed []string) bool {
	if w <= 0 || h <= 0 {
		return true
	}
	const tolerance = 0.02
	actual := float64(w) / float64(h)
	for _, r := range allowed {
		parts := strings.SplitN(r, ":", 2)
		if len(parts) != 2 {
			continue
		}
		a, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		b, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || a <= 0 || b <= 0 {
			continue
		}
		want := float64(a) / float64(b)
		if math.Abs(actual-want)/want <= tolerance {
			return true
		}
	}
	return false
}

// dimLabel renders one resolution-axis bound. A zero value means the axis is
// unbounded, so it shows as "∞" instead of a misleading "0".
func dimLabel(v int) string {
	if v == 0 {
		return "∞"
	}
	return strconv.Itoa(v)
}
