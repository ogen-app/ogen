package platforms

import (
	"errors"
	"net/url"
	"strings"
	"unicode/utf8"

	"github.com/ogen-app/ogen/src/domain/models"
)

// MaxLinkLen caps a post's link. Longer URLs are rejected by browsers and
// most crawlers, so a card would never render for them.
const MaxLinkLen = 2048

// ValidateLink reports whether link is an absolute http(s) URL with a
// dotted host — the shape a network can fetch to build a preview card. It
// does not resolve or fetch anything.
func ValidateLink(link string) error {
	if len(link) > MaxLinkLen {
		return errors.New("longer than 2048 characters")
	}
	if strings.ContainsAny(link, " \t\r\n") {
		return errors.New("contains whitespace")
	}
	u, err := url.Parse(link)
	if err != nil {
		return errors.New("cannot be parsed")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return errors.New("must start with http:// or https://")
	}
	if u.User != nil {
		return errors.New("must not carry credentials")
	}
	host := u.Hostname()
	if !strings.Contains(host, ".") || strings.HasPrefix(host, ".") || strings.HasSuffix(host, ".") {
		return errors.New("has no valid host")
	}
	return nil
}

// OutboundText is the message a single (non-thread) post publishes: the
// flattened body plus, for a link post, its URL. The submit worker sends this
// string and the char-limit check counts it, so both agree.
func OutboundText(post *models.Post) string {
	return post.AppendLink(FlattenSocialText(post.Content))
}

// OutboundLen counts OutboundText in Unicode code points.
func OutboundLen(post *models.Post) int {
	return utf8.RuneCountInString(OutboundText(post))
}
