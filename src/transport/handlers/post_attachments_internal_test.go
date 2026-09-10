package handlers

import (
	"strings"
	"testing"
)

func TestNormalizeAltText(t *testing.T) {
	// Trims surrounding whitespace.
	if got, err := normalizeAltText("  hi  "); err != nil || got != "hi" {
		t.Fatalf("trim: got %q err %v, want %q", got, err, "hi")
	}

	// The cap is in characters (runes), not bytes: exactly maxAltTextLen()
	// multibyte runes must be accepted even though it far exceeds
	// maxAltTextLen() bytes (byte-counting would wrongly reject this). The cap
	// is now operator config (CON-292); absent InitGlobalLimits it is the
	// built-in default.
	limit := maxAltTextLen()
	atLimit := strings.Repeat("é", limit) // 2 bytes per rune
	if _, err := normalizeAltText(atLimit); err != nil {
		t.Errorf("exactly maxAltTextLen() runes should be accepted, got %v", err)
	}

	// One rune over the cap is rejected.
	if _, err := normalizeAltText(strings.Repeat("é", limit+1)); err == nil {
		t.Errorf("maxAltTextLen()+1 runes should be rejected")
	}
}
