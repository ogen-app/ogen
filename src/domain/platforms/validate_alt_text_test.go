package platforms

import (
	"strings"
	"testing"

	"github.com/ogen-app/ogen/src/domain/models"
)

func altTextPlatform(altCap int) *models.Platform {
	return &models.Platform{
		ID:   "imgplat",
		Name: "ImgPlat",
		ImageConstraints: models.ImageConstraints{
			MaxFileSizeBytes:      1 << 20,
			AllowedFormats:        []string{"png"},
			MaxAttachmentsPerPost: 4,
			MaxAltTextChars:       altCap,
		},
	}
}

func TestValidateImageAttachment_AltTextLength(t *testing.T) {
	cases := []struct {
		name   string
		altCap int
		alt    string
		want   bool
	}{
		{name: "at the cap", altCap: 5, alt: "abcde"},
		{name: "over the cap", altCap: 5, alt: "abcdef", want: true},
		{name: "counted in characters, not bytes", altCap: 5, alt: "ééééé"},
		{name: "no cap means no alt text, not unlimited length", altCap: 0, alt: strings.Repeat("a", 5000)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			att := &models.PostAttachment{ID: "a1", MimeType: "image/png", SizeBytes: 10, AltText: tc.alt}
			errs := ValidateAttachment(att, altTextPlatform(tc.altCap))
			if got := hasRule(errs, RuleMaxAltTextChars); got != tc.want {
				t.Fatalf("max_alt_text_chars flagged = %v, want %v (errs %+v)", got, tc.want, errs)
			}
		})
	}
}

// A platform that sets only an alt-text cap must not have its zero-valued file
// rules (size 0, no formats, 0 attachments) applied to every image.
func TestValidatePostAttachments_AltTextOnlyPlatform(t *testing.T) {
	p := &models.Platform{ID: "altonly", Name: "AltOnly", ImageConstraints: models.ImageConstraints{MaxAltTextChars: 5}}
	atts := []models.PostAttachment{
		{ID: "a1", MimeType: "image/png", SizeBytes: 10, AltText: "short"},
		{ID: "a2", MimeType: "image/png", SizeBytes: 10, AltText: "too long"},
	}
	errs := ValidatePostAttachments(atts, p)
	if len(errs) != 1 || errs[0].Rule != RuleMaxAltTextChars || errs[0].AttachmentID != "a2" {
		t.Fatalf("want only a2's max_alt_text_chars error, got %+v", errs)
	}
}

func TestValidateImageAttachment_AltTextMessageNamesPlatform(t *testing.T) {
	att := &models.PostAttachment{ID: "a1", MimeType: "image/png", SizeBytes: 10, AltText: "abcdef"}
	for _, e := range ValidateAttachment(att, altTextPlatform(5)) {
		if e.Rule != RuleMaxAltTextChars {
			continue
		}
		if e.Expected != "<= 5" || e.Actual != "6" || !strings.Contains(e.Message, "ImgPlat allows up to 5") {
			t.Fatalf("unexpected error %+v", e)
		}
		return
	}
	t.Fatal("no max_alt_text_chars error")
}
