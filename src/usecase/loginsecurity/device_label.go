package loginsecurity

import (
	"strings"

	"github.com/mssola/useragent"
)

const (
	unknownBrowser = "Unknown browser"
	// rawUALabelMax bounds the fallback label taken verbatim from the header.
	rawUALabelMax = 120
)

// DeviceLabel turns a User-Agent header into the "<Browser> on <OS>" line the
// alert email and the secure-account page show, e.g. "Firefox on Windows". A
// header the parser can't make sense of falls back to its first 120 characters.
func DeviceLabel(uaHeader string) string {
	uaHeader = strings.TrimSpace(uaHeader)
	if uaHeader == "" {
		return unknownBrowser
	}
	ua := useragent.New(uaHeader)
	browser, _ := ua.Browser()
	os := osLabel(ua.Platform(), ua.OS())
	if browser != "" && os != "" {
		return browser + " on " + os
	}
	return truncateRunes(uaHeader, rawUALabelMax)
}

// osLabel maps the parser's raw platform/OS strings ("Intel Mac OS X 10_15_7",
// "CPU iPhone OS 17_0 like Mac OS X") to a product name.
func osLabel(platform, os string) string {
	switch {
	case platform == "iPad" || strings.Contains(os, "iPad"):
		return "iPadOS"
	case platform == "iPhone" || strings.Contains(os, "iPhone"):
		return "iOS"
	case strings.Contains(os, "Android"):
		return "Android"
	case strings.Contains(os, "CrOS"):
		return "ChromeOS"
	case strings.Contains(os, "Mac OS X"), platform == "Macintosh":
		return "macOS"
	case strings.HasPrefix(os, "Windows"), platform == "Windows":
		return "Windows"
	case strings.Contains(os, "Linux"), platform == "Linux", platform == "X11":
		return "Linux"
	}
	return ""
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
